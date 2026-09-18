package core_test

import (
	"net/netip"
	"testing"
	"time"

	"github.com/wcpe/jrp/core"
)

// TestClientConfigIsImmutableAgainstHostSlices 覆盖宿主持有的入参切片被修改后配置值不变。
func TestClientConfigIsImmutableAgainstHostSlices(t *testing.T) {
	proxies := []core.TCPProxy{
		validTCPProxy("ssh", 6000),
		validTCPProxy("web", 6001),
	}

	config, err := core.NewClientConfig(
		core.WithClientID(testCredentialName),
		core.WithServerEndpoint(validServerEndpoint()),
		core.WithClientAuth(core.TokenAuth{Token: testToken}),
		core.WithTCPProxies(proxies),
		core.WithHeartbeat(30*time.Second),
	)
	if err != nil {
		t.Fatalf("构建失败：%v", err)
	}

	proxies[0].Name = "被宿主改写"
	proxies[0].RemotePort = 9999
	proxies[1].LocalAddr = netip.MustParseAddrPort("127.0.0.1:9999")

	assertClientProxyNames(t, config, []string{"ssh", "web"})
	if config.Proxies()[0].RemotePort != 6000 || config.Proxies()[1].LocalAddr != netip.MustParseAddrPort("127.0.0.1:22") {
		t.Fatalf("宿主持有切片被修改后配置值发生变化：%+v", config.Proxies())
	}
}

// TestClientConfigReturnsCopies 覆盖读取返回的切片与结构体副本不可回写配置值。
func TestClientConfigReturnsCopies(t *testing.T) {
	config, err := core.NewClientConfig(validClientOptions()...)
	if err != nil {
		t.Fatalf("构建失败：%v", err)
	}

	proxies := config.Proxies()
	proxies[0].Name = "被读取方改写"
	proxies[0].RemotePort = 9999
	proxies = append(proxies, validTCPProxy("注入", 7000))
	if len(proxies) != 2 {
		t.Fatalf("追加结果不匹配：%d", len(proxies))
	}

	assertClientProxyNames(t, config, []string{"ssh"})
	if config.Proxies()[0].RemotePort != 6000 {
		t.Fatalf("读取返回值被修改后配置值发生变化：%+v", config.Proxies())
	}

	endpoint := config.ServerEndpoint()
	endpoint.Address = netip.MustParseAddrPort("127.0.0.1:9999")
	if config.ServerEndpoint().Address != netip.MustParseAddrPort("127.0.0.1:7000") {
		t.Fatalf("读取端点副本被修改后配置值发生变化：%+v", config.ServerEndpoint())
	}

	auth := config.Auth()
	auth.Token = "被读取方改写"
	if config.Auth().Token != testToken {
		t.Fatalf("读取鉴权副本被修改后配置值发生变化：%s", config.Auth().Token)
	}
}

// TestServerConfigIsImmutableAgainstHostSlices 覆盖服务端入参切片与读取副本的隔离。
func TestServerConfigIsImmutableAgainstHostSlices(t *testing.T) {
	credentials := []core.ClientCredential{
		validCredential("client-a"),
		{ClientID: "client-b", Token: "second-token"},
	}
	bindings := []core.TCPProxyBinding{
		validBinding("ssh", "client-a", 6000),
		validBinding("web", "client-b", 6001),
	}

	config, err := core.NewServerConfig(
		core.WithListen(validListenEndpoint()),
		core.WithWire(core.WireV1),
		core.WithClientCredentials(credentials),
		core.WithTCPProxyBindings(bindings),
	)
	if err != nil {
		t.Fatalf("构建失败：%v", err)
	}

	credentials[0].Token = "被宿主改写"
	credentials[1].ClientID = "被宿主改写"
	bindings[0].Name = "被宿主改写"
	bindings[1].RemotePort = 9999

	assertServerCollections(t, config)

	returnedCredentials := config.Credentials()
	returnedCredentials[0].Token = "被读取方改写"
	returnedBindings := config.Bindings()
	returnedBindings[0].Name = "被读取方改写"

	assertServerCollections(t, config)
}

// assertClientProxyNames 断言客户端配置中的代理名序列。
func assertClientProxyNames(t *testing.T, config core.ClientConfig, expected []string) {
	t.Helper()
	proxies := config.Proxies()
	if len(proxies) != len(expected) {
		t.Fatalf("代理条目数不匹配：实际 %d，期望 %d", len(proxies), len(expected))
	}
	for index, name := range expected {
		if proxies[index].Name != name {
			t.Fatalf("第 %d 个代理名不匹配：实际 %s，期望 %s", index, proxies[index].Name, name)
		}
	}
}

// assertServerCollections 断言服务端配置中的凭证与绑定内容未被外部修改影响。
func assertServerCollections(t *testing.T, config core.ServerConfig) {
	t.Helper()
	credentials := config.Credentials()
	if len(credentials) != 2 || credentials[0].Token != testToken || credentials[1].ClientID != "client-b" {
		t.Fatalf("凭证集合不匹配：%+v", credentials)
	}
	if credentials[1].Token != "second-token" {
		t.Fatalf("凭证 token 不匹配：%+v", credentials)
	}
	bindings := config.Bindings()
	if len(bindings) != 2 || bindings[0].Name != "ssh" || bindings[1].Name != "web" {
		t.Fatalf("绑定集合不匹配：%+v", bindings)
	}
	// 断言「未被宿主改写」这一原意图：宿主把第二条改写为 9999，两条入口端口
	// 又必须互不相同（TCP 入口独占端口），因此逐端口断言不等于 9999。
	if bindings[0].RemotePort == 9999 || bindings[1].RemotePort == 9999 {
		t.Fatalf("绑定远程端口被宿主改写：%+v", bindings)
	}
}
