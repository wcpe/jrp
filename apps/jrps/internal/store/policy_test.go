package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// 策略默认值必须与规格 §2.3 一致：采集关闭、30 天、5 GiB。
func TestCapturePolicyDefaultsMatchSpec(t *testing.T) {
	database := openServerStore(t, filepath.Join(t.TempDir(), "jrps.db"))

	var policy CapturePolicy
	if err := database.View(context.Background(), func(tx *Tx) error {
		var err error
		policy, err = tx.CapturePolicy()
		return err
	}); err != nil {
		t.Fatalf("读取保留策略失败：%v", err)
	}
	if policy.CaptureEnabled {
		t.Fatal("采集默认必须关闭")
	}
	if policy.RetentionDays != 30 {
		t.Fatalf("保留天数默认应为 30，实际 %d", policy.RetentionDays)
	}
	if policy.MaxTotalBytes != 5<<30 {
		t.Fatalf("总量上限默认应为 5 GiB，实际 %d", policy.MaxTotalBytes)
	}
	if policy.AuditRetentionDays != DefaultAuditRetentionDays {
		t.Fatalf("审计保留天数默认应为 %d，实际 %d", DefaultAuditRetentionDays, policy.AuditRetentionDays)
	}
}

// 合法变更应写入策略并产生一条变更审计。
func TestCapturePolicyUpdateWritesAudit(t *testing.T) {
	database := openInitializedServerStore(t)

	var view CapturePolicyView
	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		var err error
		view, err = tx.UpdateCapturePolicy(ActorAdmin("admin"), CapturePolicyInput{
			CaptureEnabled:     true,
			RetentionDays:      14,
			MaxTotalBytes:      2 << 30,
			AuditRetentionDays: 90,
		})
		return err
	}); err != nil {
		t.Fatalf("更新保留策略失败：%v", err)
	}
	if view.RetentionDays != 14 || !view.CaptureEnabled {
		t.Fatalf("更新后视图不匹配：%+v", view)
	}

	events := mustAuditEvents(t, database)
	assertAuditEvent(t, events, auditExpectation{
		Action:     ActionPolicyUpdate,
		ObjectType: ObjectTypeRetentionPolicy,
		Result:     AuditResultSuccess,
	})
}

// 变更审计须记录前后摘要值，使"策略被改成了什么"可回溯。
func TestCapturePolicyAuditRecordsBeforeAndAfter(t *testing.T) {
	database := openInitializedServerStore(t)

	if err := database.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.UpdateCapturePolicy(ActorAdmin("admin"), CapturePolicyInput{
			CaptureEnabled:     true,
			RetentionDays:      7,
			MaxTotalBytes:      1 << 30,
			AuditRetentionDays: 60,
		})
		return err
	}); err != nil {
		t.Fatalf("更新保留策略失败：%v", err)
	}

	events := mustAuditEvents(t, database)
	latest := events[len(events)-1]
	if !strings.Contains(latest.Context, "30→7") {
		t.Fatalf("审计应记录保留天数变更前后值：%s", latest.Context)
	}
	if !strings.Contains(latest.Context, "关闭→开启") {
		t.Fatalf("审计应记录采集开关变更前后状态：%s", latest.Context)
	}
}

// 越界值必须返回校验错误，且不写入任何变更（FR-16 规格 §2.3）。
func TestCapturePolicyRejectsOutOfRangeValues(t *testing.T) {
	database := openInitializedServerStore(t)
	cases := []struct {
		name  string
		input CapturePolicyInput
	}{
		{"保留天数为零", CapturePolicyInput{RetentionDays: 0, MaxTotalBytes: DefaultMaxTotalBytes, AuditRetentionDays: DefaultAuditRetentionDays}},
		{"保留天数为负", CapturePolicyInput{RetentionDays: -1, MaxTotalBytes: DefaultMaxTotalBytes, AuditRetentionDays: DefaultAuditRetentionDays}},
		{"保留天数超上限", CapturePolicyInput{RetentionDays: MaxRetentionDays + 1, MaxTotalBytes: DefaultMaxTotalBytes, AuditRetentionDays: DefaultAuditRetentionDays}},
		{"总量为零", CapturePolicyInput{RetentionDays: DefaultRetentionDays, MaxTotalBytes: 0, AuditRetentionDays: DefaultAuditRetentionDays}},
		{"总量为负", CapturePolicyInput{RetentionDays: DefaultRetentionDays, MaxTotalBytes: -1, AuditRetentionDays: DefaultAuditRetentionDays}},
		{"总量超上限", CapturePolicyInput{RetentionDays: DefaultRetentionDays, MaxTotalBytes: MaxMaxTotalBytes + 1, AuditRetentionDays: DefaultAuditRetentionDays}},
		{"审计保留天数为零", CapturePolicyInput{RetentionDays: DefaultRetentionDays, MaxTotalBytes: DefaultMaxTotalBytes, AuditRetentionDays: 0}},
		{"审计保留天数超上限", CapturePolicyInput{RetentionDays: DefaultRetentionDays, MaxTotalBytes: DefaultMaxTotalBytes, AuditRetentionDays: MaxAuditRetentionDays + 1}},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			err := database.Transaction(context.Background(), func(tx *Tx) error {
				_, err := tx.UpdateCapturePolicy(ActorAdmin("admin"), item.input)
				return err
			})
			var violationErr PolicyViolationError
			if !errors.As(err, &violationErr) {
				t.Fatalf("越界值应返回策略校验错误：%v", err)
			}
		})
	}

	// 全部尝试失败后，策略必须保持默认值：非法输入不产生部分变更。
	var policy CapturePolicy
	if err := database.View(context.Background(), func(tx *Tx) error {
		var err error
		policy, err = tx.CapturePolicy()
		return err
	}); err != nil {
		t.Fatalf("读取保留策略失败：%v", err)
	}
	if policy.RetentionDays != DefaultRetentionDays || policy.MaxTotalBytes != DefaultMaxTotalBytes {
		t.Fatalf("非法输入不得改动策略：%+v", policy)
	}
}

// 多个字段同时越界时应一次返回全部违规项，而不是只报第一个。
func TestCapturePolicyReportsAllViolations(t *testing.T) {
	violations := ValidateCapturePolicyInput(CapturePolicyInput{
		RetentionDays:      -5,
		MaxTotalBytes:      0,
		AuditRetentionDays: 0,
	})
	if len(violations) != 3 {
		t.Fatalf("应返回三处违规，实际 %d 处", len(violations))
	}
}

// 校验错误不得回显被拒绝的数值，只给允许区间。
func TestPolicyViolationErrorDoesNotEchoInput(t *testing.T) {
	err := policyViolationError([]PolicyViolation{
		{Field: "retentionDays", Detail: rangeDetail("保留天数", MinRetentionDays, MaxRetentionDays)},
	})
	if strings.Contains(err.Error(), "-999999") {
		t.Fatalf("校验错误不应回显非法输入：%v", err)
	}
	if !strings.Contains(err.Error(), "保留天数") {
		t.Fatalf("校验错误应指明字段：%v", err)
	}
}

// 边界值本身必须放行：下限与上限都是合法取值。
func TestCapturePolicyAcceptsBoundaryValues(t *testing.T) {
	database := openInitializedServerStore(t)
	cases := []CapturePolicyInput{
		{RetentionDays: MinRetentionDays, MaxTotalBytes: MinMaxTotalBytes, AuditRetentionDays: MinAuditRetentionDays},
		{RetentionDays: MaxRetentionDays, MaxTotalBytes: MaxMaxTotalBytes, AuditRetentionDays: MaxAuditRetentionDays},
	}
	for _, input := range cases {
		if err := database.Transaction(context.Background(), func(tx *Tx) error {
			_, err := tx.UpdateCapturePolicy(ActorAdmin("admin"), input)
			return err
		}); err != nil {
			t.Fatalf("边界值 %+v 应被接受：%v", input, err)
		}
	}
}
