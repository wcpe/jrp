package store

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// 投递结果查询的分页边界：与审计查询取同一档位，避免同类列表接口
// 出现两套翻页口径。
const (
	DefaultDeliveryPageSize = 50
	MaxDeliveryPageSize     = 200

	maxDeliveryFilterLength = 128
)

// ErrDeliveryQueryInvalid 表示投递结果查询参数不合法。
var ErrDeliveryQueryInvalid = errors.New("投递结果查询参数不合法")

// DeliveryQuery 是投递结果查询条件；零值表示不加该维度过滤。
type DeliveryQuery struct {
	TargetID  string
	EventType string
	Status    string

	Limit  int
	Cursor string

	// OnlyStopped 只看已停止重试的记录（failed 与 discarded）。
	// Web 通知页的"最终失败状态"用它把仍在重试的记录排除在外。
	OnlyStopped bool
}

// DeliveryPage 是投递结果查询结果页。
type DeliveryPage struct {
	Items      []NotificationOutbox
	NextCursor string
}

// QueryDeliveries 按条件查询投递记录，使用游标分页。
//
// 游标与排序都基于自增主键：同一毫秒内可能有多条记录，用时间戳做游标会在
// 边界上重复或漏读。返回按主键降序，使最近发生的投递最先可见。
func (tx *Tx) QueryDeliveries(query DeliveryQuery) (DeliveryPage, error) {
	limit, err := normalizeDeliveryLimit(query.Limit)
	if err != nil {
		return DeliveryPage{}, err
	}
	conditions, args, err := deliveryQueryConditions(query)
	if err != nil {
		return DeliveryPage{}, err
	}
	cursor, err := decodeDeliveryCursor(query.Cursor)
	if err != nil {
		return DeliveryPage{}, err
	}
	if cursor > 0 {
		conditions = append(conditions, "id < ?")
		args = append(args, cursor)
	}

	// 多取一条用于判断是否还有下一页，避免额外一次计数查询。
	var entries []NotificationOutbox
	statement := "1 = 1"
	if len(conditions) > 0 {
		statement = strings.Join(conditions, " AND ")
	}
	err = tx.db.Where(statement, args...).
		Order("id DESC").Limit(limit + 1).Find(&entries).Error
	if err != nil {
		return DeliveryPage{}, fmt.Errorf("查询投递记录失败：%w", translateSQLError(err))
	}

	page := DeliveryPage{Items: entries}
	if len(entries) > limit {
		page.Items = entries[:limit]
		page.NextCursor = encodeDeliveryCursor(page.Items[len(page.Items)-1].ID)
	}
	return page, nil
}

// normalizeDeliveryLimit 归一化每页条数：为零取默认值，越界与非法值报错。
func normalizeDeliveryLimit(limit int) (int, error) {
	if limit == 0 {
		return DefaultDeliveryPageSize, nil
	}
	if limit < 0 || limit > MaxDeliveryPageSize {
		return 0, deliveryQueryError("每页条数必须在 1 至 " + strconv.Itoa(MaxDeliveryPageSize) + " 之间")
	}
	return limit, nil
}

// deliveryQueryConditions 把过滤条件转为 SQL 片段与参数。
//
// 状态按封闭枚举校验：非法值返回 400 而不是静默忽略，否则调用方会以为
// 过滤生效、实际拿到全量结果。
func deliveryQueryConditions(query DeliveryQuery) ([]string, []any, error) {
	conditions := make([]string, 0, 4)
	args := make([]any, 0, 4)

	if query.OnlyStopped {
		stopped := []string{OutboxStatusFailed, OutboxStatusDiscarded}
		conditions = append(conditions, "status IN ?")
		args = append(args, stopped)
	}
	if query.TargetID != "" {
		if err := checkDeliveryFilterLength("targetId", query.TargetID); err != nil {
			return nil, nil, err
		}
		conditions = append(conditions, "target_id = ?")
		args = append(args, query.TargetID)
	}
	if query.EventType != "" {
		if err := checkDeliveryFilterLength("eventType", query.EventType); err != nil {
			return nil, nil, err
		}
		conditions = append(conditions, "event_type = ?")
		args = append(args, query.EventType)
	}
	if query.Status != "" {
		if err := checkDeliveryFilterLength("status", query.Status); err != nil {
			return nil, nil, err
		}
		if !IsOutboxStatus(query.Status) {
			return nil, nil, deliveryQueryError("状态不在封闭枚举内")
		}
		conditions = append(conditions, "status = ?")
		args = append(args, query.Status)
	}
	return conditions, args, nil
}

// checkDeliveryFilterLength 限制单个过滤参数长度。
func checkDeliveryFilterLength(name string, value string) error {
	if len(value) > maxDeliveryFilterLength {
		return deliveryQueryError("过滤参数 " + name + " 超出长度上限")
	}
	return nil
}

// encodeDeliveryCursor 把主键编码为不透明游标，形态与审计查询一致。
func encodeDeliveryCursor(id uint64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatUint(id, 10)))
}

// decodeDeliveryCursor 解析游标；空游标表示从头开始。
//
// 上界取 int64 最大值：SQLite 的 INTEGER 是有符号 64 位，超范围游标会让驱动
// 报错并以 500 返回，而它的性质是"非法输入"，契约要求 400。
func decodeDeliveryCursor(cursor string) (uint64, error) {
	if cursor == "" {
		return 0, nil
	}
	if len(cursor) > maxDeliveryFilterLength {
		return 0, deliveryQueryError("游标不合法")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, deliveryQueryError("游标不合法")
	}
	id, err := strconv.ParseUint(string(decoded), 10, 64)
	if err != nil {
		return 0, deliveryQueryError("游标不合法")
	}
	if id > math.MaxInt64 {
		return 0, deliveryQueryError("游标不合法")
	}
	return id, nil
}

// DeliveryQueryError 携带可安全公开的中文说明。
type DeliveryQueryError struct {
	Detail string
}

func (err DeliveryQueryError) Error() string { return err.Detail }

// Unwrap 使 errors.Is(err, ErrDeliveryQueryInvalid) 成立。
func (err DeliveryQueryError) Unwrap() error { return ErrDeliveryQueryInvalid }

func deliveryQueryError(detail string) error {
	return DeliveryQueryError{Detail: detail}
}
