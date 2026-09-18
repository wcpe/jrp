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

// 测试用常量：token 使用不会与其它文本混淆的字面量，便于断言错误消息脱敏。
const (
	testToken          = "sw0rdf1sh-plaintext-token"
	testCredentialName = "client-a"
)

// validServerEndpoint 返回一个合法的服务端端点。
func validServerEndpoint() core.ServerEndpoint {
	return core.ServerEndpoint{
		Address:   netip.MustParseAddrPort("127.0.0.1:7000"),
		Transport: core.TransportTCP,
		Wire:      core.WireV1,
	}
}

// validTCPProxy 返回一个合法的本地 TCP 代理条目。
func validTCPProxy(name string, remotePort int) core.TCPProxy {
	return core.TCPProxy{
		Name:       name,
		LocalAddr:  netip.MustParseAddrPort("127.0.0.1:22"),
		RemotePort: remotePort,
	}
}

// validClientOptions 返回规格 §3.2 客户端示例对应的选项集合。
func validClientOptions() []core.ClientOption {
	return []core.ClientOption{
		core.WithClientID(testCredentialName),
		core.WithServerEndpoint(validServerEndpoint()),
		core.WithClientAuth(core.TokenAuth{Token: testToken}),
		core.WithTCPProxy(validTCPProxy("ssh", 6000)),
		core.WithHeartbeat(30 * time.Second),
	}
}

// withClientOptions 在合法选项基础上追加额外选项。
func withClientOptions(extra ...core.ClientOption) []core.ClientOption {
	return append(validClientOptions(), extra...)
}

// clientCase 描述一条客户端校验用例。
type clientCase struct {
	name    string
	options func() []core.ClientOption
	code    core.ErrorCode
	field   string
}

// assertClientCodes 跑表驱动用例，断言错误码与字段路径，并确认错误可判定且消息脱敏。
func assertClientCodes(t *testing.T, cases []clientCase) {
	t.Helper()
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config, err := core.NewClientConfig(testCase.options()...)

			if err == nil {
				t.Fatalf("期望构建失败，实际得到配置 %+v", config)
			}
			if errors.Is(err, core.ErrConfigInvalid) == false {
				t.Fatalf("错误未命中哨兵 ErrConfigInvalid：%v", err)
			}

			var configError *core.ConfigError
			if errors.As(err, &configError) == false {
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

// TestNewClientConfigFromSpecExample 覆盖规格 §3.2 的客户端示例与正常路径验收。
func TestNewClientConfigFromSpecExample(t *testing.T) {
	config, err := core.NewClientConfig(
		core.WithClientID("client-a"),
		core.WithServerEndpoint(core.ServerEndpoint{
			Address:   netip.MustParseAddrPort("127.0.0.1:7000"),
			Transport: core.TransportTCP,
			Wire:      core.WireV1,
		}),
		core.WithClientAuth(core.TokenAuth{Token: testToken}),
		core.WithTCPProxy(core.TCPProxy{
			Name:       "ssh",
			LocalAddr:  netip.MustParseAddrPort("127.0.0.1:22"),
			RemotePort: 6000,
		}),
		core.WithHeartbeat(30*time.Second),
	)
	if err != nil {
		t.Fatalf("示例配置应当构建成功：%v", err)
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("示例配置校验应当通过：%v", err)
	}

	if config.ClientID() != "client-a" {
		t.Fatalf("客户端标识不匹配：%s", config.ClientID())
	}
	if config.ServerEndpoint() != validServerEndpoint() {
		t.Fatalf("服务端端点不匹配：%+v", config.ServerEndpoint())
	}
	if config.Auth().Token != testToken {
		t.Fatalf("鉴权材料不匹配：%s", config.Auth().Token)
	}
	if config.Heartbeat() != 30*time.Second {
		t.Fatalf("心跳间隔不匹配：%s", config.Heartbeat())
	}
	if config.Timeout() != core.DefaultTimeout {
		t.Fatalf("未设置超时应采用默认值：%s", config.Timeout())
	}

	proxies := config.Proxies()
	if len(proxies) != 1 {
		t.Fatalf("代理条目数不匹配：%d", len(proxies))
	}
	if proxies[0] != validTCPProxy("ssh", 6000) {
		t.Fatalf("代理条目内容不匹配：%+v", proxies[0])
	}
	if proxies[0].Type() != core.ProxyTypeTCP {
		t.Fatalf("代理类型取值不匹配：%s", proxies[0].Type())
	}
}

// TestClientConfigValidateIsRepeatable 覆盖「Validate 可重复调用且无副作用」。
func TestClientConfigValidateIsRepeatable(t *testing.T) {
	config, err := core.NewClientConfig(validClientOptions()...)
	if err != nil {
		t.Fatalf("构建失败：%v", err)
	}

	for attempt := 0; attempt < 3; attempt++ {
		if err := config.Validate(); err != nil {
			t.Fatalf("第 %d 次校验不应失败：%v", attempt+1, err)
		}
		if len(config.Proxies()) != 1 || config.ClientID() != testCredentialName {
			t.Fatalf("第 %d 次校验后配置值发生变化", attempt+1)
		}
	}
}

// TestNewClientConfigDefaultDurations 覆盖零值时间参数采用 Core 默认常量。
func TestNewClientConfigDefaultDurations(t *testing.T) {
	options := []core.ClientOption{
		core.WithClientID(testCredentialName),
		core.WithServerEndpoint(validServerEndpoint()),
		core.WithClientAuth(core.TokenAuth{Token: testToken}),
		core.WithHeartbeat(0),
		core.WithTimeout(0),
	}

	config, err := core.NewClientConfig(options...)
	if err != nil {
		t.Fatalf("零值时间参数应当构建成功：%v", err)
	}
	if config.Heartbeat() != core.DefaultHeartbeat {
		t.Fatalf("心跳零值未采用默认常量：%s", config.Heartbeat())
	}
	if config.Timeout() != core.DefaultTimeout {
		t.Fatalf("超时零值未采用默认常量：%s", config.Timeout())
	}
}

// TestNewClientConfigProxyCountBoundary 覆盖代理条目数上限与上限加一。
func TestNewClientConfigProxyCountBoundary(t *testing.T) {
	build := func(count int) []core.ClientOption {
		options := validClientOptions()
		for index := 0; index < count; index++ {
			name := fmt.Sprintf("proxy-%d", index)
			options = append(options, core.WithTCPProxy(validTCPProxy(name, 6000)))
		}
		return options
	}

	config, err := core.NewClientConfig(build(core.MaxProxyCount - 1)...)
	if err != nil {
		t.Fatalf("代理条目数恰为上限时应当构建成功：%v", err)
	}
	if len(config.Proxies()) != core.MaxProxyCount {
		t.Fatalf("代理条目数不匹配：%d", len(config.Proxies()))
	}

	if _, err := core.NewClientConfig(build(core.MaxProxyCount)...); err == nil {
		t.Fatal("代理条目数超过上限时应当构建失败")
	} else {
		assertSingleErrorCode(t, err, core.CodeLimitExceeded, "proxies")
	}
}

// TestNewClientConfigProxyNameBoundary 覆盖代理名长度边界。
func TestNewClientConfigProxyNameBoundary(t *testing.T) {
	build := func(name string) core.ClientConfig {
		config, err := core.NewClientConfig(withClientOptions(
			core.WithTCPProxy(validTCPProxy(name, 6001)),
		)...)
		if err != nil {
			t.Fatalf("代理名 %d 字节应当构建成功：%v", len(name), err)
		}
		return config
	}

	if got := len(build("a").Proxies()[1].Name); got != 1 {
		t.Fatalf("单字节代理名长度不匹配：%d", got)
	}
	limit := strings.Repeat("a", core.MaxProxyNameLength)
	if got := len(build(limit).Proxies()[1].Name); got != core.MaxProxyNameLength {
		t.Fatalf("上限长度代理名不匹配：%d", got)
	}

	assertClientCodes(t, []clientCase{
		{
			name:    "空代理名",
			options: func() []core.ClientOption { return withClientOptions(core.WithTCPProxy(validTCPProxy("", 6001))) },
			code:    core.CodeLimitExceeded,
			field:   "proxies[1].name",
		},
		{
			name: "超长代理名",
			options: func() []core.ClientOption {
				return withClientOptions(core.WithTCPProxy(validTCPProxy(strings.Repeat("a", core.MaxProxyNameLength+1), 6001)))
			},
			code:  core.CodeLimitExceeded,
			field: "proxies[1].name",
		},
	})
}

// TestNewClientConfigPortBoundary 覆盖端口 1 与 65535 成功、0 与 65536 失败。
func TestNewClientConfigPortBoundary(t *testing.T) {
	for _, port := range []int{1, 65535} {
		config, err := core.NewClientConfig(withClientOptions(
			core.WithServerEndpoint(core.ServerEndpoint{
				Address:   netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port)),
				Transport: core.TransportTCP,
				Wire:      core.WireV1,
			}),
			core.WithTCPProxy(validTCPProxy("web", port)),
		)...)
		if err != nil {
			t.Fatalf("端口 %d 应当构建成功：%v", port, err)
		}
		if config.Proxies()[1].RemotePort != port {
			t.Fatalf("远程端口不匹配：%d", config.Proxies()[1].RemotePort)
		}
	}

	zeroPort := core.ServerEndpoint{
		Address:   netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), 0),
		Transport: core.TransportTCP,
		Wire:      core.WireV1,
	}
	assertClientCodes(t, []clientCase{
		{
			name:    "端点端口为0",
			options: func() []core.ClientOption { return withClientOptions(core.WithServerEndpoint(zeroPort)) },
			code:    core.CodeInvalidAddress,
			field:   "endpoint.address",
		},
		{
			name:    "远程端口为0",
			options: func() []core.ClientOption { return withClientOptions(core.WithTCPProxy(validTCPProxy("ssh", 0))) },
			code:    core.CodePortOutOfRange,
			field:   "proxies[1].remotePort",
		},
		{
			name:    "远程端口为65536",
			options: func() []core.ClientOption { return withClientOptions(core.WithTCPProxy(validTCPProxy("ssh", 65536))) },
			code:    core.CodePortOutOfRange,
			field:   "proxies[1].remotePort",
		},
		{
			name:    "目标端口为负数",
			options: func() []core.ClientOption { return withClientOptions(core.WithTCPProxy(validTCPProxy("ssh", -1))) },
			code:    core.CodePortOutOfRange,
			field:   "proxies[1].remotePort",
		},
	})
}

// TestNewClientConfigValidationErrors 覆盖规格 §3.4 的客户端错误路径。
func TestNewClientConfigValidationErrors(t *testing.T) {
	assertClientCodes(t, []clientCase{
		{
			name: "未提供服务端端点",
			options: func() []core.ClientOption {
				return []core.ClientOption{
					core.WithClientID(testCredentialName),
					core.WithClientAuth(core.TokenAuth{Token: testToken}),
				}
			},
			code:  core.CodeIncomplete,
			field: "endpoint",
		},
		{
			name:    "端点地址为零值",
			options: func() []core.ClientOption { return withClientOptions(core.WithServerEndpoint(core.ServerEndpoint{})) },
			code:    core.CodeInvalidAddress,
			field:   "endpoint.address",
		},
		{
			name: "端点地址为未指定地址",
			options: func() []core.ClientOption {
				return withClientOptions(core.WithServerEndpoint(core.ServerEndpoint{
					Address:   netip.MustParseAddrPort("0.0.0.0:7000"),
					Transport: core.TransportTCP,
					Wire:      core.WireV1,
				}))
			},
			code:  core.CodeInvalidAddress,
			field: "endpoint.address",
		},
		{
			name: "传输取值为空",
			options: func() []core.ClientOption {
				endpoint := validServerEndpoint()
				endpoint.Transport = ""
				return withClientOptions(core.WithServerEndpoint(endpoint))
			},
			code:  core.CodeUnsupportedValue,
			field: "endpoint.transport",
		},
		{
			name: "传输取值不在已交付集合内",
			options: func() []core.ClientOption {
				endpoint := validServerEndpoint()
				endpoint.Transport = "kcp"
				return withClientOptions(core.WithServerEndpoint(endpoint))
			},
			code:  core.CodeUnsupportedValue,
			field: "endpoint.transport",
		},
		{
			name: "wire 取值为空",
			options: func() []core.ClientOption {
				endpoint := validServerEndpoint()
				endpoint.Wire = ""
				return withClientOptions(core.WithServerEndpoint(endpoint))
			},
			code:  core.CodeUnsupportedValue,
			field: "endpoint.wire",
		},
		{
			name: "wire 取值不在已交付集合内",
			options: func() []core.ClientOption {
				endpoint := validServerEndpoint()
				endpoint.Wire = "v2"
				return withClientOptions(core.WithServerEndpoint(endpoint))
			},
			code:  core.CodeUnsupportedValue,
			field: "endpoint.wire",
		},
		{
			name:    "客户端标识为空",
			options: func() []core.ClientOption { return withClientOptions(core.WithClientID("")) },
			code:    core.CodeMissingAuth,
			field:   "clientID",
		},
		{
			name:    "鉴权材料为空",
			options: func() []core.ClientOption { return withClientOptions(core.WithClientAuth(core.TokenAuth{})) },
			code:    core.CodeMissingAuth,
			field:   "auth.token",
		},
		{
			name: "代理本地地址为零值",
			options: func() []core.ClientOption {
				return withClientOptions(core.WithTCPProxy(core.TCPProxy{Name: "web", RemotePort: 6001}))
			},
			code:  core.CodeInvalidAddress,
			field: "proxies[1].localAddr",
		},
		{
			name: "代理本地地址端口为0",
			options: func() []core.ClientOption {
				return withClientOptions(core.WithTCPProxy(core.TCPProxy{
					Name:       "web",
					LocalAddr:  netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), 0),
					RemotePort: 6001,
				}))
			},
			code:  core.CodeInvalidAddress,
			field: "proxies[1].localAddr",
		},
		{
			name: "代理重名",
			options: func() []core.ClientOption {
				return withClientOptions(core.WithTCPProxy(validTCPProxy("ssh", 6001)))
			},
			code:  core.CodeDuplicateProxyName,
			field: "proxies[1].name",
		},
		{
			name:    "心跳为负值",
			options: func() []core.ClientOption { return withClientOptions(core.WithHeartbeat(-time.Second)) },
			code:    core.CodeInvalidDuration,
			field:   "heartbeat",
		},
		{
			name:    "超时为负值",
			options: func() []core.ClientOption { return withClientOptions(core.WithTimeout(-time.Second)) },
			code:    core.CodeInvalidDuration,
			field:   "timeout",
		},
	})
}

// TestClientConfigZeroValueValidate 覆盖零值配置调用 Validate 返回错误而非 panic。
//
// FR-25 的 Engine 会再执行一次 Validate，用于防止宿主绕过构建器直接零值构造。
func TestClientConfigZeroValueValidate(t *testing.T) {
	var config core.ClientConfig

	err := config.Validate()
	if err == nil {
		t.Fatal("零值配置校验应当失败")
	}
	if !errors.Is(err, core.ErrConfigInvalid) {
		t.Fatalf("错误未命中哨兵 ErrConfigInvalid：%v", err)
	}

	var problems core.ConfigErrors
	if !errors.As(err, &problems) {
		t.Fatalf("errors.As 未取得 core.ConfigErrors：%v", err)
	}
	assertHasErrorCode(t, problems, core.CodeIncomplete, "endpoint")
	assertHasErrorCode(t, problems, core.CodeMissingAuth, "clientID")
	assertNoCredentialLeak(t, err)
}

// TestNewClientConfigAggregatesAllProblems 覆盖「聚合错误中包含全部三类问题」。
func TestNewClientConfigAggregatesAllProblems(t *testing.T) {
	_, err := core.NewClientConfig(
		core.WithClientID(""),
		core.WithServerEndpoint(validServerEndpoint()),
		core.WithClientAuth(core.TokenAuth{}),
		core.WithTCPProxy(validTCPProxy("ssh", 70000)),
		core.WithTCPProxy(validTCPProxy("ssh", 6001)),
	)
	if err == nil {
		t.Fatal("聚合用例应当构建失败")
	}

	var problems core.ConfigErrors
	if errors.As(err, &problems) == false {
		t.Fatalf("errors.As 未取得 core.ConfigErrors：%v", err)
	}
	if len(problems) < 4 {
		t.Fatalf("聚合错误条目数不足：%d（%v）", len(problems), problems)
	}

	assertHasErrorCode(t, problems, core.CodeMissingAuth, "clientID")
	assertHasErrorCode(t, problems, core.CodeMissingAuth, "auth.token")
	assertHasErrorCode(t, problems, core.CodePortOutOfRange, "proxies[0].remotePort")
	assertHasErrorCode(t, problems, core.CodeDuplicateProxyName, "proxies[1].name")
	assertNoCredentialLeak(t, err)
}
