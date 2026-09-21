package store

import "time"

// 本文件是 jrps 外壳私有的 GORM 模型（FR-09 规格 §3.3 的实体清单）。
// 这些类型不得进入 Core：Core 不接触 GORM 模型，只接受不可变配置快照（ADR-0004）。

// 数据库角色标记：用于识别数据库文件归属于哪一侧外壳。
const (
	RoleServer = "jrps"
	RoleClient = "jrpc"
)

// store_meta 键名。
const (
	metaKeyRole          = "role"
	metaKeySchemaVersion = "schema_version"
)

// StoreMeta 保存数据库级别的元信息，其中 role 用于阻止两侧外壳复用同一文件。
type StoreMeta struct {
	Key   string `gorm:"primaryKey;size:64"`
	Value string `gorm:"size:255;not null"`
}

func (StoreMeta) TableName() string { return "store_meta" }

// AdminCredential 是 P1 唯一的管理员记录，不提供角色或租户字段。
type AdminCredential struct {
	ID               uint64    `gorm:"primaryKey"`
	Username         string    `gorm:"size:128;not null"`
	PasswordDigest   string    `gorm:"size:255;not null"` // 凭据派生材料，明文静态存储按已接受风险
	PasswordSalt     string    `gorm:"size:128;not null"`
	PasswordParams   string    `gorm:"size:255;not null"` // 派生参数随记录保存
	InitializedAt    time.Time `gorm:"not null"`
	InitializationID string    `gorm:"size:64;not null"` // 初始化标记，与管理员记录同事务提交
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func (AdminCredential) TableName() string { return "admin_credentials" }

// Session 只保存会话标识摘要，不保存可重放的服务端会话密钥明文。
type Session struct {
	ID            uint64 `gorm:"primaryKey"`
	TokenDigest   string `gorm:"size:128;uniqueIndex;not null"`
	CSRFToken     string `gorm:"size:128;not null"`
	CreatedAt     time.Time
	ExpiresAt     time.Time `gorm:"index;not null"`
	InvalidatedAt *time.Time
}

func (Session) TableName() string { return "sessions" }

// 客户端 enrollment 与连接状态取值。
const (
	EnrollmentStatePending = "pending"
	EnrollmentStateActive  = "active"
	EnrollmentStateRevoked = "revoked"

	ConnectionStateOffline = "offline"
	ConnectionStateOnline  = "online"
)

// Client 保存客户端身份与独立 token 摘要；token 明文只在创建响应中返回一次。
type Client struct {
	ID              string `gorm:"primaryKey;size:64"`
	Name            string `gorm:"size:128;not null"`
	TokenDigest     string `gorm:"size:128;uniqueIndex;not null"`
	EnrollmentState string `gorm:"size:32;not null"`
	ConnectionState string `gorm:"size:32;not null"`
	// DesiredRevision 是服务端为该客户端表达的期望版本，属持久状态。
	DesiredRevision uint64
	// ActiveRevision 是最近一次成功 publish 的 Apply 结果记录，仅供审计与展示（ADR-0012）。
	ActiveRevision uint64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (Client) TableName() string { return "clients" }

// EnrollmentCredential 是一次性 enrollment 凭据。
//
// 与客户端 token 是两种不同的凭据（FR-07 §3.4）：凭据用于首次注册换取独立
// token，本身不可用于管理通道鉴权；客户端 token 才是后续请求的凭据。两者
// 都只存摘要，明文只在发行响应中出现一次。
type EnrollmentCredential struct {
	ID string `gorm:"primaryKey;size:64"`
	// ClientID 是这张凭据绑定的待注册客户端。
	ClientID string `gorm:"size:64;index;not null"`
	// TokenDigest 是凭据明文的摘要，落库内容不可用于直接鉴权。
	TokenDigest string `gorm:"size:128;uniqueIndex;not null"`
	// ExpiresAt 是凭据的过期时间；过期与已使用都会使兑换失败。
	ExpiresAt time.Time `gorm:"index;not null"`
	// UsedAt 非空表示凭据已被兑换；一次性语义靠它保证。
	UsedAt    *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (EnrollmentCredential) TableName() string { return "enrollment_credentials" }

// Proxy 是代理定义；任何变更都会生成新的配置版本，不原地修改历史。
type Proxy struct {
	ID             string `gorm:"primaryKey;size:64"`
	ClientID       string `gorm:"size:64;index;not null"`
	Name           string `gorm:"size:128;not null"`
	Type           string `gorm:"size:32;not null"`
	LocalPort      int
	RemotePort     int
	Target         string `gorm:"size:255"`
	Transport      string `gorm:"size:32"`
	SecurityParams string `gorm:"size:255"`
	CaptureEnabled bool   `gorm:"not null;default:false"` // 正文采集默认关闭，逐代理开启
	Deleted        bool   `gorm:"not null;default:false"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (Proxy) TableName() string { return "proxies" }

// 配置版本的来源，用于审计追溯。
const (
	OriginProxyCreate = "proxy_create"
	OriginProxyUpdate = "proxy_update"
	OriginProxyDelete = "proxy_delete"
	OriginRestore     = "restore"
)

// ConfigRevision 是不可变配置版本：新版本以新记录追加，禁止原地修改。
type ConfigRevision struct {
	Revision uint64 `gorm:"primaryKey;autoIncrement:false"`
	// Content 是 desired 的不可变内容，是唯一具备真源性质的部分（ADR-0012）。
	Content       string `gorm:"not null"`
	Creator       string `gorm:"size:128;not null"`
	Origin        string `gorm:"size:32;not null"`
	ChangeSummary string `gorm:"size:255"`
	CreatedAt     time.Time
}

func (ConfigRevision) TableName() string { return "config_revisions" }

// RevisionState 把 desired、active、last-good 分列表达，禁止用单一字段兼表多种含义。
//
// 三列的语义严格区分（ADR-0012）：
//   - DesiredRevision：持久化真源，只有控制面适配器写入。
//   - ActiveRevision、LastGoodRevision：Core 运行态归 Core 内存持有，此处保存的是
//     Apply 结果记录，仅供审计与重启后展示，不构成真源。
type RevisionState struct {
	ID uint64 `gorm:"primaryKey"`
	// Scope 区分状态归属：服务端整体配置或某个客户端。
	Scope            string `gorm:"size:32;not null;default:'server'"`
	DesiredRevision  uint64
	ActiveRevision   uint64
	LastGoodRevision uint64
	UpdatedAt        time.Time
}

func (RevisionState) TableName() string { return "revision_state" }

// 配置应用的四个阶段，与服务端、客户端共用的阶段命名一致。
const (
	PhasePrepare     = "prepare"
	PhaseHealthCheck = "health_check"
	PhasePublish     = "publish"
	PhaseDrain       = "drain"
)

// ApplyResult 记录某个 revision 在某个阶段的 Apply 结果，供审计、展示与重启后重建。
// Core 不知道本表的格式，也不读取它。
type ApplyResult struct {
	ID          uint64 `gorm:"primaryKey"`
	Revision    uint64 `gorm:"index;not null"`
	Scope       string `gorm:"size:32;not null;default:'server'"`
	Phase       string `gorm:"size:32;not null"`
	Succeeded   bool   `gorm:"not null"`
	ErrorDetail string `gorm:"size:255"` // 安全可公开的中文摘要，不含堆栈、路径或 SQL
	OccurredAt  time.Time
}

func (ApplyResult) TableName() string { return "apply_results" }

// 审计结果取值。
const (
	AuditResultSuccess = "success"
	AuditResultFailure = "failure"
	AuditResultDenied  = "denied"
)

// AuditEvent 记录安全敏感操作；上下文为白名单字段，不含完整秘密与完整正文。
type AuditEvent struct {
	ID              uint64    `gorm:"primaryKey"`
	OccurredAt      time.Time `gorm:"index;not null"`
	ActorType       string    `gorm:"size:32;not null"`
	ActorID         string    `gorm:"size:128"`
	Action          string    `gorm:"size:64;index;not null"`
	ObjectType      string    `gorm:"size:64;index;not null"`
	ObjectID        string    `gorm:"size:128;index"`
	Result          string    `gorm:"size:32;not null"`
	Context         string    `gorm:"size:255"` // 已脱敏的中文上下文
	RequestID       string    `gorm:"size:64"`
	DesiredRevision uint64
}

func (AuditEvent) TableName() string { return "audit_events" }

// 通知渠道类型；P1 只支持两种，不引入其他渠道。
const (
	NotificationTypeWebhook = "webhook"
	NotificationTypeEmail   = "email"
)

// SMTP 安全传输选项。
const (
	// SMTPSecurityNone 是明文 SMTP，仅允许显式选择：默认值不是它。
	SMTPSecurityNone = "none"
	// SMTPSecurityStartTLS 是先明文连接再升级 TLS，P1 的默认与推荐取值。
	SMTPSecurityStartTLS = "starttls"
)

// NotificationTarget 保存通知目标；秘密只在写入时接收，读取时掩码。
//
// 两种渠道的配置字段互斥共存：Webhook 用 URL 相关列，邮件用 SMTP 相关列。
// 拆成显式列而非一个 JSON 配置列，是为了让校验（URL 协议/主机/端口、收件人
// 上限、端口范围）落在明确字段上，避免把校验规则藏进序列化字符串。
type NotificationTarget struct {
	ID            string `gorm:"primaryKey;size:64"`
	Type          string `gorm:"size:32;not null"`
	Name          string `gorm:"size:128;not null"`
	TargetSummary string `gorm:"size:255;not null"` // 脱敏后的目标摘要
	Secret        string `gorm:"size:255"`          // Webhook 签名密钥或 SMTP 密码，明文静态存储按已接受风险
	Enabled       bool   `gorm:"not null;default:true"`

	// WebhookURL 是 Webhook 渠道的投递地址，仅 Type 为 webhook 时非空。
	WebhookURL string `gorm:"size:512"`

	// SMTP 渠道配置，仅 Type 为 email 时非空。
	SMTPHost     string `gorm:"size:255"`
	SMTPPort     int
	SMTPFrom     string `gorm:"size:255"`
	SMTPTo       string `gorm:"size:1024"` // 收件人，逗号分隔；读取时按上限校验
	SMTPSecurity string `gorm:"size:32"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

func (NotificationTarget) TableName() string { return "notification_targets" }

// outbox 记录状态，与 FR-15 规格 §3.3 的状态表一致。
const (
	OutboxStatusPending   = "pending"
	OutboxStatusSending   = "sending"
	OutboxStatusSent      = "sent"
	OutboxStatusRetrying  = "retrying"
	OutboxStatusFailed    = "failed"
	OutboxStatusDiscarded = "discarded"
)

// NotificationOutbox 是事务 outbox：业务事务提交成功后才由发送器读取执行外部副作用。
type NotificationOutbox struct {
	ID             uint64 `gorm:"primaryKey"`
	EventID        string `gorm:"size:64;uniqueIndex;not null"` // 幂等事件标识
	TargetID       string `gorm:"size:64;index"`
	EventType      string `gorm:"size:64;not null"`
	Payload        string `gorm:"not null"` // 脱敏载荷，只含白名单字段
	Status         string `gorm:"size:32;index;not null"`
	Attempts       int    `gorm:"not null;default:0"`
	NextAttemptAt  time.Time
	LeaseExpiresAt *time.Time
	LastError      string `gorm:"size:255"`
	// StoppedAt 记录进入失败终态的时间，供运维查询"何时停止重试"（规格 §3.3）。
	// 只有 failed 与 discarded 会写入它。
	StoppedAt *time.Time
	CreatedAt time.Time `gorm:"index"`
	UpdatedAt time.Time
}

func (NotificationOutbox) TableName() string { return "notification_outbox" }

// outboxStatuses 是投递状态的封闭枚举。
//
// 与审计动作、对象类型同样按封闭枚举校验：查询参数里出现枚举外的值时返回
// 400，而不是静默忽略让调用方误以为过滤生效。
var outboxStatuses = map[string]struct{}{
	OutboxStatusPending:   {},
	OutboxStatusSending:   {},
	OutboxStatusSent:      {},
	OutboxStatusRetrying:  {},
	OutboxStatusFailed:    {},
	OutboxStatusDiscarded: {},
}

// IsOutboxStatus 判断投递状态是否在封闭枚举内。
func IsOutboxStatus(status string) bool {
	_, ok := outboxStatuses[status]
	return ok
}

// RequestRecord 是 HTTP 请求元数据，仅在采集开启时产生（ADR-0007）。
type RequestRecord struct {
	ID            uint64    `gorm:"primaryKey"`
	ClientID      string    `gorm:"size:64;index"`
	ProxyID       string    `gorm:"size:64;index"`
	OccurredAt    time.Time `gorm:"index;not null"`
	Method        string    `gorm:"size:16;not null"`
	Host          string    `gorm:"size:255"`
	PathDigest    string    `gorm:"size:255"` // 路径只保留摘要，查询串按键名掩码
	StatusCode    int
	RequestBytes  int64
	ResponseBytes int64
	DurationMS    int64
	ResultClass   string `gorm:"size:32"`
	SegmentID     string `gorm:"size:64;index"` // 正文段引用，可为空
	SegmentOffset int64
}

func (RequestRecord) TableName() string { return "request_records" }

// 正文分段状态。
const (
	SegmentStateActive   = "active"
	SegmentStateDeleting = "deleting"
	SegmentStateDeleted  = "deleted"
	SegmentStateMissing  = "missing"
)

// BodySegment 是正文分段索引；正文本身不存入 SQLite 大字段（ADR-0007）。
type BodySegment struct {
	ID              uint64 `gorm:"primaryKey"`
	SegmentID       string `gorm:"size:64;uniqueIndex;not null"`
	EntryCount      int
	CompressedBytes int64
	OriginalBytes   int64
	RetainUntil     time.Time `gorm:"index;not null"`
	State           string    `gorm:"size:32;index;not null"`
	CreatedAt       time.Time
}

func (BodySegment) TableName() string { return "body_segments" }

// capturePolicyRowID 是策略单例行的固定主键：策略是受管理的对象而非流水记录，
// 全库只允许存在一行，读取时按其定位，不引入版本表。
const capturePolicyRowID = 1

// CapturePolicy 是受管理的保留策略对象（FR-16 规格 §3.4）。
//
// 采集默认关闭；保留天数与正文总量上限两个限制独立计算，任一先到即触发清理。
// 它是策略真源，不散落在采集代码里的常量（ADR-0004）。
//
// 审计保留天数与正文策略同表但相互独立（§3.7）：正文清理不牵连审计，
// 审计清理只按时间维度、不按大小。同表存放是因为两者同属"保留策略对象"、
// 由同一个端点读写，不是因为它们互相影响。
type CapturePolicy struct {
	ID uint64 `gorm:"primaryKey"`

	// CaptureEnabled 是采集总开关，默认关闭：未显式开启前不产生任何请求记录。
	CaptureEnabled bool `gorm:"not null;default:false"`

	// RetentionDays 是正文保留天数，从记录产生时刻起算。
	RetentionDays int `gorm:"not null"`

	// MaxTotalBytes 是正文分段文件总量上限，单位字节。
	MaxTotalBytes int64 `gorm:"not null"`

	// AuditRetentionDays 是审计事件保留天数，独立于正文策略。
	AuditRetentionDays int `gorm:"not null"`

	UpdatedAt time.Time
}

func (CapturePolicy) TableName() string { return "capture_policy" }
