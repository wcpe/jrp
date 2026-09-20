package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// 投递结果查询按主键降序返回，使最近发生的投递最先可见。
func TestQueryDeliveriesOrdersNewestFirst(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	enqueueDeliveries(t, store, 3)

	page, err := queryDeliveries(t, store, DeliveryQuery{})
	if err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("应返回 3 条，实际 %d", len(page.Items))
	}
	if page.Items[0].EventID != "evt-3" || page.Items[2].EventID != "evt-1" {
		t.Fatalf("顺序应为最新在前，实际首条 %s、末条 %s", page.Items[0].EventID, page.Items[2].EventID)
	}
}

// 游标翻页不重不漏：这一条是分页实现最容易出错的地方。
func TestQueryDeliveriesPaginatesWithoutGaps(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	enqueueDeliveries(t, store, 5)

	first, err := queryDeliveries(t, store, DeliveryQuery{Limit: 2})
	if err != nil {
		t.Fatalf("首页查询失败：%v", err)
	}
	if len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("首页应有 2 条且带游标：条数=%d 游标=%q", len(first.Items), first.NextCursor)
	}

	second, err := queryDeliveries(t, store, DeliveryQuery{Limit: 2, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("第二页查询失败：%v", err)
	}
	seen := map[string]bool{}
	for _, item := range first.Items {
		seen[item.EventID] = true
	}
	for _, item := range second.Items {
		if seen[item.EventID] {
			t.Fatalf("翻页出现重复记录：%s", item.EventID)
		}
		seen[item.EventID] = true
	}
	if len(seen) != 4 {
		t.Fatalf("两页应覆盖 4 条不重复记录，实际 %d", len(seen))
	}
}

// failed 与 discarded 都是"已停止重试"，stopped 过滤必须同时覆盖两者；
// 只查 failed 会让被丢弃的记录从页面上消失。
func TestQueryDeliveriesStoppedFilterCoversBothTerminalStates(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	enqueueDeliveries(t, store, 2)
	setOutboxStatus(t, store, "evt-1", OutboxStatusFailed)
	setOutboxStatus(t, store, "evt-2", OutboxStatusDiscarded)
	enqueueDeliveriesAfter(t, store, 1) // evt-3 保持 pending

	page, err := queryDeliveries(t, store, DeliveryQuery{OnlyStopped: true})
	if err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("停止态应返回 2 条，实际 %d", len(page.Items))
	}
	for _, item := range page.Items {
		if item.Status != OutboxStatusFailed && item.Status != OutboxStatusDiscarded {
			t.Fatalf("过滤结果混入非停止态：%s", item.Status)
		}
	}
}

// 枚举外的状态必须报错而不是静默忽略，否则调用方会以为过滤生效。
func TestQueryDeliveriesRejectsUnknownStatus(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")

	_, err := queryDeliveries(t, store, DeliveryQuery{Status: "不存在"})
	if !errors.Is(err, ErrDeliveryQueryInvalid) {
		t.Fatalf("枚举外状态应返回查询参数错误，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "状态不在封闭枚举内") {
		t.Fatalf("错误说明应为中文且可安全公开，实际 %q", err.Error())
	}
}

// 越界与非法游标都按非法输入处理，不落到 500。
func TestQueryDeliveriesRejectsInvalidLimitAndCursor(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")

	if _, err := queryDeliveries(t, store, DeliveryQuery{Limit: MaxDeliveryPageSize + 1}); !errors.Is(err, ErrDeliveryQueryInvalid) {
		t.Fatalf("越界条数应报错，实际 %v", err)
	}
	if _, err := queryDeliveries(t, store, DeliveryQuery{Limit: -1}); !errors.Is(err, ErrDeliveryQueryInvalid) {
		t.Fatalf("负数条数应报错，实际 %v", err)
	}
	if _, err := queryDeliveries(t, store, DeliveryQuery{Cursor: "不是游标"}); !errors.Is(err, ErrDeliveryQueryInvalid) {
		t.Fatalf("非法游标应报错，实际 %v", err)
	}
}

// 目标与事件类型过滤按精确匹配生效。
func TestQueryDeliveriesFiltersByTargetAndEventType(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	enqueueDeliveries(t, store, 3)

	page, err := queryDeliveries(t, store, DeliveryQuery{TargetID: "target-1"})
	if err != nil {
		t.Fatalf("按目标过滤失败：%v", err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("三个事件同属 target-1，应返回 3 条，实际 %d", len(page.Items))
	}

	page, err = queryDeliveries(t, store, DeliveryQuery{EventType: "notification_target_deleted"})
	if err != nil {
		t.Fatalf("按事件类型过滤失败：%v", err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("不存在该事件类型，应返回 0 条，实际 %d", len(page.Items))
	}
}

// enqueueDeliveries 写入 n 条待发送记录，事件标识依次编号。
func enqueueDeliveries(t *testing.T, store *Store, n int) {
	t.Helper()
	for index := 1; index <= n; index++ {
		eventID := "evt-" + string(rune('0'+index))
		err := store.Transaction(context.Background(), func(tx *Tx) error {
			_, err := tx.OutboxEnqueue(NotificationOutbox{
				EventID:   eventID,
				EventType: "notification_target_created",
				Payload:   `{"摘要":"测试"}`,
				TargetID:  "target-1",
			})
			return err
		})
		if err != nil {
			t.Fatalf("写入待发送记录 %s 失败：%v", eventID, err)
		}
	}
}

// enqueueDeliveriesAfter 追加写入一条记录，编号接在已有记录之后。
func enqueueDeliveriesAfter(t *testing.T, store *Store, index int) {
	t.Helper()
	err := store.Transaction(context.Background(), func(tx *Tx) error {
		_, err := tx.OutboxEnqueue(NotificationOutbox{
			EventID:   "evt-" + string(rune('0'+index+2)),
			EventType: "notification_target_created",
			Payload:   `{"摘要":"测试"}`,
			TargetID:  "target-1",
		})
		return err
	})
	if err != nil {
		t.Fatalf("追加待发送记录失败：%v", err)
	}
}

// setOutboxStatus 把指定事件的状态改为目标状态，用于构造终态记录。
func setOutboxStatus(t *testing.T, store *Store, eventID string, status string) {
	t.Helper()
	stoppedAt := time.Now().UTC()
	err := store.Transaction(context.Background(), func(tx *Tx) error {
		return tx.db.Model(&NotificationOutbox{}).
			Where("event_id = ?", eventID).
			Updates(map[string]any{"status": status, "attempts": 4, "stopped_at": stoppedAt}).Error
	})
	if err != nil {
		t.Fatalf("更新状态失败：%v", err)
	}
}

// queryDeliveries 在只读事务内执行查询，与 HTTP 层的调用方式一致。
func queryDeliveries(t *testing.T, store *Store, query DeliveryQuery) (DeliveryPage, error) {
	t.Helper()
	var page DeliveryPage
	err := store.View(context.Background(), func(tx *Tx) error {
		var queryErr error
		page, queryErr = tx.QueryDeliveries(query)
		return queryErr
	})
	return page, err
}
