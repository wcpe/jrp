package transport

import (
	"crypto/tls"
	"net/http"
	"strings"
	"time"

	"github.com/wcpe/jrp/core"
	kcp "github.com/xtaci/kcp-go/v5"
)

// WebSocketOptionsFromConfig 将 Core 的 WebSocket 配置转换为内部适配参数。
func WebSocketOptionsFromConfig(config core.WebSocketConfig, secure bool) (WebSocketOptions, error) {
	options := WebSocketOptions{Path: config.Path, MaxPayloadBytes: config.MaxPayloadBytes}
	if config.Header != "" {
		parts := strings.SplitN(config.Header, ":", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
			return WebSocketOptions{}, coreConfigError("WebSocket 握手头格式无效")
		}
		options.Headers = make(http.Header)
		options.Headers.Set(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]))
	}
	if secure {
		tlsConfig, err := tlsConfigFromCore(config.TLS, false)
		if err != nil {
			return WebSocketOptions{}, err
		}
		options.TLS = tlsConfig
	}
	return options, nil
}

// KCPOptionsFromConfig 将 Core 的 KCP 配置转换为内部适配参数。
func KCPOptionsFromConfig(config core.KCPConfig) (KCPOptions, error) {
	options := KCPOptions{MTU: config.MTU, SendWindow: config.SendWindow, ReceiveWindow: config.ReceiveWindow, DataShards: config.DataShards, ParityShards: config.ParityShards, NoDelay: config.NoDelay, Interval: config.Interval, Resend: config.Resend, NoCongestion: config.NoCongestion}
	if options.MTU == 0 {
		options.MTU = 1350
	}
	if options.SendWindow == 0 {
		options.SendWindow = 1024
	}
	if options.ReceiveWindow == 0 {
		options.ReceiveWindow = 1024
	}
	if options.DataShards == 0 {
		options.DataShards = 10
	}
	if options.ParityShards == 0 {
		options.ParityShards = 3
	}
	if options.Interval == 0 {
		options.Interval = 20 * time.Millisecond
	}
	if options.Resend == 0 {
		options.Resend = 2
	}
	options.NoDelay = true
	options.NoCongestion = true
	if config.Key != "" {
		block, err := kcp.NewAESBlockCrypt([]byte(config.Key))
		if err != nil {
			return KCPOptions{}, coreConfigError("KCP 加密密钥无效")
		}
		options.blockCrypt = block
	}
	return options, nil
}

// QUICOptionsFromConfig 将 Core 的 QUIC 配置转换为内部适配参数。
func QUICOptionsFromConfig(config core.QUICConfig, server bool) (QUICOptions, error) {
	tlsConfig, err := tlsConfigFromCore(config.TLS, server)
	if err != nil {
		return QUICOptions{}, err
	}
	alpn := config.ALPN
	if alpn == "" {
		alpn = "frp"
	}
	return QUICOptions{TLS: tlsConfig, ALPN: alpn, HandshakeTimeout: config.HandshakeTimeout, MaxIdleTimeout: config.MaxIdleTimeout, KeepAlivePeriod: config.KeepAlivePeriod, InitialStreamReceiveWindow: config.InitialStreamReceiveWindow, MaxStreamReceiveWindow: config.MaxStreamReceiveWindow, InitialConnectionReceiveWindow: config.InitialConnectionReceiveWindow, MaxConnectionReceiveWindow: config.MaxConnectionReceiveWindow, MaxIncomingStreams: config.MaxIncomingStreams}, nil
}

func tlsConfigFromCore(config core.TLSConfig, server bool) (*tls.Config, error) {
	return TLSConfigFromPEM(config.ServerName, []byte(config.RootCAPEM), []byte(config.CertificatePEM), []byte(config.PrivateKeyPEM), config.FingerprintSHA256, server)
}

func coreConfigError(message string) error { return &transportConfigError{message: message} }

type transportConfigError struct{ message string }

func (err *transportConfigError) Error() string { return err.message }
