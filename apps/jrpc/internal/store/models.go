package store

import "time"

// 本文件是 jrpc 外壳私有的 GORM 模型。
//
// 与 jrps 侧的模型完全独立：两侧各维护自己的模型与迁移，互不导入、不共享 schema 文件
// （FR-09 规格 §3.1）。这些类型不得进入 Core。

// store_meta 键名。
const (
	metaKeyRole = "role"
)

// StoreMeta 保存数据库级别的元信息；role 用于阻止两侧外壳复用同一文件。
type StoreMeta struct {
	Key   string `gorm:"primaryKey;size:64"`
	Value string `gorm:"size:255;not null"`
}

func (StoreMeta) TableName() string { return "store_meta" }

// Identity 是本客户端的 enrollment 身份与独立 token。
//
// token 明文保存在本地（FR-08 §2），但进程命令行与服务注册不得包含它。
type Identity struct {
	ID            uint64 `gorm:"primaryKey"`
	ClientID      string `gorm:"size:64;uniqueIndex;not null"`
	Token         string `gorm:"size:255;not null"`
	ServerAddress string `gorm:"size:255"`
	DeviceSummary string `gorm:"size:255"`
	EnrolledAt    time.Time
	UpdatedAt     time.Time
}

func (Identity) TableName() string { return "identities" }

// DesiredState 是服务端下发的本地期望配置版本，内容不可变。
//
// DesiredRevision 是本地 desired 真源；active 与 last-good 分列在 RevisionRecord 中，
// 保存的是 Apply 结果记录，运行态归 Core 内存（ADR-0012）。
type DesiredState struct {
	Revision       uint64 `gorm:"primaryKey;autoIncrement:false"`
	Content        string `gorm:"not null"`
	ServerRevision uint64 // 服务端版本号，用于冲突判定与条件请求
	ReceivedAt     time.Time
}

func (DesiredState) TableName() string { return "desired_states" }

// RevisionRecord 分列表达本地的 desired、active 与 last-good。
//
// active 与 last-good 保存的是 Apply 结果记录，仅供审计、回执与重启后展示，不构成真源。
type RevisionRecord struct {
	ID               uint64 `gorm:"primaryKey"`
	DesiredRevision  uint64
	ActiveRevision   uint64
	LastGoodRevision uint64
	UpdatedAt        time.Time
}

func (RevisionRecord) TableName() string { return "revision_records" }

// 配置应用的四个阶段，与服务端共用的阶段命名一致，便于两侧回执对照。
const (
	PhasePrepare     = "prepare"
	PhaseHealthCheck = "health_check"
	PhasePublish     = "publish"
	PhaseDrain       = "drain"
)

// ApplyResult 记录某个 revision 在某个阶段的本地 Apply 结果，供审计与回执上报。
type ApplyResult struct {
	ID          uint64     `gorm:"primaryKey"`
	Revision    uint64     `gorm:"index;not null"`
	Phase       string     `gorm:"size:32;not null"`
	Succeeded   bool       `gorm:"not null"`
	ErrorDetail string     `gorm:"size:255"` // 安全可公开的中文摘要
	ReportedAt  *time.Time // 已向服务端回执的时间；未回执为空
	OccurredAt  time.Time
}

func (ApplyResult) TableName() string { return "apply_results" }

// 审计结果取值。
const (
	AuditResultSuccess = "success"
	AuditResultFailure = "failure"
	AuditResultDenied  = "denied"
)

// 本地审计动作：jrpc 只记录与自身相关的本地敏感操作与应用结果。
const (
	ActionDesiredReceived = "desired_received"
	ActionApplyPublish    = "apply_publish"
	ActionApplyFailure    = "apply_failure"
	ActionApplyPrepare    = "apply_prepare"
	ActionIdentityStored  = "identity_stored"
)

// AuditEvent 是本地审计事件；内容脱敏，不含 token 明文、密码或完整正文。
type AuditEvent struct {
	ID         uint64    `gorm:"primaryKey"`
	OccurredAt time.Time `gorm:"index;not null"`
	Action     string    `gorm:"size:64;index;not null"`
	ObjectType string    `gorm:"size:64;not null"`
	ObjectID   string    `gorm:"size:128;index"`
	Result     string    `gorm:"size:32;not null"`
	Context    string    `gorm:"size:255"` // 已脱敏的中文上下文
	Revision   uint64
}

func (AuditEvent) TableName() string { return "audit_events" }

// 连接状态取值。
const (
	ConnectionStateOffline = "offline"
	ConnectionStateOnline  = "online"
)

// RuntimeState 是本地运行元数据：连接状态与最近一次成功应用的版本。
type RuntimeState struct {
	ID                   uint64 `gorm:"primaryKey"`
	ConnectionState      string `gorm:"size:32;not null"`
	LastReportedRevision uint64
	UpdatedAt            time.Time
}

func (RuntimeState) TableName() string { return "runtime_states" }

// 本地通知队列状态取值；jrpc 不上报审计，只上报回执。
const (
	OutboxStatusPending = "pending"
	OutboxStatusSent    = "sent"
	OutboxStatusFailed  = "failed"
)

// Outbox 是本地待上报队列：回执在业务事务提交后才由上报器取出发送。
type Outbox struct {
	ID        uint64 `gorm:"primaryKey"`
	EventID   string `gorm:"size:64;uniqueIndex;not null"`
	EventType string `gorm:"size:64;not null"`
	Payload   string `gorm:"not null"`
	Status    string `gorm:"size:32;index;not null"`
	Attempts  int    `gorm:"not null;default:0"`
	LastError string `gorm:"size:255"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (Outbox) TableName() string { return "outbox" }
