package core_test

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jrp/core"
)

// validListenEndpoint 返回一个合法的监听端点。
func validListenEndpoint() core.BindEndpoint {
	return core.BindEndpoint{
		Address:   netip.MustParseAddrPort("0.0.0.0:7000"),
		Transport: core.TransportTCP,
	}
}

// validCredential 返回一条合法的客户端凭证。
func validCredential(clientID string) core.ClientCredential {
	return core.ClientCredential{ClientID: clientID, Token: testToken}
}

// validBinding 返回一条合法的服务端代理绑定。
func validBinding(name, clientID string) core.TCPProxyBinding {
	return core.TCPProxyBinding{Name: name, ClientID: clientID, RemotePort: 6000}
}

// validServerOptions 返回规格 §3.2 服务端示例对应的选项集合。
func validServerOptions() []core.ServerOption {
	return []core.ServerOption{
		core.WithListen(validListenEndpoint()),
		core.WithWire(core.WireV1),
		core.WithClientCredential(validCredential(testCredentialName)),
		core.WithTCPProxyBinding(validBinding("ssh", testCredentialName)),
	}
}

// withServerOptions 在合法选项基础上追加额外选项。
func withServerOptions(extra ...core.ServerOption) []core.ServerOption {
	return append(validServerOptions(), extra...)
}

// TestNewServerConfigFromSpecExample 覆盖规格 §3.2 的服务端示例与正常路径验收。
func TestNewServerConfigFromSpecExample(t *testing.T) {
	config, err := core.NewServerConfig(
		core.WithListen(core.BindEndpoint{
			Address:   netip.MustParseAddrPort("0.0.0.0:7000"),
			Transport: core.TransportTCP,
		}),
		core.WithWire(core.WireV1),
		core.WithClientCredential(core.ClientCredential{
			ClientID: "client-a",
			Token:    testToken,
		}),
		core.WithTCPProxyBinding(core.TCPProxyBinding{
			Name:       "ssh",
			ClientID:   "client-a",
			RemotePort: 6000,
		}),
	)
	if err != nil {
		t.Fatalf("示例配置应当构建成功：%v", err)
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("示例配置校验应当通过：%v", err)
	}

	if config.Listen() != validListenEndpoint() {
		t.Fatalf("监听端点不匹配：%+v", config.Listen())
	}
	if config.Wire() != core.WireV1 {
		t.Fatalf("wire 版本不匹配：%s", config.Wire())
	}
	if config.Heartbeat() != core.DefaultHeartbeat {
		t.Fatalf("未设置心跳应采用默认值：%s", config.Heartbeat())
	}
	if config.Timeout() != core.DefaultTimeout {
		t.Fatalf("未设置超时应采用默认值：%s", config.Timeout())
	}

	credentials := config.Credentials()
	if len(credentials) != 1 || credentials[0] != validCredential("client-a") {
		t.Fatalf("凭证集合不匹配：%+v", credentials)
	}
	bindings := config.Bindings()
	if len(bindings) != 1 || bindings[0] != validBinding("ssh", "client-a") {
		t.Fatalf("代理绑定集合不匹配：%+v", bindings)
	}
}

// TestServerConfigValidateIsRepeatable 覆盖「Validate 可重复调用且无副作用」。
func TestServerConfigValidateIsRepeatable(t *testing.T) {
	config, err := core.NewServerConfig(validServerOptions()...)
	if err != nil {
		t.Fatalf("构建失败：%v", err)
	}

	for attempt := 0; attempt < 3; attempt++ {
		if err := config.Validate(); err != nil {
			t.Fatalf("第 %d 次校验不应失败：%v", attempt+1, err)
		}
		if len(config.Credentials()) != 1 || len(config.Bindings()) != 1 {
			t.Fatalf("第 %d 次校验后配置值发生变化", attempt+1)
		}
	}
}

// TestNewServerConfigDefaultDurations 覆盖零值时间参数采用 Core 默认常量。
func TestNewServerConfigDefaultDurations(t *testing.T) {
	config, err := core.NewServerConfig(withServerOptions(
		core.WithServerHeartbeat(0),
		core.WithServerTimeout(0),
	)...)
	if err != nil {
		t.Fatalf("零值时间参数应当构建成功：%v", err)
	}
	if config.Heartbeat() != core.DefaultHeartbeat || config.Timeout() != core.DefaultTimeout {
		t.Fatalf("零值时间参数未采用默认常量：%s / %s", config.Heartbeat(), config.Timeout())
	}
}

// TestServerConfigCountBoundaries 覆盖凭证数与绑定数的上限与上限加一。
func TestServerConfigCountBoundaries(t *testing.T) {
	credentials := make([]core.ClientCredential, 0, core.MaxClientCredentialCount+1)
	bindings := make([]core.TCPProxyBinding, 0, core.MaxProxyCount+1)
	for index := 0; index <= core.MaxClientCredentialCount; index++ {
		clientID := fmt.Sprintf("client-%d", index)
		credentials = append(credentials, validCredential(clientID))
	}
	for index := 0; index <= core.MaxProxyCount; index++ {
		bindings = append(bindings, validBinding(fmt.Sprintf("proxy-%d", index), "client-0"))
	}

	config, err := core.NewServerConfig(
		core.WithListen(validListenEndpoint()),
		core.WithWire(core.WireV1),
		core.WithClientCredentials(credentials[:core.MaxClientCredentialCount]),
		core.WithTCPProxyBindings(bindings[:core.MaxProxyCount]),
	)
	if err != nil {
		t.Fatalf("恰为上限时应当构建成功：%v", err)
	}
	if len(config.Credentials()) != core.MaxClientCredentialCount || len(config.Bindings()) != core.MaxProxyCount {
		t.Fatalf("上限条目数不匹配：%d / %d", len(config.Credentials()), len(config.Bindings()))
	}

	_, err = core.NewServerConfig(
		core.WithListen(validListenEndpoint()),
		core.WithWire(core.WireV1),
		core.WithClientCredentials(credentials),
		core.WithTCPProxyBindings(bindings[:core.MaxProxyCount]),
	)
	assertSingleErrorCode(t, err, core.CodeLimitExceeded, "credentials")

	_, err = core.NewServerConfig(
		core.WithListen(validListenEndpoint()),
		core.WithWire(core.WireV1),
		core.WithClientCredentials(credentials[:core.MaxClientCredentialCount]),
		core.WithTCPProxyBindings(bindings),
	)
	assertSingleErrorCode(t, err, core.CodeLimitExceeded, "bindings")
}

// TestServerConfigPortBoundary 覆盖监听端口 1 与 65535 成功、0 与 65536 失败。
func TestServerConfigPortBoundary(t *testing.T) {
	for _, port := range []int{1, 65535} {
		config, err := core.NewServerConfig(
			core.WithListen(core.BindEndpoint{
				Address:   netip.AddrPortFrom(netip.MustParseAddr("0.0.0.0"), uint16(port)),
				Transport: core.TransportTCP,
			}),
			core.WithWire(core.WireV1),
			core.WithClientCredential(validCredential(testCredentialName)),
			core.WithTCPProxyBinding(validBinding("ssh", testCredentialName)),
		)
		if err != nil {
			t.Fatalf("监听端口 %d 应当构建成功：%v", port, err)
		}
		if config.Listen().Address.Port() != uint16(port) {
			t.Fatalf("监听端口不匹配：%d", config.Listen().Address.Port())
		}
	}

	_, err := core.NewServerConfig(validServerOptions()...)
	if err != nil {
		t.Fatalf("基线配置不应失败：%v", err)
	}

	zeroPortListen := validListenEndpoint()
	zeroPortListen.Address = netip.AddrPortFrom(netip.MustParseAddr("0.0.0.0"), 0)
	_, err = core.NewServerConfig(
		core.WithListen(zeroPortListen),
		core.WithWire(core.WireV1),
		core.WithClientCredential(validCredential(testCredentialName)),
		core.WithTCPProxyBinding(validBinding("ssh", testCredentialName)),
	)
	assertSingleErrorCode(t, err, core.CodePortOutOfRange, "listen.address")

	for _, port := range []int{0, 65536} {
		t.Run(fmt.Sprintf("代理入口端口%d", port), func(t *testing.T) {
			binding := validBinding("ssh", testCredentialName)
			binding.RemotePort = port
			_, err := core.NewServerConfig(
				core.WithListen(validListenEndpoint()),
				core.WithWire(core.WireV1),
				core.WithClientCredential(validCredential(testCredentialName)),
				core.WithTCPProxyBinding(binding),
			)
			assertSingleErrorCode(t, err, core.CodePortOutOfRange, "bindings[0].remotePort")
		})
	}
}

// TestNewServerConfigValidationErrors 覆盖规格 §3.4 的服务端错误路径。
func TestNewServerConfigValidationErrors(t *testing.T) {
	invalidListen := validListenEndpoint()
	invalidListen.Address = netip.AddrPort{}
	zeroPortListen := validListenEndpoint()
	zeroPortListen.Address = netip.AddrPortFrom(netip.MustParseAddr("0.0.0.0"), 0)
	emptyTransport := validListenEndpoint()
	emptyTransport.Transport = ""
	unsupportedTransport := validListenEndpoint()
	unsupportedTransport.Transport = "quic"
	unknownClientBinding := validBinding("ssh", "client-b")
	longNameBinding := validBinding(strings.Repeat("a", core.MaxProxyNameLength+1), testCredentialName)
	emptyNameBinding := validBinding("", testCredentialName)
	zeroPortBinding := validBinding("ssh", testCredentialName)
	zeroPortBinding.RemotePort = 0
	negativePortBinding := validBinding("ssh", testCredentialName)
	negativePortBinding.RemotePort = -1

	cases := []struct {
		name    string
		options func() []core.ServerOption
		code    core.ErrorCode
		field   string
	}{
		{
			name: "未提供监听端点",
			options: func() []core.ServerOption {
				return []core.ServerOption{core.WithWire(core.WireV1), core.WithClientCredential(validCredential(testCredentialName))}
			},
			code:  core.CodeIncomplete,
			field: "listen",
		},
		{
			name:    "监听地址为零值",
			options: func() []core.ServerOption { return withServerOptions(core.WithListen(invalidListen)) },
			code:    core.CodeInvalidAddress,
			field:   "listen.address",
		},
		{
			name:    "监听端口为0",
			options: func() []core.ServerOption { return withServerOptions(core.WithListen(zeroPortListen)) },
			code:    core.CodePortOutOfRange,
			field:   "listen.address",
		},
		{
			name:    "传输取值为空",
			options: func() []core.ServerOption { return withServerOptions(core.WithListen(emptyTransport)) },
			code:    core.CodeUnsupportedValue,
			field:   "listen.transport",
		},
		{
			name:    "传输取值不在已交付集合内",
			options: func() []core.ServerOption { return withServerOptions(core.WithListen(unsupportedTransport)) },
			code:    core.CodeUnsupportedValue,
			field:   "listen.transport",
		},
		{
			name:    "wire 取值为空",
			options: func() []core.ServerOption { return withServerOptions(core.WithWire("")) },
			code:    core.CodeUnsupportedValue,
			field:   "wire",
		},
		{
			name:    "wire 取值不在已交付集合内",
			options: func() []core.ServerOption { return withServerOptions(core.WithWire("v2")) },
			code:    core.CodeUnsupportedValue,
			field:   "wire",
		},
		{
			name: "未配置任何客户端凭证",
			options: func() []core.ServerOption {
				return []core.ServerOption{
					core.WithListen(validListenEndpoint()),
					core.WithWire(core.WireV1),
					core.WithTCPProxyBinding(validBinding("ssh", testCredentialName)),
				}
			},
			code:  core.CodeIncomplete,
			field: "credentials",
		},
		{
			name: "凭证客户端标识为空",
			options: func() []core.ServerOption {
				return withServerOptions(core.WithClientCredential(core.ClientCredential{Token: testToken}))
			},
			code:  core.CodeMissingAuth,
			field: "credentials[1].clientID",
		},
		{
			name: "凭证 token 为空",
			options: func() []core.ServerOption {
				return withServerOptions(core.WithClientCredential(core.ClientCredential{ClientID: "client-b"}))
			},
			code:  core.CodeMissingAuth,
			field: "credentials[1].token",
		},
		{
			name:    "绑定引用不存在的客户端",
			options: func() []core.ServerOption { return withServerOptions(core.WithTCPProxyBinding(unknownClientBinding)) },
			code:    core.CodeUnknownClient,
			field:   "bindings[1].clientID",
		},
		{
			name: "绑定引用空客户端标识",
			options: func() []core.ServerOption {
				return withServerOptions(core.WithTCPProxyBinding(validBinding("web", "")))
			},
			code:  core.CodeUnknownClient,
			field: "bindings[1].clientID",
		},
		{
			name:    "绑定名为空",
			options: func() []core.ServerOption { return withServerOptions(core.WithTCPProxyBinding(emptyNameBinding)) },
			code:    core.CodeLimitExceeded,
			field:   "bindings[1].name",
		},
		{
			name:    "绑定名超长",
			options: func() []core.ServerOption { return withServerOptions(core.WithTCPProxyBinding(longNameBinding)) },
			code:    core.CodeLimitExceeded,
			field:   "bindings[1].name",
		},
		{
			name: "绑定重名",
			options: func() []core.ServerOption {
				return withServerOptions(core.WithTCPProxyBinding(validBinding("ssh", testCredentialName)))
			},
			code:  core.CodeDuplicateProxyName,
			field: "bindings[1].name",
		},
		{
			name:    "绑定远程端口为0",
			options: func() []core.ServerOption { return withServerOptions(core.WithTCPProxyBinding(zeroPortBinding)) },
			code:    core.CodePortOutOfRange,
			field:   "bindings[1].remotePort",
		},
		{
			name:    "绑定远程端口为负数",
			options: func() []core.ServerOption { return withServerOptions(core.WithTCPProxyBinding(negativePortBinding)) },
			code:    core.CodePortOutOfRange,
			field:   "bindings[1].remotePort",
		},
		{
			name:    "心跳为负值",
			options: func() []core.ServerOption { return withServerOptions(core.WithServerHeartbeat(-time.Second)) },
			code:    core.CodeInvalidDuration,
			field:   "heartbeat",
		},
		{
			name:    "超时为负值",
			options: func() []core.ServerOption { return withServerOptions(core.WithServerTimeout(-time.Second)) },
			code:    core.CodeInvalidDuration,
			field:   "timeout",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config, err := core.NewServerConfig(testCase.options()...)
			if err == nil {
				t.Fatalf("期望构建失败，实际得到配置 %+v", config)
			}
			if !errors.Is(err, core.ErrConfigInvalid) {
				t.Fatalf("错误未命中哨兵 ErrConfigInvalid：%v", err)
			}

			var configError *core.ConfigError
			if !errors.As(err, &configError) {
				t.Fatalf("errors.As 未取得 *core.ConfigError：%v", err)
			}
			if configError.Code() != testCase.code {
				t.Fatalf("错误码不匹配：实际 %s，期望 %s（消息 %s）", configError.Code(), testCase.code, configError.Error())
			}
			if configError.Field() != testCase.field {
				t.Fatalf("字段路径不匹配：实际 %s，期望 %s", configError.Field(), testCase.field)
			}
			assertNoCredentialLeak(t, err)
		})
	}
}

// TestServerConfigAggregatesAllProblems 覆盖服务端聚合错误与固定顺序。
func TestServerConfigAggregatesAllProblems(t *testing.T) {
	zeroPortListen := validListenEndpoint()
	zeroPortListen.Address = netip.AddrPortFrom(netip.MustParseAddr("0.0.0.0"), 0)
	overLimitBinding := validBinding("ssh", testCredentialName)
	overLimitBinding.RemotePort = 70000

	_, err := core.NewServerConfig(
		core.WithListen(zeroPortListen),
		core.WithWire(""),
		core.WithClientCredential(core.ClientCredential{ClientID: "client-a"}),
		core.WithTCPProxyBinding(overLimitBinding),
		core.WithTCPProxyBinding(validBinding("ssh", testCredentialName)),
		core.WithServerTimeout(-time.Second),
	)
	if err == nil {
		t.Fatal("聚合用例应当构建失败")
	}

	var problems core.ConfigErrors
	if !errors.As(err, &problems) {
		t.Fatalf("errors.As 未取得 core.ConfigErrors：%v", err)
	}

	expected := []struct {
		code  core.ErrorCode
		field string
	}{
		{core.CodePortOutOfRange, "listen.address"},
		{core.CodeUnsupportedValue, "wire"},
		{core.CodeMissingAuth, "credentials[0].token"},
		{core.CodePortOutOfRange, "bindings[0].remotePort"},
		{core.CodeDuplicateProxyName, "bindings[1].name"},
		{core.CodeInvalidDuration, "timeout"},
	}
	if len(problems) != len(expected) {
		t.Fatalf("聚合错误条目数不匹配：实际 %d，期望 %d（%v）", len(problems), len(expected), problems)
	}
	for index, want := range expected {
		if problems[index].Code() != want.code || problems[index].Field() != want.field {
			t.Fatalf("第 %d 条聚合错误不匹配：实际 %s/%s，期望 %s/%s",
				index, problems[index].Code(), problems[index].Field(), want.code, want.field)
		}
	}
	assertNoCredentialLeak(t, err)
}

// TestServerConfigZeroValueValidate 覆盖零值配置调用 Validate 返回错误而非 panic。
func TestServerConfigZeroValueValidate(t *testing.T) {
	var config core.ServerConfig

	err := config.Validate()
	if err == nil {
		t.Fatal("零值配置校验应当失败")
	}
	if !errors.Is(err, core.ErrConfigInvalid) {
		t.Fatalf("错误未命中哨兵 ErrConfigInvalid：%v", err)
	}
	assertNoCredentialLeak(t, err)
}
