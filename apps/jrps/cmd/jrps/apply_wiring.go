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
// 摘要桥接（FR-03 §3.4 已收口）：登录校验链把请求明文在服务端转为 SHA-256 摘要
// 后与快照内的摘要恒定时间比较——因此快照凭证的 Token 字段直接承载客户端表的
// `TokenDigest` 列。真源始终是摘要（FR-07），明文不落库、不经快照传递；官方 frpc
// 与 jrpc 均以明文登录、由服务端摘要后比对，两个客户端体系共用同一条校验链。
type storeCredentials struct {
	store *store.Store
}

// DataPlaneCredentials 返回参与数据面登录的凭证集合（摘要形态）。
//
// 只纳入 enrollment 状态为 active 的客户端：pending 尚未兑换凭据、revoked 是
// 终态，两者都不应出现在数据面的凭证集合里。
func (provider storeCredentials) DataPlaneCredentials(ctx context.Context) ([]core.ClientCredential, error) {
	var credentials []core.ClientCredential
	err := provider.store.View(ctx, func(tx *store.Tx) error {
		digests, err := tx.ActiveClientDigests()
		if err != nil {
			return err
		}
		credentials = make([]core.ClientCredential, 0, len(digests))
		for clientID, digest := range digests {
			credentials = append(credentials, core.ClientCredential{
				ClientID: clientID,
				Token:    digest,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return credentials, nil
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
		// bootstrap 凭证只满足构建器的非空约束；引擎启动后立即由启动恢复以
		// desired 快照换代，占位值不会通过任何真实登录（快照里的凭证来自
		// ActiveClientDigests，摘要语义）。
		core.WithClientCredential(core.ClientCredential{ClientID: "bootstrap", Token: server.DigestToken("bootstrap")}),
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
