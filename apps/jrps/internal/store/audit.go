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

	// 通知目标管理（FR-15 §3.6）：增删改与测试通知都必须留痕。
	ActionNotificationTargetCreate = "notification_target_create"
	ActionNotificationTargetUpdate = "notification_target_update"
	ActionNotificationTargetDelete = "notification_target_delete"
	ActionNotificationTargetTest   = "notification_target_test"
	ActionNotificationDiscard      = "notification_discard"
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
	ActionNotificationTargetCreate: {}, ActionNotificationTargetUpdate: {},
	ActionNotificationTargetDelete: {}, ActionNotificationTargetTest: {},
	ActionNotificationDiscard: {},
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

// auditLabelLimit 是嵌入审计上下文的用户输入标签长度上限。
//
// 取一个远小于 maxAuditContextRunes 的值，使"标签 + 固定模板"永远落在上下文
// 上限内：审计长度校验是防止脏数据落库的护栏，不该反过来变成业务写入的隐式约束。
// 早先的实现把不限长的客户端名称直接拼进上下文，导致名字较长的客户端在轮换与
// 吊销时因审计校验失败而整体回滚——管理员因此无法吊销已失陷客户端的凭据。
const auditLabelLimit = 32

// truncateAuditLabel 把用户输入截断为可安全嵌入审计上下文的标签。
//
// 按 rune 截断而不是按字节：按字节切会撕裂多字节字符，落库后是无法阅读的乱码。
// 超长时追加省略号，让读审计的人知道这里被截断过，而不是以为名称本就这么短。
func truncateAuditLabel(value string) string {
	if utf8.RuneCountInString(value) <= auditLabelLimit {
		return value
	}
	runes := []rune(value)
	return string(runes[:auditLabelLimit]) + "…"
}

// maxAuditIdentifierRunes 是主体标识与对象标识的最大字符数。
//
// 模型列宽为 128 字节；中文按 3 字节计，取 42 字以保证任何字符集下都不超列宽。
// 没有这道校验时，未认证请求提交的超长用户名会一路走到审计写入，落库时被截断
// 或失败——而查询响应会把它原样带出（实测 2MB 用户名可让审计响应膨胀到 4.2MB）。
const maxAuditIdentifierRunes = 42

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

// forbiddenPathMarkers 覆盖规格禁止出现的**分段文件**路径特征。
//
// 规格禁止的是正文分段文件的路径（FR-13 的产物），不是任意文件路径：审计里记录
// "读取 /etc/nginx/nginx.conf 失败" 是正当的运维上下文，把通用路径前缀列为禁词
// 会拒绝这类合法写入，反而迫使写入方把上下文写成含糊措辞、削弱审计价值。
//
// 因此按分段文件的命名形态识别：分段文件由 FR-13 生成，扩展名为 .seg 或
// segments 目录下的编号文件。FR-13 落地后若命名规则变化，此处需要同步。
var forbiddenPathMarkers = []string{
	".seg", "segments/", `segments\`,
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
	// 标识长度受列宽约束：超长输入会被数据库截断或直接写入失败，两者都不可接受。
	// 在写入前明确拒绝，避免把"标识被悄悄截断"变成难以追查的审计缺陷。
	if utf8.RuneCountInString(event.ActorID) > maxAuditIdentifierRunes {
		return invalidAudit("主体标识超出长度上限")
	}
	if utf8.RuneCountInString(event.ObjectID) > maxAuditIdentifierRunes {
		return invalidAudit("对象标识超出长度上限")
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
// 只认等号赋值：`token=abc123`、`secret=s3cr3t` 这类形态是凭据被写出来的典型
// 标志，而冒号在中文运维摘要里太常见（"已更新代理 secret: 分组"、"密钥轮换：
// 失败"），把它一并当赋值会拒绝大量合法上下文，迫使写入方把审计写成含糊措辞。
//
// 这确实放过了 `secret: abc123` 这种以冒号赋值的写法。取舍依据是两侧代价不对称：
// 漏判的是一条本该被拦的记录，而误判会让正当的审计写入整体失败并回滚业务事务。
// 弥补方式是写入侧只传白名单字段（见 validateAuditEvent 的说明）。
func findCredentialAssignment(loweredContext string) (string, bool) {
	for _, name := range forbiddenAssignmentNames {
		if strings.Contains(loweredContext, name+"=") {
			return name, true
		}
	}
	return "", false
}

// invalidAudit 构造带中文说明的审计校验错误。
func invalidAudit(detail string) error {
	return errors.Join(ErrAuditInvalid, errors.New(detail))
}
