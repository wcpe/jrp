package store

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"gorm.io/gorm"
)

// 通知目标标识的随机字节数；与客户端 token 同为不可猜测的不透明标识。
const notificationTargetIDBytes = 16

// 目标字段的边界。
const (
	maxTargetNameLength         = 128
	maxTargetSecretLength       = 255
	maxWebhookURLLength         = 512
	maxSMTPHostLength           = 255
	maxSMTPAddressLength        = 255
	maxRecipientsPerTarget      = 20
	maxEnabledTargets           = 50
	allowedSMTPSecurityStartTLS = "starttls"
	allowedSMTPSecurityNone     = "none"
	// defaultSMTPPort 在端口缺省时使用：587 是提交端口，配合 STARTTLS。
	defaultSMTPPort = 587
)

// ErrNotificationTargetMissing 表示目标不存在。
var ErrNotificationTargetMissing = errors.New("通知目标不存在")

// NotificationTargetInput 是目标创建与更新的输入。
//
// 秘密只在此处接收，读取一律掩码（FR-15 §3.5）。更新时秘密为空表示
// 保留原有秘密，避免管理员为了改个名字而重新输入密码。
type NotificationTargetInput struct {
	Name       string
	Type       string
	Enabled    bool
	Secret     string
	WebhookURL string
	SMTPHost   string
	SMTPPort   int
	SMTPFrom   string
	SMTPTo     []string
	// SMTPSecurity 为空时由写入侧取 starttls。
	SMTPSecurity string

	// EnabledProvided 表示调用方是否显式给出了启用状态。
	//
	// 更新时未提供应沿用原值：`Enabled` 的假零值无法区分"显式停用"与"没提这一项"，
	// 而按默认值处理会在 PATCH 只改名字时把已停用的目标静默重新启用——那与
	// "停用即不接收通知"的语义直接冲突。创建路径不需要该字段（未提供即启用）。
	EnabledProvided bool
}

// NotificationTargetInputViolation 描述一处目标校验失败。
type NotificationTargetInputViolation struct {
	Field  string
	Detail string
}

// NotificationTargetValidationError 携带全部违规项。
type NotificationTargetValidationError struct {
	Violations []NotificationTargetInputViolation
}

func (err NotificationTargetValidationError) Error() string {
	details := make([]string, 0, len(err.Violations))
	for _, violation := range err.Violations {
		details = append(details, violation.Field+"："+violation.Detail)
	}
	return "通知目标校验未通过：" + strings.Join(details, "；")
}

// NotificationTargetView 是目标的读取视图；秘密只以掩码出现。
type NotificationTargetView struct {
	ID           string
	Name         string
	Type         string
	Enabled      bool
	MaskedSecret string
	Summary      string
	WebhookURL   string
	SMTPHost     string
	SMTPPort     int
	SMTPFrom     string
	SMTPTo       []string
	SMTPSecurity string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// NewNotificationTargetID 生成不可猜测的目标标识。
func NewNotificationTargetID() (string, error) {
	buffer := make([]byte, notificationTargetIDBytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("生成通知目标标识失败：%w", err)
	}
	return "nt_" + base64.RawURLEncoding.EncodeToString(buffer), nil
}

// ValidateNotificationTargetInput 校验目标输入，返回全部违规项。
//
// 一次返回全部而不是遇到首个就停：管理员提交的表单应一次看全所有问题。
func ValidateNotificationTargetInput(input NotificationTargetInput) []NotificationTargetInputViolation {
	var violations []NotificationTargetInputViolation
	appendViolation := func(field, detail string) {
		violations = append(violations, NotificationTargetInputViolation{Field: field, Detail: detail})
	}

	name := strings.TrimSpace(input.Name)
	if name == "" {
		appendViolation("name", "名称不能为空")
	} else if len([]rune(name)) > maxTargetNameLength {
		appendViolation("name", "名称超出长度上限")
	}
	if input.Secret != "" && len(input.Secret) > maxTargetSecretLength {
		appendViolation("secret", "秘密超出长度上限")
	}

	switch input.Type {
	case NotificationTypeWebhook:
		violations = append(violations, validateWebhookInput(input)...)
	case NotificationTypeEmail:
		violations = append(violations, validateEmailInput(input)...)
	default:
		appendViolation("type", "渠道类型只能是 webhook 或 email")
	}
	return violations
}

// validateWebhookInput 校验 Webhook 目标的必填字段。
func validateWebhookInput(input NotificationTargetInput) []NotificationTargetInputViolation {
	var violations []NotificationTargetInputViolation
	rawURL := strings.TrimSpace(input.WebhookURL)
	if rawURL == "" {
		return []NotificationTargetInputViolation{{Field: "webhookUrl", Detail: "Webhook 地址不能为空"}}
	}
	if len(rawURL) > maxWebhookURLLength {
		return []NotificationTargetInputViolation{{Field: "webhookUrl", Detail: "Webhook 地址超出长度上限"}}
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return []NotificationTargetInputViolation{{Field: "webhookUrl", Detail: "Webhook 地址格式不合法"}}
	}
	if parsed.Scheme != "https" {
		violations = append(violations, NotificationTargetInputViolation{
			Field: "webhookUrl", Detail: "Webhook 地址必须使用 HTTPS 协议",
		})
	}
	if parsed.Hostname() == "" {
		violations = append(violations, NotificationTargetInputViolation{
			Field: "webhookUrl", Detail: "Webhook 地址缺少主机名",
		})
	}
	if parsed.User != nil {
		violations = append(violations, NotificationTargetInputViolation{
			Field: "webhookUrl", Detail: "Webhook 地址不得内嵌凭据",
		})
	}
	return violations
}

// validateEmailInput 校验邮件目标的必填字段。
func validateEmailInput(input NotificationTargetInput) []NotificationTargetInputViolation {
	var violations []NotificationTargetInputViolation
	host := strings.TrimSpace(input.SMTPHost)
	if host == "" {
		violations = append(violations, NotificationTargetInputViolation{Field: "smtpHost", Detail: "SMTP 主机不能为空"})
	} else if len(host) > maxSMTPHostLength {
		violations = append(violations, NotificationTargetInputViolation{Field: "smtpHost", Detail: "SMTP 主机超出长度上限"})
	}
	if input.SMTPPort < 0 || input.SMTPPort > 65535 {
		violations = append(violations, NotificationTargetInputViolation{Field: "smtpPort", Detail: "SMTP 端口必须在 1 至 65535 之间"})
	}
	from := strings.TrimSpace(input.SMTPFrom)
	if from == "" {
		violations = append(violations, NotificationTargetInputViolation{Field: "smtpFrom", Detail: "发件人不能为空"})
	} else if len(from) > maxSMTPAddressLength {
		violations = append(violations, NotificationTargetInputViolation{Field: "smtpFrom", Detail: "发件人超出长度上限"})
	}
	if len(input.SMTPTo) == 0 {
		violations = append(violations, NotificationTargetInputViolation{Field: "smtpTo", Detail: "至少需要一个收件人"})
	} else if len(input.SMTPTo) > maxRecipientsPerTarget {
		violations = append(violations, NotificationTargetInputViolation{
			Field: "smtpTo", Detail: fmt.Sprintf("收件人数量超出上限 %d", maxRecipientsPerTarget),
		})
	}
	for _, recipient := range input.SMTPTo {
		if strings.TrimSpace(recipient) == "" {
			violations = append(violations, NotificationTargetInputViolation{Field: "smtpTo", Detail: "收件人不能为空"})
			break
		}
		if len(recipient) > maxSMTPAddressLength {
			violations = append(violations, NotificationTargetInputViolation{Field: "smtpTo", Detail: "收件人超出长度上限"})
			break
		}
	}
	switch input.SMTPSecurity {
	case "":
		// 缺省时由写入侧取 starttls。
	case allowedSMTPSecurityStartTLS, allowedSMTPSecurityNone:
	default:
		violations = append(violations, NotificationTargetInputViolation{
			Field: "smtpSecurity", Detail: "安全传输选项只能是 starttls 或 none",
		})
	}
	return violations
}

// targetSummary 生成脱敏的目标摘要，用于列表展示。
//
// Webhook 只显示协议与主机，不显示路径与查询串：路径常含 token，
// 查询串更可能直接是凭据（规格 §3.5 的"隐藏凭据部分"）。
func targetSummary(input NotificationTargetInput) string {
	if input.Type == NotificationTypeWebhook {
		parsed, err := url.Parse(strings.TrimSpace(input.WebhookURL))
		if err != nil || parsed.Host == "" {
			return "Webhook 目标"
		}
		return "Webhook " + parsed.Host
	}
	host := strings.TrimSpace(input.SMTPHost)
	if host == "" {
		return "邮件目标"
	}
	return fmt.Sprintf("邮件 %s（%d 个收件人）", host, len(input.SMTPTo))
}

// CreateNotificationTarget 创建目标并写入审计。
func (tx *Tx) CreateNotificationTarget(actor Actor, input NotificationTargetInput) (NotificationTargetView, error) {
	if violations := ValidateNotificationTargetInput(input); len(violations) > 0 {
		return NotificationTargetView{}, NotificationTargetValidationError{Violations: violations}
	}
	id, err := NewNotificationTargetID()
	if err != nil {
		return NotificationTargetView{}, err
	}
	record, err := buildNotificationTarget(id, input)
	if err != nil {
		return NotificationTargetView{}, err
	}
	err = tx.Transaction(func() error {
		var count int64
		if err := tx.db.Model(&NotificationTarget{}).Count(&count).Error; err != nil {
			return fmt.Errorf("统计通知目标失败：%w", translateSQLError(err))
		}
		if count >= maxEnabledTargets {
			return fmt.Errorf("通知目标数量已达上限 %d", maxEnabledTargets)
		}
		// 用 map 写入而不是 Create(&record)：GORM 对带 default 标签的零值字段
		// 会改用数据库默认值，而 Enabled 的零值恰是 false，导致显式的"停用"
		// 被 default:true 覆盖。map 写入不做这层推断，显式值如实落库。
		if err := tx.db.Model(&NotificationTarget{}).Create(notificationTargetValues(record)).Error; err != nil {
			return fmt.Errorf("写入通知目标失败：%w", translateSQLError(err))
		}
		return tx.writeAudit(AuditEvent{
			ActorType:  actor.Type,
			ActorID:    actor.ID,
			Action:     ActionNotificationTargetCreate,
			ObjectType: ObjectTypeNotificationMsg,
			ObjectID:   record.ID,
			Result:     AuditResultSuccess,
			// 上下文只含标识与渠道，不含地址与秘密。
			Context: "创建通知目标（渠道 " + record.Type + "，秘密已掩码）",
		})
	})
	if err != nil {
		return NotificationTargetView{}, err
	}
	return viewFromNotificationTarget(record), nil
}

// UpdateNotificationTarget 更新目标并写入审计。
//
// 秘密为空表示保留原值：管理员改名字或改启用状态时不必重新输入密码，
// 也避免"未填写即清空"导致目标静默失效。
func (tx *Tx) UpdateNotificationTarget(actor Actor, id string, input NotificationTargetInput) (NotificationTargetView, error) {
	if violations := ValidateNotificationTargetInput(input); len(violations) > 0 {
		return NotificationTargetView{}, NotificationTargetValidationError{Violations: violations}
	}
	var updated NotificationTarget
	err := tx.Transaction(func() error {
		var existing NotificationTarget
		if err := tx.db.Where("id = ?", id).First(&existing).Error; err != nil {
			return fmt.Errorf("%w：%s", ErrNotificationTargetMissing, id)
		}
		replacement, err := buildNotificationTarget(id, input)
		if err != nil {
			return err
		}
		if input.Secret == "" {
			replacement.Secret = existing.Secret
		}
		// 未显式给出启用状态时沿用原值：按默认值处理会把已停用的目标静默重新启用。
		if !input.EnabledProvided {
			replacement.Enabled = existing.Enabled
		}
		replacement.CreatedAt = existing.CreatedAt
		replacement.UpdatedAt = time.Now().UTC()
		values := notificationTargetValues(replacement)
		// 更新不改写主键与创建时间：它们是记录身份，不属于本次变更的内容。
		delete(values, "id")
		delete(values, "created_at")
		if err := tx.db.Model(&NotificationTarget{}).Where("id = ?", id).
			Updates(values).Error; err != nil {
			return fmt.Errorf("更新通知目标失败：%w", translateSQLError(err))
		}
		updated = replacement
		if err := writeTargetAudit(tx, actor, ActionNotificationTargetUpdate, id,
			"更新通知目标（渠道 "+replacement.Type+"，秘密已掩码）"); err != nil {
			return err
		}
		// 目标被停用时，其待发送记录不再有投递意义。
		if !replacement.Enabled {
			return tx.discardForDisabledTarget(id)
		}
		return nil
	})
	if err != nil {
		return NotificationTargetView{}, err
	}
	return viewFromNotificationTarget(updated), nil
}

// DeleteNotificationTarget 删除目标并写入审计。
//
// 删除前先把在途记录转入 discarded：不这样做会让这些记录继续重试，
// 最终以"投递失败"的面貌出现在运维视野里，而真实原因是目标已不存在
// （规格 §3.6 要求不得静默丢失）。
func (tx *Tx) DeleteNotificationTarget(actor Actor, id string) error {
	return tx.Transaction(func() error {
		var existing NotificationTarget
		if err := tx.db.Where("id = ?", id).First(&existing).Error; err != nil {
			return fmt.Errorf("%w：%s", ErrNotificationTargetMissing, id)
		}
		if err := tx.discardForDisabledTarget(id); err != nil {
			return err
		}
		if err := tx.db.Where("id = ?", id).Delete(&NotificationTarget{}).Error; err != nil {
			return fmt.Errorf("删除通知目标失败：%w", translateSQLError(err))
		}
		return writeTargetAudit(tx, actor, ActionNotificationTargetDelete, id,
			"删除通知目标（渠道 "+existing.Type+"）")
	})
}

// discardForDisabledTarget 把目标的可投递记录转入 discarded。
func (tx *Tx) discardForDisabledTarget(id string) error {
	if _, err := tx.DiscardOutboxForTarget(id); err != nil {
		return err
	}
	return nil
}

// writeTargetAudit 写入通知目标相关的审计事件。
func writeTargetAudit(tx *Tx, actor Actor, action, id, context string) error {
	return tx.writeAudit(AuditEvent{
		ActorType:  actor.Type,
		ActorID:    actor.ID,
		Action:     action,
		ObjectType: ObjectTypeNotificationMsg,
		ObjectID:   id,
		Result:     AuditResultSuccess,
		Context:    context,
	})
}

// notificationTargetValues 把目标记录展开为列名到值的映射。
//
// 创建与更新共用同一份字段清单，避免两处各抄一遍而在增删字段时漂移。
//
// 用 map 而非结构体写入的原因：GORM 对带 default 标签的零值字段会改用数据库
// 默认值。Enabled 的默认值是 true 而 bool 零值是 false，两者在结构体写入路径
// 上不可区分——管理员显式停用的目标会被静默改成启用。实测确认 Select("*")
// 与 Select(指定列) 都不能避免该行为，只有 map 写入如实落库。
func notificationTargetValues(record NotificationTarget) map[string]any {
	return map[string]any{
		"id":             record.ID,
		"name":           record.Name,
		"type":           record.Type,
		"target_summary": record.TargetSummary,
		"secret":         record.Secret,
		"enabled":        record.Enabled,
		"webhook_url":    record.WebhookURL,
		"smtp_host":      record.SMTPHost,
		"smtp_port":      record.SMTPPort,
		"smtp_from":      record.SMTPFrom,
		"smtp_to":        record.SMTPTo,
		"smtp_security":  record.SMTPSecurity,
		"created_at":     record.CreatedAt,
		"updated_at":     record.UpdatedAt,
	}
}

// buildNotificationTarget 由输入构造目标记录。
func buildNotificationTarget(id string, input NotificationTargetInput) (NotificationTarget, error) {
	record := NotificationTarget{
		ID:            id,
		Name:          strings.TrimSpace(input.Name),
		Type:          input.Type,
		TargetSummary: targetSummary(input),
		Secret:        input.Secret,
		Enabled:       input.Enabled,
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	switch input.Type {
	case NotificationTypeWebhook:
		record.WebhookURL = strings.TrimSpace(input.WebhookURL)
	case NotificationTypeEmail:
		record.SMTPHost = strings.TrimSpace(input.SMTPHost)
		record.SMTPPort = input.SMTPPort
		if record.SMTPPort == 0 {
			record.SMTPPort = defaultSMTPPort
		}
		record.SMTPFrom = strings.TrimSpace(input.SMTPFrom)
		record.SMTPTo = strings.Join(trimRecipients(input.SMTPTo), ",")
		record.SMTPSecurity = input.SMTPSecurity
		if record.SMTPSecurity == "" {
			record.SMTPSecurity = allowedSMTPSecurityStartTLS
		}
	default:
		return NotificationTarget{}, fmt.Errorf("未知的通知渠道类型：%s", input.Type)
	}
	return record, nil
}

// trimRecipients 清理收件人列表中的空白项。
func trimRecipients(recipients []string) []string {
	cleaned := make([]string, 0, len(recipients))
	for _, recipient := range recipients {
		if trimmed := strings.TrimSpace(recipient); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	return cleaned
}

// NotificationTargets 返回全部目标的脱敏视图。
func (tx *Tx) NotificationTargets() ([]NotificationTargetView, error) {
	var records []NotificationTarget
	if err := tx.db.Order("created_at ASC, id ASC").Find(&records).Error; err != nil {
		return nil, fmt.Errorf("读取通知目标失败：%w", translateSQLError(err))
	}
	views := make([]NotificationTargetView, 0, len(records))
	for _, record := range records {
		views = append(views, viewFromNotificationTarget(record))
	}
	return views, nil
}

// NotificationTargetByID 按标识返回单个目标的脱敏视图。
func (tx *Tx) NotificationTargetByID(id string) (NotificationTargetView, error) {
	var record NotificationTarget
	if err := tx.db.Where("id = ?", id).First(&record).Error; err != nil {
		// 只有"查不到记录"才是目标不存在。此前把任何数据库错误都包装成该哨兵，
		// 调用方据此无法区分"目标确实没了"与"查询本身失败"——后者会让调用方
		// 把完好的目标的在途通知误判为应丢弃。
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return NotificationTargetView{}, fmt.Errorf("%w：%s", ErrNotificationTargetMissing, id)
		}
		return NotificationTargetView{}, fmt.Errorf("读取通知目标失败：%w", translateSQLError(err))
	}
	return viewFromNotificationTarget(record), nil
}

// viewFromNotificationTarget 构造脱敏视图。
func viewFromNotificationTarget(record NotificationTarget) NotificationTargetView {
	return NotificationTargetView{
		ID:           record.ID,
		Name:         record.Name,
		Type:         record.Type,
		Enabled:      record.Enabled,
		MaskedSecret: MaskSecret(record.Secret),
		Summary:      record.TargetSummary,
		WebhookURL:   maskWebhookURL(record.WebhookURL),
		SMTPHost:     record.SMTPHost,
		SMTPPort:     record.SMTPPort,
		SMTPFrom:     record.SMTPFrom,
		SMTPTo:       splitStoredRecipients(record.SMTPTo),
		SMTPSecurity: record.SMTPSecurity,
		CreatedAt:    record.CreatedAt,
		UpdatedAt:    record.UpdatedAt,
	}
}

// maskWebhookURL 掩去目标地址中的查询串与片段。
//
// 规格 §3.5 要求读取时隐藏地址的凭据部分，而查询串正是凭据的常见载体
// （`?token=...`、`?key=...`）——它与 secret 字段一样只在写入时接收，
// 读取一律不完整回显。路径予以保留：管理员需要靠它区分同一主机上的多个目标。
func maskWebhookURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		// 解析失败时不回显原文：它可能正是导致解析失败的异常内容。
		return "（地址不可解析）"
	}
	if parsed.RawQuery != "" {
		parsed.RawQuery = "已隐藏"
	}
	parsed.Fragment = ""
	return parsed.String()
}

// MaskSecret 返回秘密的掩码：只保留末四位供运维识别。
//
// 未设置时返回中文说明而不是空串，避免界面把"没有秘密"显示成空白而
// 让人误以为加载失败。
func MaskSecret(secret string) string {
	if secret == "" {
		return "（未设置）"
	}
	if len(secret) < 8 {
		return "****"
	}
	return "****" + secret[len(secret)-4:]
}

// splitStoredRecipients 还原逗号分隔的收件人列表。
func splitStoredRecipients(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return trimRecipients(strings.Split(value, ","))
}
