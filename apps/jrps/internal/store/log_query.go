package store

import (
	"strings"
	"time"
)

// QueryLogEvents 按过滤条件分页查询运行日志（FR-12 规格 §3.5）。
//
// 过滤维度：时间范围、等级、组件、事件名与关联标识；游标为递增主键。
// 参数校验失败返回 ErrLogQueryInvalid，由端点层转成 400 问题详情。
func (tx *Tx) QueryLogEvents(query LogQuery) (LogPage, error) {
	limit, err := normalizeAuditLimit(query.Limit)
	if err != nil {
		return LogPage{}, err
	}
	level, err := normalizeLogLevel(query.Level)
	if err != nil {
		return LogPage{}, err
	}
	cursor, err := decodeAuditCursor(query.Cursor)
	if err != nil {
		return LogPage{}, err
	}

	conditions := []string{"1 = 1"}
	args := []any{}
	if level != "" {
		conditions = append(conditions, "level = ?")
		args = append(args, level)
	}
	if query.Component != "" {
		conditions = append(conditions, "component = ?")
		args = append(args, query.Component)
	}
	if query.Event != "" {
		conditions = append(conditions, "event = ?")
		args = append(args, query.Event)
	}
	if query.ClientID != "" {
		conditions = append(conditions, "client_id = ?")
		args = append(args, query.ClientID)
	}
	if query.ProxyName != "" {
		conditions = append(conditions, "proxy_name = ?")
		args = append(args, query.ProxyName)
	}
	if query.RequestID != "" {
		conditions = append(conditions, "request_id = ?")
		args = append(args, query.RequestID)
	}
	if !query.From.IsZero() {
		conditions = append(conditions, "occurred_at >= ?")
		args = append(args, query.From)
	}
	if !query.To.IsZero() {
		conditions = append(conditions, "occurred_at <= ?")
		args = append(args, query.To)
	}
	if cursor > 0 {
		conditions = append(conditions, "id > ?")
		args = append(args, cursor)
	}

	// 多取一条用于判断是否还有下一页，避免额外一次计数查询。
	var events []LogEvent
	statement := strings.Join(conditions, " AND ")
	if err := tx.db.Where(statement, args...).
		Order("id ASC").
		Limit(limit + 1).
		Find(&events).Error; err != nil {
		return LogPage{}, err
	}

	page := LogPage{}
	if len(events) > limit {
		events = events[:limit]
		next := events[len(events)-1].ID
		page.NextCursor = encodeAuditCursor(next)
	}
	page.Items = events
	return page, nil
}

// logEventOccurredAtNow 以当前时间补齐事件时间戳（提交入口使用）。
func logEventOccurredAtNow(event *LogEvent) {
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now()
	}
}
