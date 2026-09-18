package core_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/wcpe/jrp/core"
)

// TestConfigErrorExposesStableFields 覆盖错误模型的可判定字段与脱敏消息。
func TestConfigErrorExposesStableFields(t *testing.T) {
	options := withClientOptions(core.WithClientAuth(core.TokenAuth{}))

	_, err := core.NewClientConfig(options...)
	if err == nil {
		t.Fatal("空鉴权材料应当构建失败")
	}
	if !errors.Is(err, core.ErrConfigInvalid) {
		t.Fatalf("错误未命中哨兵 ErrConfigInvalid：%v", err)
	}

	var problems core.ConfigErrors
	if !errors.As(err, &problems) {
		t.Fatalf("errors.As 未取得 core.ConfigErrors：%v", err)
	}

	var configError *core.ConfigError
	if !errors.As(err, &configError) {
		t.Fatalf("errors.As 未取得 *core.ConfigError：%v", err)
	}
	if configError.Code() != core.CodeMissingAuth {
		t.Fatalf("错误码不匹配：%s", configError.Code())
	}
	if configError.Field() != "auth.token" {
		t.Fatalf("字段路径不匹配：%s", configError.Field())
	}
	if strings.TrimSpace(configError.Message()) == "" {
		t.Fatal("错误消息为空")
	}
	if configError.Unwrap() != core.ErrConfigInvalid {
		t.Fatalf("Unwrap 未返回哨兵：%v", configError.Unwrap())
	}
	if !strings.Contains(configError.Error(), "auth.token") {
		t.Fatalf("错误消息未包含字段路径：%s", configError.Error())
	}
	if !strings.Contains(configError.Error(), configError.Message()) {
		t.Fatalf("错误消息未包含 Message 内容：%s", configError.Error())
	}
}

// TestErrConfigInvalidSentinel 覆盖哨兵不匹配其它错误。
func TestErrConfigInvalidSentinel(t *testing.T) {
	if errors.Is(errors.New("无关错误"), core.ErrConfigInvalid) {
		t.Fatal("无关错误不应命中哨兵 ErrConfigInvalid")
	}
	if core.ErrConfigInvalid == nil {
		t.Fatal("哨兵 ErrConfigInvalid 不得为 nil")
	}
	if strings.TrimSpace(core.ErrConfigInvalid.Error()) == "" {
		t.Fatal("哨兵错误消息为空")
	}
}

// TestConfigErrorsMessageMentionsAllFields 覆盖聚合错误消息包含全部字段路径。
func TestConfigErrorsMessageMentionsAllFields(t *testing.T) {
	_, err := core.NewClientConfig(
		core.WithClientID(testCredentialName),
		core.WithServerEndpoint(validServerEndpoint()),
		core.WithClientAuth(core.TokenAuth{}),
		core.WithTCPProxy(validTCPProxy("ssh", 70000)),
	)
	if err == nil {
		t.Fatal("用例应当构建失败")
	}

	var problems core.ConfigErrors
	if !errors.As(err, &problems) {
		t.Fatalf("errors.As 未取得 core.ConfigErrors：%v", err)
	}
	if len(problems) != 2 {
		t.Fatalf("聚合错误条目数不匹配：%d（%v）", len(problems), problems)
	}

	message := problems.Error()
	for _, field := range []string{"auth.token", "proxies[0].remotePort"} {
		if !strings.Contains(message, field) {
			t.Fatalf("聚合错误消息缺少字段路径 %s：%s", field, message)
		}
	}
	if problems[0] == problems[1] {
		t.Fatal("聚合错误条目不应为同一指针")
	}
	assertNoCredentialLeak(t, err)
}

// TestErrorCodeValues 覆盖错误码为稳定的机器可读取值。
func TestErrorCodeValues(t *testing.T) {
	codes := map[core.ErrorCode]string{
		core.CodeIncomplete:         "配置不完整",
		core.CodePortOutOfRange:     "端口越界",
		core.CodeMissingAuth:        "缺少鉴权信息",
		core.CodeDuplicateProxyName: "代理名重复",
		core.CodeInvalidAddress:     "地址非法",
		core.CodeUnsupportedValue:   "取值不受支持",
		core.CodeUnknownClient:      "客户端不存在",
		core.CodeInvalidDuration:    "时间参数非法",
		core.CodeLimitExceeded:      "超出上限",
	}

	seen := make(map[core.ErrorCode]bool, len(codes))
	for code, label := range codes {
		if strings.TrimSpace(string(code)) == "" {
			t.Fatalf("%s 的取值不得为空", label)
		}
		if seen[code] {
			t.Fatalf("错误码取值重复：%s", code)
		}
		seen[code] = true
	}
	if len(seen) != 9 {
		t.Fatalf("错误码数量不匹配：%d", len(seen))
	}
}
