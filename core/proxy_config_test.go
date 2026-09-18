package core_test

import (
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	"github.com/wcpe/jrp/core"
)

// 本文件覆盖 FR-06a §3.2 的注册校验四级顺序与目标地址越权约束。
// 先红后绿：四种代理类型与目标地址允许集合尚未表达时全部失败。

var (
	// testTarget 是一个合法的目标地址，用于允许集合与本地目标。
	testTarget = netip.MustParseAddrPort("127.0.0.1:8080")
	// foreignTarget 是不在允许集合内的目标地址。
	foreignTarget = netip.MustParseAddrPort("127.0.0.1:9090")
)

// assertHasCode 断言问题列表中存在给定错误码与字段路径的问题。
func assertHasCode(t *testing.T, err error, code core.ErrorCode, field string) {
	t.Helper()
	var problems core.ConfigErrors
	if !errors.As(err, &problems) {
		t.Fatalf("错误不是聚合的 ConfigErrors：%v", err)
	}
	for _, problem := range problems {
		if problem.Code() == code && problem.Field() == field {
			return
		}
	}
	t.Fatalf("未找到错误码 %s 字段 %s 的问题：%+v", code, field, problems)
}

// TestServerBindingRequiresAllowedTargets 覆盖目标地址允许集合：缺失即配置校验失败。
func TestServerBindingRequiresAllowedTargets(t *testing.T) {
	_, err := core.NewServerConfig(
		core.WithListen(validListenEndpoint()),
		core.WithWire(core.WireV1),
		core.WithClientCredential(validCredential(testCredentialName)),
		core.WithTCPProxyBinding(core.TCPProxyBinding{
			Name:       "ssh",
			ClientID:   testCredentialName,
			RemotePort: 6000,
		}),
	)
	if err == nil {
		t.Fatalf("缺少目标地址允许集合应当构建失败")
	}
	assertHasCode(t, err, core.CodeIncomplete, "bindings[0].allowedTargets")
}

// TestServerBindingAcceptsAllowedTargets 覆盖允许集合的正常路径与副本语义。
func TestServerBindingAcceptsAllowedTargets(t *testing.T) {
	targets := []netip.AddrPort{testTarget}
	config, err := core.NewServerConfig(
		core.WithListen(validListenEndpoint()),
		core.WithWire(core.WireV1),
		core.WithClientCredential(validCredential(testCredentialName)),
		core.WithTCPProxyBinding(core.TCPProxyBinding{
			Name:           "ssh",
			ClientID:       testCredentialName,
			RemotePort:     6000,
			AllowedTargets: targets,
		}),
	)
	if err != nil {
		t.Fatalf("带允许集合的绑定应当构建成功：%v", err)
	}
	// 修改宿主持有的切片不得影响配置值。
	targets[0] = foreignTarget
	if got := config.Bindings()[0].AllowedTargets[0]; got != testTarget {
		t.Fatalf("配置值随宿主切片被修改：%v", got)
	}
}

// TestAllProxyTypesInOneConfig 覆盖四种代理在同一份配置中共存。
func TestAllProxyTypesInOneConfig(t *testing.T) {
	config, err := core.NewServerConfig(
		core.WithListen(validListenEndpoint()),
		core.WithWire(core.WireV1),
		core.WithClientCredential(validCredential(testCredentialName)),
		core.WithTCPProxyBinding(core.TCPProxyBinding{
			Name: "tcp-1", ClientID: testCredentialName, RemotePort: 6000,
			AllowedTargets: []netip.AddrPort{testTarget},
		}),
		core.WithUDPProxyBinding(core.UDPProxyBinding{
			Name: "udp-1", ClientID: testCredentialName, RemotePort: 6001,
			AllowedTargets: []netip.AddrPort{testTarget},
		}),
		core.WithHTTPProxyBinding(core.HTTPProxyBinding{
			Name: "http-1", ClientID: testCredentialName, RemotePort: 6080,
			Hosts:          []string{"app.example.com"},
			Path:           "/api",
			AllowedTargets: []netip.AddrPort{testTarget},
		}),
		core.WithHTTPSProxyBinding(core.HTTPSProxyBinding{
			Name: "https-1", ClientID: testCredentialName, RemotePort: 6443,
			AllowedTargets: []netip.AddrPort{testTarget},
		}),
	)
	if err != nil {
		t.Fatalf("四种代理共存应当构建成功：%v", err)
	}
	bindings := config.AllBindings()
	if len(bindings) != 4 {
		t.Fatalf("绑定总数不匹配：%d", len(bindings))
	}
	wantTypes := []core.ProxyType{
		core.ProxyTypeTCP, core.ProxyTypeUDP, core.ProxyTypeHTTP, core.ProxyTypeHTTPS,
	}
	for index, want := range wantTypes {
		if bindings[index].Type() != want {
			t.Fatalf("第 %d 项类型不匹配：%s", index, bindings[index].Type())
		}
	}
}

// TestHTTPBindingsSharePort 覆盖 HTTP 代理共享入口端口：不同主机与路径组合可共存。
func TestHTTPBindingsSharePort(t *testing.T) {
	_, err := core.NewServerConfig(
		core.WithListen(validListenEndpoint()),
		core.WithWire(core.WireV1),
		core.WithClientCredential(validCredential(testCredentialName)),
		core.WithHTTPProxyBinding(core.HTTPProxyBinding{
			Name: "a", ClientID: testCredentialName, RemotePort: 6080,
			Hosts:          []string{"a.example.com"},
			AllowedTargets: []netip.AddrPort{testTarget},
		}),
		core.WithHTTPProxyBinding(core.HTTPProxyBinding{
			Name: "b", ClientID: testCredentialName, RemotePort: 6080,
			Hosts:          []string{"b.example.com"},
			AllowedTargets: []netip.AddrPort{testTarget},
		}),
		core.WithHTTPProxyBinding(core.HTTPProxyBinding{
			Name: "c", ClientID: testCredentialName, RemotePort: 6080,
			Hosts:          []string{"a.example.com"},
			Path:           "/admin",
			AllowedTargets: []netip.AddrPort{testTarget},
		}),
	)
	if err != nil {
		t.Fatalf("不同主机与路径组合应能共享入口端口：%v", err)
	}
}

// TestHTTPRouteConflictRejected 覆盖路由冲突：同一端口上主机与路径组合重复即失败。
func TestHTTPRouteConflictRejected(t *testing.T) {
	_, err := core.NewServerConfig(
		core.WithListen(validListenEndpoint()),
		core.WithWire(core.WireV1),
		core.WithClientCredential(validCredential(testCredentialName)),
		core.WithHTTPProxyBinding(core.HTTPProxyBinding{
			Name: "a", ClientID: testCredentialName, RemotePort: 6080,
			Hosts:          []string{"app.example.com"},
			Path:           "/api",
			AllowedTargets: []netip.AddrPort{testTarget},
		}),
		core.WithHTTPProxyBinding(core.HTTPProxyBinding{
			Name: "b", ClientID: testCredentialName, RemotePort: 6080,
			Hosts:          []string{"app.example.com"},
			Path:           "/api",
			AllowedTargets: []netip.AddrPort{testTarget},
		}),
	)
	if err == nil {
		t.Fatalf("主机与路径组合冲突应当构建失败")
	}
	assertHasCode(t, err, core.CodeRouteConflict, "httpBindings[1].hosts")
}

// TestPortConflictRejected 覆盖端口冲突：TCP、UDP、HTTPS 入口独占端口。
func TestPortConflictRejected(t *testing.T) {
	_, err := core.NewServerConfig(
		core.WithListen(validListenEndpoint()),
		core.WithWire(core.WireV1),
		core.WithClientCredential(validCredential(testCredentialName)),
		core.WithTCPProxyBinding(core.TCPProxyBinding{
			Name: "tcp-1", ClientID: testCredentialName, RemotePort: 6000,
			AllowedTargets: []netip.AddrPort{testTarget},
		}),
		core.WithUDPProxyBinding(core.UDPProxyBinding{
			Name: "udp-1", ClientID: testCredentialName, RemotePort: 6000,
			AllowedTargets: []netip.AddrPort{testTarget},
		}),
	)
	if err == nil {
		t.Fatalf("TCP 与 UDP 占用同一端口应当构建失败")
	}
	assertHasCode(t, err, core.CodePortConflict, "udpBindings[0].remotePort")
}

// TestHTTPSBindingHasNoCertificateField 覆盖 HTTPS 透传边界：绑定类型不含证书字段。
//
// 断言方式：HTTPS 绑定的字段集合必须不包含任何证书或私钥字段，因此反射字段名
// 中不得出现 cert、key 的拼写；Core 不持有被代理服务的 TLS 材料（§3.6）。
func TestHTTPSBindingHasNoCertificateField(t *testing.T) {
	binding := core.HTTPSProxyBinding{Name: "https-1"}
	fields := []string{}
	bindingValue := reflectValue(binding)
	for index := 0; index < bindingValue.NumField(); index++ {
		fields = append(fields, bindingValue.Type().Field(index).Name)
	}
	for _, field := range fields {
		if stringsContainsFold(field, "cert") || stringsContainsFold(field, "key") ||
			stringsContainsFold(field, "tls") {
			t.Fatalf("HTTPS 绑定不得出现证书或私钥字段，实际字段集合 %v", fields)
		}
	}
	if len(fields) == 0 {
		t.Fatalf("字段集合不应为空")
	}
}

// TestHTTPProxyRejectsEmptyHosts 覆盖 HTTP 绑定的主机名必填。
func TestHTTPProxyRejectsEmptyHosts(t *testing.T) {
	_, err := core.NewServerConfig(
		core.WithListen(validListenEndpoint()),
		core.WithWire(core.WireV1),
		core.WithClientCredential(validCredential(testCredentialName)),
		core.WithHTTPProxyBinding(core.HTTPProxyBinding{
			Name:           "http-1",
			ClientID:       testCredentialName,
			RemotePort:     6080,
			AllowedTargets: []netip.AddrPort{testTarget},
		}),
	)
	if err == nil {
		t.Fatalf("HTTP 代理缺少主机名应当构建失败")
	}
	assertHasCode(t, err, core.CodeIncomplete, "httpBindings[0].hosts")
}

// TestClientProxyTypesInOneConfig 覆盖客户端四种代理共存与跨类型重名检测。
func TestClientProxyTypesInOneConfig(t *testing.T) {
	_, err := core.NewClientConfig(
		core.WithClientID(testCredentialName),
		core.WithServerEndpoint(core.ServerEndpoint{
			Address:   netip.MustParseAddrPort("127.0.0.1:7000"),
			Transport: core.TransportTCP,
			Wire:      core.WireV1,
		}),
		core.WithClientAuth(core.TokenAuth{Token: testToken}),
		core.WithTCPProxy(core.TCPProxy{Name: "dup", LocalAddr: testTarget, RemotePort: 6000}),
		core.WithUDPProxy(core.UDPProxy{Name: "dup", LocalAddr: testTarget, RemotePort: 6001}),
	)
	if err == nil {
		t.Fatalf("跨类型重名应当构建失败")
	}
	assertHasCode(t, err, core.CodeDuplicateProxyName, "udpProxies[0].name")
}

// TestUDPParametersUseDefaults 覆盖 UDP 资源约束参数的默认值与可配。
func TestUDPParametersUseDefaults(t *testing.T) {
	config, err := core.NewServerConfig(
		core.WithListen(validListenEndpoint()),
		core.WithWire(core.WireV1),
		core.WithClientCredential(validCredential(testCredentialName)),
	)
	if err != nil {
		t.Fatalf("无代理的服务端配置应当构建成功：%v", err)
	}
	if config.UDPSessionIdle() != core.DefaultUDPSessionIdle {
		t.Fatalf("空闲上限应采用默认值：%s", config.UDPSessionIdle())
	}
	if config.UDPSessionLimit() != core.DefaultUDPSessionLimit {
		t.Fatalf("会话上限应采用默认值：%d", config.UDPSessionLimit())
	}
	if config.UDPDatagramSize() != core.DefaultUDPDatagramSize {
		t.Fatalf("数据报上限应采用默认值：%d", config.UDPDatagramSize())
	}
}

// reflectValue 返回绑定值的反射视图，供字段名断言使用。
func reflectValue(binding any) reflect.Value {
	return reflect.ValueOf(binding)
}

// stringsContainsFold 判定字符串是否包含给定子串，忽略大小写。
func stringsContainsFold(value, substring string) bool {
	return strings.Contains(strings.ToLower(value), strings.ToLower(substring))
}

// TestHTTPSBindingRejectsCaptureEnabling 覆盖 HTTPS 透传边界：
// HTTPS 绑定上没有、也不得出现正文采集开关（规格 §3.6）。
//
// HTTP 是唯一允许开启采集的代理类型，因此采集开关只存在于 HTTP 绑定；HTTPS
// 透传看不到明文语义，若也提供开关就会形成「看起来支持」的错误界面。
func TestHTTPSBindingRejectsCaptureEnabling(t *testing.T) {
	binding := core.HTTPSProxyBinding{Name: "https-1"}
	value := reflect.ValueOf(binding)
	for index := 0; index < value.NumField(); index++ {
		field := value.Type().Field(index).Name
		if stringsContainsFold(field, "capture") || stringsContainsFold(field, "body") {
			t.Fatalf("HTTPS 绑定不得提供正文采集开关，实际字段 %s", field)
		}
	}
	// HTTP 绑定保留采集开关，且默认关闭。
	httpBinding := core.HTTPProxyBinding{Name: "http-1", Hosts: []string{"a.example.com"}}
	if httpBinding.CaptureBody {
		t.Fatalf("HTTP 正文采集开关必须默认关闭")
	}
}
