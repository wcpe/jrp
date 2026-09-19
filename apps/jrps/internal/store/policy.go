package store

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

// 保留策略的默认值与边界（FR-16 规格 §2.3；§6 待定项在实现前定稿）。
//
// 上下限存在的意义是让非法值可以被明确拒绝，而不是静默收敛为某个"合理值"：
// 规格要求越界返回 400 问题详情，不写入部分变更。
const (
	DefaultRetentionDays = 30
	MinRetentionDays     = 1
	MaxRetentionDays     = 365

	DefaultMaxTotalBytes int64 = 5 << 30 // 5 GiB
	MinMaxTotalBytes     int64 = 1 << 20 // 1 MiB
	MaxMaxTotalBytes     int64 = 1 << 40 // 1 TiB

	DefaultAuditRetentionDays = 180
	MinAuditRetentionDays     = 30
	MaxAuditRetentionDays     = 3650
)

// DefaultCapturePolicy 返回规格规定的默认策略：采集关闭、正文保留 30 天、上限 5 GiB。
func DefaultCapturePolicy() CapturePolicy {
	return CapturePolicy{
		CaptureEnabled:     false,
		RetentionDays:      DefaultRetentionDays,
		MaxTotalBytes:      DefaultMaxTotalBytes,
		AuditRetentionDays: DefaultAuditRetentionDays,
	}
}

// CapturePolicyInput 是策略变更的输入。
type CapturePolicyInput struct {
	CaptureEnabled     bool
	RetentionDays      int
	MaxTotalBytes      int64
	AuditRetentionDays int
}

// PolicyViolation 描述一处策略校验失败；调用方据此生成 400 问题详情。
type PolicyViolation struct {
	Field  string
	Detail string
}

// ValidateCapturePolicyInput 校验策略输入，返回全部违规项而不是遇到首个就返回：
// 管理员一次提交多个越界值时应当一次看全，不必反复试错。
func ValidateCapturePolicyInput(input CapturePolicyInput) []PolicyViolation {
	var violations []PolicyViolation
	if input.RetentionDays < MinRetentionDays || input.RetentionDays > MaxRetentionDays {
		violations = append(violations, PolicyViolation{
			Field:  "retentionDays",
			Detail: rangeDetail("保留天数", MinRetentionDays, MaxRetentionDays),
		})
	}
	if input.MaxTotalBytes < MinMaxTotalBytes || input.MaxTotalBytes > MaxMaxTotalBytes {
		violations = append(violations, PolicyViolation{
			Field:  "maxTotalBytes",
			Detail: rangeDetail("正文总量上限", MinMaxTotalBytes, MaxMaxTotalBytes),
		})
	}
	if input.AuditRetentionDays < MinAuditRetentionDays || input.AuditRetentionDays > MaxAuditRetentionDays {
		violations = append(violations, PolicyViolation{
			Field:  "auditRetentionDays",
			Detail: rangeDetail("审计保留天数", MinAuditRetentionDays, MaxAuditRetentionDays),
		})
	}
	return violations
}

// rangeDetail 生成中文区间说明；越界值本身不回显，避免把非法输入带进日志与响应。
func rangeDetail(name string, min int64, max int64) string {
	return name + "必须在 " + strconv.FormatInt(min, 10) + " 至 " + strconv.FormatInt(max, 10) + " 之间"
}

// CapturePolicyView 是策略读取视图。
type CapturePolicyView struct {
	CaptureEnabled     bool
	RetentionDays      int
	MaxTotalBytes      int64
	AuditRetentionDays int
	UpdatedAt          time.Time
}

// View 把模型转换为读取视图。
func (policy CapturePolicy) View() CapturePolicyView {
	return CapturePolicyView{
		CaptureEnabled:     policy.CaptureEnabled,
		RetentionDays:      policy.RetentionDays,
		MaxTotalBytes:      policy.MaxTotalBytes,
		AuditRetentionDays: policy.AuditRetentionDays,
		UpdatedAt:          policy.UpdatedAt,
	}
}

// ErrCapturePolicyMissing 表示策略对象缺失；正常迁移后不应出现。
var ErrCapturePolicyMissing = errors.New("保留策略尚未初始化")

// CapturePolicy 读取当前保留策略。
//
// 策略是单例对象，迁移时已写入默认值；缺失说明数据库被外部改动过，属异常状态，
// 不静默以默认值顶替——那会让管理员以为策略生效，实际读到的不是落库值。
func (tx *Tx) CapturePolicy() (CapturePolicy, error) {
	var policy CapturePolicy
	err := tx.db.Where("id = ?", capturePolicyRowID).First(&policy).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return CapturePolicy{}, ErrCapturePolicyMissing
	}
	if err != nil {
		return CapturePolicy{}, fmt.Errorf("读取保留策略失败：%w", translateSQLError(err))
	}
	return policy, nil
}

// UpdateCapturePolicy 校验并写入保留策略，同时写入变更审计。
//
// 校验在写入之前完成并返回全部违规项：非法输入不得产生部分变更，也不得静默
// 收敛为默认值（FR-16 规格 §2.3）。审计记录变更前后的摘要值，使"策略被谁改成
// 了什么"可回溯。
func (tx *Tx) UpdateCapturePolicy(actor Actor, input CapturePolicyInput) (CapturePolicyView, error) {
	if violations := ValidateCapturePolicyInput(input); len(violations) > 0 {
		return CapturePolicyView{}, policyViolationError(violations)
	}
	updated := CapturePolicy{
		ID:                 capturePolicyRowID,
		CaptureEnabled:     input.CaptureEnabled,
		RetentionDays:      input.RetentionDays,
		MaxTotalBytes:      input.MaxTotalBytes,
		AuditRetentionDays: input.AuditRetentionDays,
		UpdatedAt:          time.Now().UTC(),
	}
	err := tx.Transaction(func() error {
		previous, err := tx.CapturePolicy()
		if err != nil {
			return err
		}
		if err := tx.db.Model(&CapturePolicy{}).Where("id = ?", capturePolicyRowID).
			Updates(map[string]any{
				"capture_enabled":      input.CaptureEnabled,
				"retention_days":       input.RetentionDays,
				"max_total_bytes":      input.MaxTotalBytes,
				"audit_retention_days": input.AuditRetentionDays,
				"updated_at":           updated.UpdatedAt,
			}).Error; err != nil {
			return fmt.Errorf("写入保留策略失败：%w", translateSQLError(err))
		}
		return tx.writeAudit(AuditEvent{
			ActorType:  actor.Type,
			ActorID:    actor.ID,
			Action:     ActionPolicyUpdate,
			ObjectType: ObjectTypeRetentionPolicy,
			ObjectID:   "capture",
			Result:     AuditResultSuccess,
			Context:    policyChangeSummary(previous, updated),
		})
	})
	if err != nil {
		return CapturePolicyView{}, err
	}
	return updated.View(), nil
}

// policyChangeSummary 生成策略变更的中文摘要，只含数值与开关状态，不含任何秘密。
func policyChangeSummary(previous CapturePolicy, updated CapturePolicy) string {
	return fmt.Sprintf("保留策略已更新：采集 %s→%s，正文保留 %d→%d 天，总量上限 %d→%d 字节，审计保留 %d→%d 天",
		enabledText(previous.CaptureEnabled), enabledText(updated.CaptureEnabled),
		previous.RetentionDays, updated.RetentionDays,
		previous.MaxTotalBytes, updated.MaxTotalBytes,
		previous.AuditRetentionDays, updated.AuditRetentionDays)
}

// enabledText 把开关状态转为中文，避免摘要里出现 true/false。
func enabledText(enabled bool) string {
	if enabled {
		return "开启"
	}
	return "关闭"
}

// PolicyViolationError 携带全部违规项，供 HTTP 层生成 400 问题详情。
type PolicyViolationError struct {
	Violations []PolicyViolation
}

func (err PolicyViolationError) Error() string {
	details := make([]string, 0, len(err.Violations))
	for _, violation := range err.Violations {
		details = append(details, violation.Field+"："+violation.Detail)
	}
	return "保留策略校验未通过：" + strings.Join(details, "；")
}

// policyViolationError 构造策略校验错误。
func policyViolationError(violations []PolicyViolation) error {
	return PolicyViolationError{Violations: violations}
}
