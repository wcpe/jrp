package store

import (
	"fmt"
	"time"
)

// 日志等级的封闭枚举（FR-12 规格 §3.3）：只允许四级，其他取值在写入点被拒绝。
//
// 用字符串枚举而不是整型：等级要出现在查询参数与 JSON 输出里，字符串让
// API 契约自描述，也避免等级数值漂移破坏已持久化的行。
const (
	LogLevelError = "ERROR"
	LogLevelWarn  = "WARN"
	LogLevelInfo  = "INFO"
	LogLevelDebug = "DEBUG"
)

// logLevels 是允许写入的等级全集。
var logLevels = map[string]struct{}{
	LogLevelError: {}, LogLevelWarn: {}, LogLevelInfo: {}, LogLevelDebug: {},
}

// DefaultLogBufferSize 是运行日志通道的默认缓冲容量（条数）。
//
// 缓冲满时按等级降级：DEBUG 与 INFO 直接丢弃并计数，WARN 与 ERROR 保留——
// 诊断可以缺，故障不能瞎（规格 §3.2）。
const DefaultLogBufferSize = 1024

// DefaultLogBatchSize 是落库批处理的单批条数。
const DefaultLogBatchSize = 64

// LogEvent 是一条运行日志（FR-12 规格 §3.2 运行日志通道）。
//
// 字段只承载标识、等级与中文摘要：token、密码、Cookie、Authorization 与正文
// 原文在本功能的任何字段中都没有位置（规格 §3.4 脱敏矩阵）。
type LogEvent struct {
	ID         uint64    `gorm:"primaryKey"`
	OccurredAt time.Time `gorm:"index;not null"`
	Level      string    `gorm:"size:8;index;not null"`
	Component  string    `gorm:"size:32;index;not null"`
	Event      string    `gorm:"size:64;index;not null"`
	Message    string    `gorm:"size:255;not null"`
	ClientID   string    `gorm:"size:64;index"`
	ProxyName  string    `gorm:"size:64;index"`
	RequestID  string    `gorm:"size:64;index"`
	Revision   uint64
}

func (LogEvent) TableName() string { return "log_events" }

// LogQuery 是日志查询的过滤条件（规格 §3.5）。
type LogQuery struct {
	// From 与 To 是时间范围，闭区间；零值表示不限。
	From time.Time
	To   time.Time

	Level     string
	Component string
	Event     string
	ClientID  string
	ProxyName string
	RequestID string

	// Limit 是每页条数；为零时取 DefaultAuditPageSize（与审计同口径）。
	Limit int

	// Cursor 是上一页返回的游标；为空表示取第一页。
	Cursor string
}

// LogPage 是日志查询的分页结果。
type LogPage struct {
	Items      []LogEvent
	NextCursor string
}

// ErrLogQueryInvalid 表示日志查询参数不合法。
var ErrLogQueryInvalid = fmt.Errorf("日志查询参数不合法")

// NormalizeLogLevel 校验并返回等级；空串表示不过滤。供端点层在解析参数时
// 提前校验（store 层查询会再校验一次，两处共用同一判定避免漂移）。
func NormalizeLogLevel(level string) (string, error) {
	return normalizeLogLevel(level)
}

// DecodeLogCursor 校验游标格式并返回其数值；空串返回 0。
func DecodeLogCursor(cursor string) (uint64, error) {
	return decodeAuditCursor(cursor)
}

// normalizeLogLevel 校验并返回等级；空串表示不过滤。
func normalizeLogLevel(level string) (string, error) {
	if level == "" {
		return "", nil
	}
	if _, ok := logLevels[level]; !ok {
		return "", fmt.Errorf("%w：等级 %q 不在四级枚举内", ErrLogQueryInvalid, level)
	}
	return level, nil
}
