package notify

import (
	"errors"
	"net"
	"strings"
	"testing"
)

// 合法公网 HTTPS 地址必须放行：校验不能过严而挡住正常目标。
func TestValidateWebhookURLAcceptsPublicHTTPS(t *testing.T) {
	cases := []string{
		"https://hooks.example.com/services/abc",
		"https://hooks.example.com:443/services/abc",
		"https://hooks.example.com:8443/path",
		"https://203.0.114.10/hook",
	}
	for _, raw := range cases {
		if _, err := ValidateWebhookURL(raw); err != nil {
			t.Fatalf("合法地址 %s 应被接受：%v", raw, err)
		}
	}
}

// 非 HTTPS 协议一律拒绝：P1 不允许明文传输通知内容与签名。
func TestValidateWebhookURLRejectsInsecureSchemes(t *testing.T) {
	cases := []string{
		"http://hooks.example.com/hook",
		"ftp://hooks.example.com/hook",
		"file:///etc/passwd",
		"gopher://hooks.example.com/hook",
	}
	for _, raw := range cases {
		_, err := ValidateWebhookURL(raw)
		if err == nil {
			t.Fatalf("非 HTTPS 协议 %s 必须被拒绝", raw)
		}
		if !errors.Is(err, ErrTargetBlocked) {
			t.Fatalf("应返回目标被拒错误：%v", err)
		}
	}
}

// 回环、链路本地与私有地址必须被拒绝（规格 §3.4 SSRF 要求）。
func TestValidateWebhookURLRejectsInternalAddresses(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"IPv4 回环", "https://127.0.0.1/hook"},
		{"IPv4 回环其他地址", "https://127.10.20.30/hook"},
		{"IPv6 回环", "https://[::1]/hook"},
		{"链路本地", "https://169.254.169.254/latest/meta-data"},
		{"IPv6 链路本地", "https://[fe80::1]/hook"},
		{"10 段私有", "https://10.0.0.5/hook"},
		{"172 段私有", "https://172.16.5.5/hook"},
		{"192 段私有", "https://192.168.1.1/hook"},
		{"IPv6 唯一本地", "https://[fd00::1]/hook"},
		{"未指定地址", "https://0.0.0.0/hook"},
		{"组播", "https://224.0.0.1/hook"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			_, err := ValidateWebhookURL(item.url)
			if err == nil {
				t.Fatalf("内部地址 %s 必须被拒绝", item.url)
			}
			if !errors.Is(err, ErrTargetBlocked) {
				t.Fatalf("应返回目标被拒错误：%v", err)
			}
		})
	}
}

// IPv4 映射形式的 IPv6 地址要按其内嵌的 IPv4 判定，不能被绕过。
func TestValidateWebhookURLRejectsMappedIPv4(t *testing.T) {
	cases := []string{
		"https://[::ffff:127.0.0.1]/hook",
		"https://[::ffff:192.168.1.1]/hook",
	}
	for _, raw := range cases {
		if _, err := ValidateWebhookURL(raw); err == nil {
			t.Fatalf("IPv4 映射地址 %s 必须被拒绝", raw)
		}
	}
}

// 非白名单端口一律拒绝：白名单让规则可穷举，不依赖"记得排除哪些端口"。
func TestValidateWebhookURLRejectsUnlistedPorts(t *testing.T) {
	cases := []string{
		"https://hooks.example.com:22/hook",
		"https://hooks.example.com:6379/hook",
		"https://hooks.example.com:9200/hook",
		"https://hooks.example.com:80/hook",
	}
	for _, raw := range cases {
		_, err := ValidateWebhookURL(raw)
		if err == nil {
			t.Fatalf("非白名单端口 %s 必须被拒绝", raw)
		}
		if !errors.Is(err, ErrTargetBlocked) {
			t.Fatalf("应返回目标被拒错误：%v", err)
		}
	}
}

// 地址内嵌凭据必须拒绝：它们会出现在日志与错误信息中。
func TestValidateWebhookURLRejectsEmbeddedCredentials(t *testing.T) {
	if _, err := ValidateWebhookURL("https://user:pass@hooks.example.com/hook"); err == nil {
		t.Fatal("内嵌凭据的地址必须被拒绝")
	}
}

// 空地址与超长地址按格式错误处理。
func TestValidateWebhookURLRejectsMalformed(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"https://",
		"not-a-url",
	}
	for _, raw := range cases {
		_, err := ValidateWebhookURL(raw)
		if err == nil {
			t.Fatalf("非法地址 %q 必须被拒绝", raw)
		}
		if !errors.Is(err, ErrTargetMalformed) {
			t.Fatalf("应返回格式错误：%v", err)
		}
	}
}

// 域名解析出的全部地址都要检查：只查首个会被"同时解析出公网与内网"绕过。
func TestCheckResolvedAddressesChecksAllResults(t *testing.T) {
	public := net.ParseIP("203.0.114.10")
	internal := net.ParseIP("127.0.0.1")

	if err := CheckResolvedAddresses([]net.IP{public}); err != nil {
		t.Fatalf("单个公网地址应放行：%v", err)
	}
	if err := CheckResolvedAddresses([]net.IP{public, internal}); err == nil {
		t.Fatal("解析结果含内网地址时必须拒绝，即使首个是公网")
	}
	if err := CheckResolvedAddresses(nil); err == nil {
		t.Fatal("未解析出任何地址必须拒绝")
	}
}

// 拨号前控制钩子校验真实连接地址：这是防 DNS 重绑定的关键一环。
func TestOutboundControlBlocksInternalAddress(t *testing.T) {
	cases := []struct {
		name    string
		address string
		blocked bool
	}{
		{"回环", "127.0.0.1:443", true},
		{"私有", "10.1.2.3:443", true},
		{"链路本地", "169.254.169.254:443", true},
		{"公网", "203.0.114.10:443", false},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			err := outboundControl("tcp", item.address, nil)
			if item.blocked && err == nil {
				t.Fatalf("拨号到 %s 必须被阻止", item.address)
			}
			if !item.blocked && err != nil {
				t.Fatalf("拨号到 %s 应被放行：%v", item.address, err)
			}
		})
	}
}

// 校验错误不得回显完整地址：地址可能含查询串形式的敏感参数。
func TestValidateWebhookURLErrorDoesNotEchoQuery(t *testing.T) {
	_, err := ValidateWebhookURL("http://hooks.example.com/hook?token=super-secret-value")
	if err == nil {
		t.Fatal("明文协议必须被拒绝")
	}
	if strings.Contains(err.Error(), "super-secret-value") {
		t.Fatalf("校验错误不得回显地址内容：%v", err)
	}
}
