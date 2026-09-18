package core_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/wcpe/jrp/core"
)

// assertSingleErrorCode 断言错误中恰好包含一条指定错误码与字段路径的问题。
func assertSingleErrorCode(t *testing.T, err error, code core.ErrorCode, field string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望构建失败并返回 %s/%s，实际成功", code, field)
	}
	if !errors.Is(err, core.ErrConfigInvalid) {
		t.Fatalf("错误未命中哨兵 ErrConfigInvalid：%v", err)
	}

	var problems core.ConfigErrors
	if !errors.As(err, &problems) {
		t.Fatalf("errors.As 未取得 core.ConfigErrors：%v", err)
	}
	if len(problems) != 1 {
		t.Fatalf("期望恰好一条问题，实际 %d 条：%v", len(problems), problems)
	}
	if problems[0].Code() != code || problems[0].Field() != field {
		t.Fatalf("问题不匹配：实际 %s/%s，期望 %s/%s", problems[0].Code(), problems[0].Field(), code, field)
	}
}

// assertHasErrorCode 断言聚合错误中存在指定错误码与字段路径的问题。
func assertHasErrorCode(t *testing.T, problems core.ConfigErrors, code core.ErrorCode, field string) {
	t.Helper()
	for _, problem := range problems {
		if problem.Code() == code && problem.Field() == field {
			return
		}
	}
	t.Fatalf("聚合错误中缺少 %s/%s：%v", code, field, problems)
}

// assertNoCredentialLeak 断言错误消息不含凭证原文与调用栈噪音。
func assertNoCredentialLeak(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	message := err.Error()
	if strings.Contains(message, testToken) {
		t.Fatalf("错误消息回显了 token 原文：%s", message)
	}
	if strings.TrimSpace(message) == "" {
		t.Fatal("错误消息为空")
	}
	lostCredential := "credential-lost-should-not-appear"
	if strings.Contains(message, lostCredential) {
		t.Fatalf("错误消息回显了凭证原文：%s", message)
	}
}
