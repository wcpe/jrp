package store

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 审计查询的分页与过滤边界（对齐 API.md §1.5）。
const (
	DefaultAuditPageSize = 50
	MaxAuditPageSize     = 200

	// maxAuditFilterLength 限制过滤参数长度：超长参数按规格返回 400，
	// 不允许无界字符串直接进入查询条件。
	maxAuditFilterLength = 128
)

// ErrAuditQueryInvalid 表示审计查询参数不合法。
//
// 与 ErrAuditInvalid 区分：前者是查询参数问题（HTTP 400），后者是写入事件
// 字段问题（业务事务回滚）。
var ErrAuditQueryInvalid = errors.New("审计查询参数不合法")

// AuditQuery 是审计查询条件；零值表示不加该维度过滤。
type AuditQuery struct {
	// From 与 To 是时间范围，闭区间；零值表示不限。
	From time.Time
	To   time.Time

	Action     string
	ObjectType string
	Result     string

	// Limit 是每页条数；为零时取 DefaultAuditPageSize。
	Limit int

	// Cursor 是上一页返回的游标；为空表示取第一页。
	Cursor string
}

// AuditPage 是审计查询结果页。
type AuditPage struct {
	Items []AuditEvent
	// NextCursor 为空表示已到末页。
	NextCursor string
}

// QueryAuditEvents 按条件查询审计事件，使用游标分页。
//
// 游标基于自增主键而非时间戳：同一毫秒内可能有多条事件，用时间戳做游标会在
// 边界上重复或漏读，而主键单调唯一。返回顺序与游标推进方向一致（主键升序），
// 保证翻页不重不漏。
func (tx *Tx) QueryAuditEvents(query AuditQuery) (AuditPage, error) {
	limit, err := normalizeAuditLimit(query.Limit)
	if err != nil {
		return AuditPage{}, err
	}
	conditions, args, err := auditQueryConditions(query)
	if err != nil {
		return AuditPage{}, err
	}
	cursor, err := decodeAuditCursor(query.Cursor)
	if err != nil {
		return AuditPage{}, err
	}
	if cursor > 0 {
		conditions = append(conditions, "id > ?")
		args = append(args, cursor)
	}

	// 多取一条用于判断是否还有下一页，避免额外一次计数查询。
	var events []AuditEvent
	statement := "1 = 1"
	if len(conditions) > 0 {
		statement = strings.Join(conditions, " AND ")
	}
	err = tx.db.Where(statement, args...).
		Order("id ASC").Limit(limit + 1).Find(&events).Error
	if err != nil {
		return AuditPage{}, fmt.Errorf("查询审计事件失败：%w", translateSQLError(err))
	}

	page := AuditPage{Items: events}
	if len(events) > limit {
		page.Items = events[:limit]
		page.NextCursor = encodeAuditCursor(page.Items[len(page.Items)-1].ID)
	}
	return page, nil
}

// normalizeAuditLimit 归一化每页条数：为零取默认值，越界与非法值报错。
func normalizeAuditLimit(limit int) (int, error) {
	if limit == 0 {
		return DefaultAuditPageSize, nil
	}
	if limit < 0 || limit > MaxAuditPageSize {
		return 0, auditQueryError("每页条数必须在 1 至 " + strconv.Itoa(MaxAuditPageSize) + " 之间")
	}
	return limit, nil
}

// auditQueryConditions 把过滤条件转为 SQL 片段与参数，并校验枚举取值。
//
// 动作、对象类型与结果都按封闭枚举校验：非法值返回 400 而不是静默忽略，
// 否则调用方会以为过滤生效、实际拿到全量结果。
func auditQueryConditions(query AuditQuery) ([]string, []any, error) {
	var conditions []string
	var args []any

	if !query.From.IsZero() {
		conditions = append(conditions, "occurred_at >= ?")
		args = append(args, query.From.UTC())
	}
	if !query.To.IsZero() {
		conditions = append(conditions, "occurred_at <= ?")
		args = append(args, query.To.UTC())
	}
	if !query.From.IsZero() && !query.To.IsZero() && query.To.Before(query.From) {
		return nil, nil, auditQueryError("时间范围的结束不得早于开始")
	}
	if query.Action != "" {
		if err := checkFilterLength("action", query.Action); err != nil {
			return nil, nil, err
		}
		if _, ok := auditActions[query.Action]; !ok {
			return nil, nil, auditQueryError("动作不在封闭枚举内")
		}
		conditions = append(conditions, "action = ?")
		args = append(args, query.Action)
	}
	if query.ObjectType != "" {
		if err := checkFilterLength("objectType", query.ObjectType); err != nil {
			return nil, nil, err
		}
		if _, ok := objectTypes[query.ObjectType]; !ok {
			return nil, nil, auditQueryError("对象类型不在封闭枚举内")
		}
		conditions = append(conditions, "object_type = ?")
		args = append(args, query.ObjectType)
	}
	if query.Result != "" {
		if err := checkFilterLength("result", query.Result); err != nil {
			return nil, nil, err
		}
		if _, ok := auditResults[query.Result]; !ok {
			return nil, nil, auditQueryError("结果只能是成功、失败或被拒绝")
		}
		conditions = append(conditions, "result = ?")
		args = append(args, query.Result)
	}
	return conditions, args, nil
}

// checkFilterLength 限制单个过滤参数长度。
func checkFilterLength(name string, value string) error {
	if len(value) > maxAuditFilterLength {
		return auditQueryError("过滤参数 " + name + " 超出长度上限")
	}
	return nil
}

// encodeAuditCursor 把主键编码为不透明游标。
//
// 编码而非直接暴露自增主键：API.md §1.2 要求标识是不透明字符串，客户端不应
// 依赖其内部形态。用 base64 而非加密——游标不承载秘密，只求不被误当作序号。
func encodeAuditCursor(id uint64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatUint(id, 10)))
}

// decodeAuditCursor 解析游标；空游标表示从头开始。
func decodeAuditCursor(cursor string) (uint64, error) {
	if cursor == "" {
		return 0, nil
	}
	if len(cursor) > maxAuditFilterLength {
		return 0, auditQueryError("游标不合法")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, auditQueryError("游标不合法")
	}
	id, err := strconv.ParseUint(string(decoded), 10, 64)
	if err != nil {
		return 0, auditQueryError("游标不合法")
	}
	return id, nil
}

// AuditQueryError 携带可安全公开的中文说明。
//
// 用类型承载说明而不是靠解析错误文本：HTTP 层需要把说明放进 400 问题详情，
// 从拼接后的字符串里截取说明既脆弱又会把哨兵错误名带进响应。
type AuditQueryError struct {
	Detail string
}

func (err AuditQueryError) Error() string { return err.Detail }

// Unwrap 使 errors.Is(err, ErrAuditQueryInvalid) 成立。
func (err AuditQueryError) Unwrap() error { return ErrAuditQueryInvalid }

// auditQueryError 构造带中文说明的查询参数错误。
func auditQueryError(detail string) error {
	return AuditQueryError{Detail: detail}
}
