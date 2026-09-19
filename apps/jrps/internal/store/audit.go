package store

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// FR-16 规格 §2.1/§2.2 要求的审计动作取值。
//
// 动作是封闭枚举：不得用自由文本拼接，新动作必须在本文件登记后才能写入。
// 前两组沿用 FR-02 与 FR-25 已落库的取值，语义不变。
const (
	// 配置与代理变更。
	ActionProxyCreate    = "proxy_create"
	ActionProxyUpdate    = "proxy_update"
	ActionProxyDelete    = "proxy_delete"
	ActionApplyPrepare   = "apply_prepare"
	ActionApplyPublish   = "apply_publish"
	ActionApplyFailure   = "apply_failure"
	ActionRestoreApply   = "restore_apply"
	ActionRevisionAppend = "revision_append"

	// 客户端与凭据。
	ActionClientCreate = "client_create"
	ActionClientRotate = "client_rotate"
	ActionClientRevoke = "client_revoke"

	// 认证会话。
	ActionAdminInitialized  = "admin_initialized"
	ActionAdminLogin        = "admin_login"
	ActionAdminLogout       = "admin_logout"
	ActionAdminLoginFailure = "admin_login_failure"

	// 保留策略。
	ActionPolicyUpdate       = "policy_update"
	ActionAuditCleanup       = "audit_cleanup"
	ActionBodyCaptureCleanup = "body_capture_cleanup"
)

// FR-16 规格 §2.1 的对象类型封闭集合。
//
// 与动作一样是封闭枚举：对象只携带类型与标识，不携带敏感属性值。
const (
	ObjectTypeClient          = "client"
	ObjectTypeProxy           = "proxy"
	ObjectTypeConfigRevision  = "config_revision"
	ObjectTypeToken           = "token"
	ObjectTypeNotificationMsg = "notification_target"
	ObjectTypeCaptureRecord   = "capture_record"
	ObjectTypeRetentionPolicy = "retention_policy"
	ObjectTypeSession         = "session"
	ObjectTypeAdminCredential = "admin_credential"
)

// auditActions 是允许写入的动作全集。
var auditActions = map[string]struct{}{
	ActionProxyCreate: {}, ActionProxyUpdate: {}, ActionProxyDelete: {},
	ActionApplyPrepare: {}, ActionApplyPublish: {}, ActionApplyFailure: {},
	ActionRestoreApply: {}, ActionRevisionAppend: {},
	ActionClientCreate: {}, ActionClientRotate: {}, ActionClientRevoke: {},
	ActionAdminInitialized: {}, ActionAdminLogin: {}, ActionAdminLogout: {},
	ActionAdminLoginFailure: {},
	ActionPolicyUpdate:      {}, ActionAuditCleanup: {}, ActionBodyCaptureCleanup: {},
}

// objectTypes 是允许写入的对象类型全集。
var objectTypes = map[string]struct{}{
	ObjectTypeClient: {}, ObjectTypeProxy: {}, ObjectTypeConfigRevision: {},
	ObjectTypeToken: {}, ObjectTypeNotificationMsg: {}, ObjectTypeCaptureRecord: {},
	ObjectTypeRetentionPolicy: {}, ObjectTypeSession: {}, ObjectTypeAdminCredential: {},
}

// auditResults 是允许的结果全集：失败与被拒绝同样要记录，不得跳过。
var auditResults = map[string]struct{}{
	AuditResultSuccess: {}, AuditResultFailure: {}, AuditResultDenied: {},
}

// maxAuditContextRunes 是脱敏上下文的最大长度，与模型列宽（255 字节）留出余量：
// 中文按 UTF-8 最大 3 字节计，85 字中文落在 255 字节内。
const maxAuditContextRunes = 85

// ErrAuditInvalid 表示审计事件未通过字段校验。
//
// 校验失败必须让业务事务整体回滚：宁可操作失败，也不能留下字段不合法或
// 携带敏感值的审计记录。
var ErrAuditInvalid = errors.New("审计事件校验未通过")

// forbiddenAuditMarkers 是绝不允许出现在审计文本中的标记。
//
// 只收录"一旦出现就必定是泄漏"的字面量：凭据字段名被写出来、协议头被整行抄录、
// 私钥块、会话 Cookie 对，以及"字段名 = 值"的赋值形态。
//
// 刻意不把 "token""password" 这类词本身列为禁词——规格禁止的是记录完整秘密，
// 不是禁止描述动作，「token 摘要前缀 a1b2c3d4」正是规格要求的合规写法。按词判禁
// 会误伤合法审计，迫使写入方把上下文写成含糊措辞，反而削弱审计价值。
//
// 局限（必须如实承认）：本表是纵深防御，不是保证。随机 token 没有可识别形状，
// 若写入方直接拼接完整 token 值而无字段名与等号，本表无法识破。真正可靠的做法
// 是在写入点只传白名单字段，而不是指望在出口识别秘密。
var forbiddenAuditMarkers = []string{
	// 凭据字段名：出现即说明写入方试图记录凭据字段。
	"password_digest", "password_salt", "password_hash",
	// 协议头整行：授权头与会话 Cookie 的原文形态。
	"authorization:", "authorization：",
	"cookie:", "cookie：",
	"set-cookie",
	// 私钥与证书块。
	"-----begin",
	// 会话 Cookie 名值对。
	"jrp_session=",
}

// forbiddenAssignmentNames 是"字段名 = 值"形态中会被判为泄漏的字段名。
//
// 合规写法以"摘要前缀 + 8 位十六进制"描述，不带等号赋值；一旦出现 `token=`
// 这类赋值，等号右侧通常就是秘密本体。用赋值形态而不是值长度判定，规则可解释、
// 不依赖启发式阈值。
var forbiddenAssignmentNames = []string{
	"token", "password", "passwd", "secret", "authorization",
	"cookie", "credential", "api_key", "apikey", "access_key",
}

// forbiddenPathMarkers 覆盖规格禁止出现的分段文件路径。
//
// 路径按形状识别：盘符前缀、UNC 前缀与常见 Unix 根目录前缀。中文上下文里正常
// 不含这些片段，命中即判为泄漏。
var forbiddenPathMarkers = []string{
	`:\`, `\\`, "/home/", "/users/", "/var/", "/tmp/", "/etc/",
}

// validateAuditEvent 校验审计事件的字段合法性。
//
// 校验范围覆盖规格 §2.1 对六项的约束中可由本层保证的部分：动作与对象类型属于
// 封闭枚举、结果属于三值、主体与标识非空、上下文长度受限且不含敏感标记。
// 时间由调用方强制为服务端当前时间，不在此校验。
func validateAuditEvent(event AuditEvent) error {
	if _, ok := auditActions[event.Action]; !ok {
		return invalidAudit("动作不在封闭枚举内：" + event.Action)
	}
	if _, ok := objectTypes[event.ObjectType]; !ok {
		return invalidAudit("对象类型不在封闭枚举内：" + event.ObjectType)
	}
	if _, ok := auditResults[event.Result]; !ok {
		return invalidAudit("结果只能是成功、失败或被拒绝：" + event.Result)
	}
	if !isKnownActorType(event.ActorType) {
		return invalidAudit("主体类别只能是管理员或客户端：" + event.ActorType)
	}
	if strings.TrimSpace(event.ActorID) == "" {
		return invalidAudit("主体标识不能为空")
	}
	if strings.TrimSpace(event.ObjectID) == "" {
		return invalidAudit("对象标识不能为空")
	}
	if utf8.RuneCountInString(event.Context) > maxAuditContextRunes {
		return invalidAudit("脱敏上下文超出长度上限")
	}
	if marker, found := findForbiddenMarker(event.Context); found {
		// 只报告命中的类别，不回显上下文原文：错误信息本身也可能进入日志。
		return invalidAudit("脱敏上下文含禁止出现的敏感内容（" + marker + "）")
	}
	return nil
}

// isKnownActorType 判断主体类别是否属于 P1 的封闭集合。
func isKnownActorType(actorType string) bool {
	return actorType == ActorTypeAdmin || actorType == ActorTypeClient
}

// findForbiddenMarker 返回上下文中命中的首个禁止标记。
func findForbiddenMarker(context string) (string, bool) {
	lowered := strings.ToLower(context)
	for _, marker := range forbiddenAuditMarkers {
		if strings.Contains(lowered, marker) {
			return marker, true
		}
	}
	for _, marker := range forbiddenPathMarkers {
		if strings.Contains(lowered, marker) {
			return marker, true
		}
	}
	if name, found := findCredentialAssignment(lowered); found {
		return name + "=", true
	}
	return "", false
}

// findCredentialAssignment 检查上下文中是否存在"凭据字段 = 值"的赋值形态。
//
// 判定规则可解释：字段名后跟等号或冒号，且二者之间无其他内容。这样
// 「token 摘要前缀 a1b2c3d4」放行，而「token=abc123」被拦。
func findCredentialAssignment(loweredContext string) (string, bool) {
	for _, name := range forbiddenAssignmentNames {
		for _, separator := range []string{"=", ":", "："} {
			if strings.Contains(loweredContext, name+separator) {
				return name, true
			}
		}
	}
	return "", false
}

// invalidAudit 构造带中文说明的审计校验错误。
func invalidAudit(detail string) error {
	return errors.Join(ErrAuditInvalid, errors.New(detail))
}
