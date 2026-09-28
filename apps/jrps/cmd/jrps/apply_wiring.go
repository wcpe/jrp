package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"

	"github.com/wcpe/jrp/apps/jrps/internal/apply"
	"github.com/wcpe/jrp/apps/jrps/internal/store"
	"github.com/wcpe/jrp/core"
	"github.com/wcpe/jrp/core/server"
)

// 数据面控制监听的默认端口；desired 文档未显式指定时使用同一默认值。
const (
	dataPlaneListenPort = store.DefaultControlListenPort
	defaultControlPath  = "/frp"
)

// storeCredentials 从 jrps 数据库组装数据面凭证集合（apply.CredentialProvider）。
//
// 双链装配（FR-03 §3.4）：摘要列（TokenDigest）供 jrpc 的摘要链使用——客户端送
// 明文、服务端摘要后恒定时间比较；兼容明文列（TokenCompat）供官方 frpc 使用——
// 官方客户端只送 md5(token ∥ 时间戳)，服务端必须持有明文才能复算。两条链共用
// 同一条兼容消息路径，管理面与日志一律只展示摘要前缀。
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
		materials, err := tx.ActiveClientCredentials()
		if err != nil {
			return err
		}
		credentials = make([]core.ClientCredential, 0, len(materials))
		for clientID, material := range materials {
			credentials = append(credentials, core.ClientCredential{
				ClientID:    clientID,
				Token:       material.Digest,
				CompatToken: material.CompatToken,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return credentials, nil
}

// startEngine 装配 Core 服务端引擎：先读取 desired 与 active 凭证，再按传输方式启动。
//
// TCP 控制监听器由宿主创建并注入；其它传输由 Core 按配置自行创建监听器。
func startEngine(ctx context.Context, database *store.Store, logger *slog.Logger) (*server.Engine, error) {
	initialConfig, listener, err := startupConfig(ctx, database)
	if err != nil {
		return nil, err
	}
	options := []server.Option{server.WithLogger(logger)}
	if listener != nil {
		options = append(options, server.WithListener(listener))
	}
	engine := server.New(initialConfig, options...)
	if err := engine.Start(ctx); err != nil {
		if listener != nil {
			_ = listener.Close()
		}
		return nil, fmt.Errorf("引擎启动失败：%w", err)
	}
	return engine, nil
}

// startupConfig 读取启动前的 desired 与 active 凭证，并构造 Core 首次配置。
func startupConfig(ctx context.Context, database *store.Store) (core.ServerConfig, net.Listener, error) {
	document, credentials, err := loadStartupState(ctx, database)
	if err != nil {
		return core.ServerConfig{}, nil, err
	}
	config, err := buildStartupServerConfig(document, credentials)
	if err != nil {
		return core.ServerConfig{}, nil, err
	}
	if config.Listen().Transport != core.TransportTCP {
		return config, nil, nil
	}
	listener, err := net.Listen("tcp", config.Listen().Address.String())
	if err != nil {
		return core.ServerConfig{}, nil, fmt.Errorf("控制监听端口被占用：%w", err)
	}
	return config, listener, nil
}

// loadStartupState 从数据库读取最新 desired 与当前 active 凭证。
func loadStartupState(ctx context.Context, database *store.Store) (store.DesiredDocument, []core.ClientCredential, error) {
	document := store.DesiredDocument{ControlListen: store.ControlListen{Port: dataPlaneListenPort}}
	var credentials []core.ClientCredential
	err := database.View(ctx, func(tx *store.Tx) error {
		latest, err := tx.LatestRevision()
		if errors.Is(err, store.ErrNoRevision) {
			latest = store.ConfigRevision{}
		} else if err != nil {
			return err
		}
		if latest.Revision != 0 {
			document, err = store.ParseDesiredDocument(latest.Content)
			if err != nil {
				return fmt.Errorf("读取最新 desired 失败：%w", err)
			}
		}
		materials, err := tx.ActiveClientCredentials()
		if err != nil {
			return err
		}
		credentials = make([]core.ClientCredential, 0, len(materials))
		for clientID, material := range materials {
			credentials = append(credentials, core.ClientCredential{
				ClientID:    clientID,
				Token:       material.Digest,
				CompatToken: material.CompatToken,
			})
		}
		return nil
	})
	if err != nil {
		return store.DesiredDocument{}, nil, err
	}
	return document, credentials, nil
}

// buildStartupServerConfig 将 desired 控制监听转换为 Core 配置。
func buildStartupServerConfig(document store.DesiredDocument, credentials []core.ClientCredential) (core.ServerConfig, error) {
	listen, err := document.ControlListen.AddrPort()
	if err != nil {
		return core.ServerConfig{}, fmt.Errorf("构造控制监听失败：%w", err)
	}
	transport := document.ControlListen.Transport
	if transport == "" {
		transport = store.ControlTransportTCP
	}
	endpoint := core.BindEndpoint{Address: listen, Transport: core.Transport(transport)}
	switch transport {
	case store.ControlTransportWebSocket, store.ControlTransportWSS:
		endpoint.TransportConfig.WebSocket.Path = document.ControlListen.Path
		if endpoint.TransportConfig.WebSocket.Path == "" {
			endpoint.TransportConfig.WebSocket.Path = defaultControlPath
		}
		if transport == store.ControlTransportWSS {
			tlsConfig, err := readControlTLS(document.ControlListen.TLSCertFile, document.ControlListen.TLSKeyFile)
			if err != nil {
				return core.ServerConfig{}, err
			}
			endpoint.TransportConfig.WebSocket.TLS = tlsConfig
		}
	case store.ControlTransportQUIC:
		tlsConfig, err := readControlTLS(document.ControlListen.TLSCertFile, document.ControlListen.TLSKeyFile)
		if err != nil {
			return core.ServerConfig{}, err
		}
		endpoint.TransportConfig.QUIC.TLS = tlsConfig
	}
	options := []core.ServerOption{core.WithListen(endpoint), core.WithWire(core.WireV1)}
	if len(credentials) == 0 {
		// 无版本或尚未有 active 凭证时，仅用占位凭证满足 Core 构建约束；
		// 启动恢复成功后会立即用 active 凭证替换运行态。
		credentials = []core.ClientCredential{{ClientID: "bootstrap", Token: server.DigestToken("bootstrap")}}
	}
	options = append(options, core.WithClientCredentials(credentials))
	config, err := core.NewServerConfig(options...)
	if err != nil {
		return core.ServerConfig{}, fmt.Errorf("构造引擎初始配置失败：%w", err)
	}
	return config, nil
}

// readControlTLS 从 desired 指定的文件读取证书与私钥 PEM。
func readControlTLS(certPath, keyPath string) (core.TLSConfig, error) {
	certificatePEM, err := os.ReadFile(certPath)
	if err != nil {
		return core.TLSConfig{}, errors.New("读取控制监听 TLS 证书失败")
	}
	privateKeyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return core.TLSConfig{}, errors.New("读取控制监听 TLS 私钥失败")
	}
	if _, err := tls.X509KeyPair(certificatePEM, privateKeyPEM); err != nil {
		return core.TLSConfig{}, errors.New("解析控制监听 TLS 证书失败")
	}
	return core.TLSConfig{CertificatePEM: string(certificatePEM), PrivateKeyPEM: string(privateKeyPEM)}, nil
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
