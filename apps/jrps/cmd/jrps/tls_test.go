package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 证书指纹应与直接对 DER 计算 SHA-256 的结果一致，且为冒号分组的十六进制。
func TestCertificateFingerprintMatchesDER(t *testing.T) {
	certificateDER := writeSelfSignedCertificate(t)

	expected := sha256.Sum256(certificateDER)
	octets := make([]string, 0, len(expected))
	for _, value := range expected {
		octets = append(octets, fmt.Sprintf("%02X", value))
	}
	expectedText := strings.Join(octets, ":")

	path := filepath.Join(t.TempDir(), "cert.pem")
	writePEM(t, path, "CERTIFICATE", certificateDER)

	actual, err := certificateFingerprint(path)
	if err != nil {
		t.Fatalf("计算指纹失败：%v", err)
	}
	if actual != expectedText {
		t.Fatalf("指纹不一致：\n期望 %s\n实际 %s", expectedText, actual)
	}
	// 形态校验收窄到"32 组两位十六进制"：这是与浏览器核对的前提。
	if groups := strings.Split(actual, ":"); len(groups) != 32 {
		t.Fatalf("指纹应含 32 组，实际 %d 组", len(groups))
	}
	for _, group := range strings.Split(actual, ":") {
		if len(group) != 2 || strings.ToUpper(group) != group {
			t.Fatalf("指纹分组应为两位大写十六进制：%q", group)
		}
	}
}

// 非 PEM 文件应被拒绝，而不是给出错误的指纹。
func TestCertificateFingerprintRejectsNonPEM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-cert.pem")
	if err := os.WriteFile(path, []byte("这不是 PEM"), 0o600); err != nil {
		t.Fatalf("写入测试文件失败：%v", err)
	}
	if _, err := certificateFingerprint(path); err == nil {
		t.Fatal("非 PEM 文件应被拒绝")
	}
}

// TLS 参数只给一个应拒绝启动，且不得静默降级为明文。
//
// 回归用例：静默降级会让管理员以为自己在用 HTTPS，而实际是明文管理连接——
// 这正是 FR-02 §5 实机条款要防的场景。
func TestServeRejectsIncompleteTLSConfig(t *testing.T) {
	dataDirectory := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	database, err := openStore(filepath.Join(dataDirectory, "jrps.db"), logger)
	if err != nil {
		t.Fatalf("打开数据库失败：%v", err)
	}
	defer func() { _ = database.Close() }()

	certificateDER := writeSelfSignedCertificate(t)
	certPath := filepath.Join(dataDirectory, "cert.pem")
	writePEM(t, certPath, "CERTIFICATE", certificateDER)

	// 只给证书：应拒绝。
	if code := serve("127.0.0.1:0", certPath, "", database, logger); code != 2 {
		t.Fatalf("只给证书应返回 2，实际 %d", code)
	}
	// 只给私钥：同样拒绝。
	if code := serve("127.0.0.1:0", "", "key.pem", database, logger); code != 2 {
		t.Fatalf("只给私钥应返回 2，实际 %d", code)
	}
}

// writeSelfSignedCertificate 生成一张自签证书并返回 DER 字节。
func writeSelfSignedCertificate(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败：%v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("生成序列号失败：%v", err)
	}
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "jrps-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发证书失败：%v", err)
	}
	return der
}

// writePEM 把 DER 以 PEM 形态写入指定路径。
func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	encoded := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("写入 PEM 失败：%v", err)
	}
}
