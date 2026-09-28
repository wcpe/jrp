package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jrp/apps/jrps/internal/store"
	"github.com/wcpe/jrp/core"
)

func TestBuildStartupServerConfigDefaultsToTCP(t *testing.T) {
	config, err := buildStartupServerConfig(store.DesiredDocument{
		ControlListen: store.ControlListen{Host: "127.0.0.1", Port: 7200},
	}, []core.ClientCredential{{ClientID: "client-a", Token: "digest-a"}})
	if err != nil {
		t.Fatalf("构造默认启动配置失败：%v", err)
	}
	if config.Listen().Transport != core.TransportTCP {
		t.Fatalf("缺省传输应为 TCP，实际为 %q", config.Listen().Transport)
	}
	if got := config.Listen().Address.String(); got != "127.0.0.1:7200" {
		t.Fatalf("控制监听地址不匹配：%s", got)
	}
	if credentials := config.Credentials(); len(credentials) != 1 || credentials[0].ClientID != "client-a" {
		t.Fatalf("active 凭证未进入启动配置：%+v", credentials)
	}
}

func TestBuildStartupServerConfigLeavesNonTCPListenerToCore(t *testing.T) {
	cases := []store.ControlTransport{
		store.ControlTransportWebSocket,
		store.ControlTransportWSS,
		store.ControlTransportKCP,
		store.ControlTransportQUIC,
	}
	for _, transport := range cases {
		t.Run(string(transport), func(t *testing.T) {
			document := store.DesiredDocument{ControlListen: store.ControlListen{
				Host: "127.0.0.1", Port: 7200, Transport: transport,
			}}
			if transport == store.ControlTransportWSS || transport == store.ControlTransportQUIC {
				document.ControlListen.TLSCertFile, document.ControlListen.TLSKeyFile = writeControlTLSFiles(t)
			}
			config, err := buildStartupServerConfig(document, []core.ClientCredential{{ClientID: "client-a", Token: "digest-a"}})
			if err != nil {
				t.Fatalf("构造 %s 启动配置失败：%v", transport, err)
			}
			if config.Listen().Transport != core.Transport(transport) {
				t.Fatalf("传输不匹配：%q", config.Listen().Transport)
			}
			if transport == store.ControlTransportWebSocket || transport == store.ControlTransportWSS {
				if config.Listen().TransportConfig.WebSocket.Path != "/frp" {
					t.Fatalf("WebSocket 路径不匹配：%q", config.Listen().TransportConfig.WebSocket.Path)
				}
			}
		})
	}
}

func TestReadControlTLSDoesNotLeakPrivateKey(t *testing.T) {
	directory := t.TempDir()
	certPath := filepath.Join(directory, "cert.pem")
	keyPath := filepath.Join(directory, "key.pem")
	if err := os.WriteFile(certPath, []byte("-----BEGIN CERTIFICATE-----\n-----END CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatalf("写入测试证书失败：%v", err)
	}
	privateKey := "私钥绝密测试值"
	if err := os.WriteFile(keyPath, []byte(privateKey), 0o600); err != nil {
		t.Fatalf("写入测试私钥失败：%v", err)
	}
	_, err := readControlTLS(certPath, keyPath)
	if err == nil {
		t.Fatal("无效 TLS 材料应返回错误")
	}
	if strings.Contains(err.Error(), privateKey) {
		t.Fatalf("错误泄露了私钥内容：%v", err)
	}
}

func writeControlTLSFiles(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成测试私钥失败：%v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		t.Fatalf("生成测试证书序列号失败：%v", err)
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
	}, &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
	}, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("生成测试证书失败：%v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("编码测试私钥失败：%v", err)
	}
	directory := t.TempDir()
	certPath := filepath.Join(directory, "cert.pem")
	keyPath := filepath.Join(directory, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}), 0o600); err != nil {
		t.Fatalf("写入测试证书失败：%v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("写入测试私钥失败：%v", err)
	}
	return certPath, keyPath
}
