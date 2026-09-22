package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"

	"github.com/wcpe/jrp/apps/jrps/internal/apply"
	"github.com/wcpe/jrp/apps/jrps/internal/store"
	"github.com/wcpe/jrp/core"
	"github.com/wcpe/jrp/core/server"
)

// 数据面控制监听的默认端口；desired 文档未显式指定时使用同一默认值。
const dataPlaneListenPort = store.DefaultControlListenPort

// storeCredentials 从 jrps 数据库组装数据面凭证集合（apply.CredentialProvider）。
//
// 边界说明（重要）：客户端 token 在 jrps 中只存 SHA-256 摘要（FR-07），明文不可
// 恢复；而 Core 当前版本的 wire v1 登录按明文逐字比对（FR-25 垂直切片的最小
// 实现）。两者之间没有可用的桥——因此本 Provider 在 P1 返回凭证的**占位集合**：
// 只含客户端标识与占位 token，用于让快照通过构建器的完整性校验并保留 ClientID
// 与绑定的归属关系。真实数据面鉴权（以摘要验证登录材料，或经宿主回调注入鉴权）
// 属 FR-03 兼容登录链与 FR-08 交付范围，届时凭此处的接口位替换实现，编排层不变。
//
// 该取舍已按构建器规格 §6 的登记（配置值允许持有明文、宿主侧真源负责脱敏）
// 记录在 FR-10 实施说明中；占位 token 不属于任何真实秘密，日志与错误路径
// 经统一脱敏辅助处理。
type storeCredentials struct {
	store *store.Store
}

// DataPlaneCredentials 返回参与数据面登录的凭证占位集合。
//
// 只纳入 enrollment 状态为 active 的客户端：pending 尚未兑换凭据、revoked 是
// 终态，两者都不应出现在数据面的凭证集合里。
func (provider storeCredentials) DataPlaneCredentials(ctx context.Context) ([]core.ClientCredential, error) {
	var credentials []core.ClientCredential
	err := provider.store.View(ctx, func(tx *store.Tx) error {
		clients, err := tx.Clients()
		if err != nil {
			return err
		}
		credentials = make([]core.ClientCredential, 0, len(clients))
		for _, client := range clients {
			if client.EnrollmentState != store.EnrollmentStateActive {
				continue
			}
			credentials = append(credentials, core.ClientCredential{
				ClientID: client.ID,
				Token:    dataPlanePlaceholderToken(client.ID),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return credentials, nil
}

// dataPlanePlaceholderToken 生成客户端的占位 token。
//
// 它由客户端标识派生、不含任何真实秘密：登录校验按明文比对时占位集合无法
// 通过真实客户端的鉴权，这正好是当前边界的诚实表达——数据面接入尚未交付
// （FR-03/FR-08），而不是假装鉴权可用。
func dataPlanePlaceholderToken(clientID string) string {
	return "jrp-placeholder:" + clientID
}

// startEngine 装配 Core 服务端引擎：构造、注入控制监听器并启动。
//
// 控制监听器由宿主创建并注入（Core 的所有权规则）：启动失败时监听器归宿主，
// 这里统一关闭并返回错误。配置用最小合法集合——真实配置由启动恢复以 desired
// 为输入走完整四阶段进入。
func startEngine(ctx context.Context, logger *slog.Logger) (*server.Engine, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", dataPlaneListenPort))
	if err != nil {
		return nil, fmt.Errorf("控制监听端口 %d 被占用：%w", dataPlaneListenPort, err)
	}
	initialConfig, err := core.NewServerConfig(
		core.WithListen(core.BindEndpoint{
			Address:   netip.AddrPortFrom(netip.IPv4Unspecified(), dataPlaneListenPort),
			Transport: core.TransportTCP,
		}),
		core.WithWire(core.WireV1),
		core.WithClientCredential(core.ClientCredential{ClientID: "bootstrap", Token: "bootstrap"}),
	)
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("构造引擎初始配置失败：%w", err)
	}
	engine := server.New(initialConfig, server.WithListener(listener))
	if err := engine.Start(ctx); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("引擎启动失败：%w", err)
	}
	return engine, nil
}

// assembleApplyService 装配配置应用编排服务并执行启动恢复。
//
// 恢复以 SQLite 中的 desired 为输入重新走完整四阶段：publish 成功后 active
// 落库记录推进，运行态由 Core 内存持有（ADR-0012）。desired 尚无版本时恢复
// 直接跳过，不产生任何应用记录。
//
// 同时启动 FR-12 的事件适配器：订阅 Core 事件通道并把事件映射为运行日志，
// 溢出时按 ResyncRequired 记录 WARN。适配器随根 ctx 取消退出（与引擎、HTTP
// 服务同一信号源），不额外占用优雅关闭时序。
func assembleApplyService(ctx context.Context, database *store.Store, engine *server.Engine, logger *slog.Logger) (*apply.Service, error) {
	service := apply.New(database, storeCredentials{store: database}, engine, logger)
	apply.StartEventAdapter(ctx, engine, database, logger)
	if err := database.Recover(ctx, service.RecoverApplier(store.ActorAdmin("server")), store.ActorAdmin("server")); err != nil {
		return nil, err
	}
	return service, nil
}

// shutdownEngine 停止 Core 引擎；幂等。
func shutdownEngine(engine *server.Engine, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := engine.Shutdown(ctx); err != nil {
		if !errors.Is(err, context.DeadlineExceeded) {
			logger.Warn("引擎停止异常", "错误", err)
			return
		}
		logger.Warn("引擎停止超时，已强制释放剩余资源")
	}
}
