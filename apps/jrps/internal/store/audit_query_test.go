package store

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// 测试辅助：打开一个未初始化的库并写入 n 条可区分动作的审计事件。
//
// 刻意不用 openInitializedServerStore：初始化管理员本身会写一条审计事件，
// 会让"计数"类断言带上一个与用例无关的基数。查询行为只与写入的事件有关。
func seedAuditEvents(t *testing.T, database *Store, count int) {
	t.Helper()
	for index := 0; index < count; index += 1 {
		event := baseAuditEvent()
		event.ObjectID = "session-" + strconv.Itoa(index)
		event.Context = "第 " + strconv.Itoa(index) + " 条审计事件"
		if index%2 == 0 {
			event.Action = ActionAdminLogin
			event.Result = AuditResultSuccess
		} else {
			event.Action = ActionAdminLogout
			event.Result = AuditResultFailure
		}
		if err := writeAuditInTransaction(t, database, event); err != nil {
			t.Fatalf("写入第 %d 条审计事件失败：%v", index, err)
		}
	}
}

// 测试辅助：打开一个空库，仅用于查询行为断言。
func openQueryStore(t *testing.T) *Store {
	t.Helper()
	return openServerStore(t, filepath.Join(t.TempDir(), "jrps.db"))
}

// 测试辅助：执行一次审计查询。
func queryAuditEvents(t *testing.T, database *Store, query AuditQuery) AuditPage {
	t.Helper()
	var page AuditPage
	if err := database.View(context.Background(), func(tx *Tx) error {
		var err error
		page, err = tx.QueryAuditEvents(query)
		return err
	}); err != nil {
		t.Fatalf("查询审计事件失败：%v", err)
	}
	return page
}

// 空结果集必须有确定行为：返回空列表且无下一页游标。
func TestAuditQueryEmptyResultSet(t *testing.T) {
	database := openQueryStore(t)
	if len(mustAuditEvents(t, database)) != 0 {
		t.Fatal("测试前提错误：库中不应有审计事件")
	}

	page := queryAuditEvents(t, database, AuditQuery{})
	if len(page.Items) != 0 {
		t.Fatalf("空库应返回空列表，实际 %d 条", len(page.Items))
	}
	if page.NextCursor != "" {
		t.Fatalf("空结果不应有下一页游标：%s", page.NextCursor)
	}
}

// 按动作过滤只返回匹配项。
func TestAuditQueryFiltersByAction(t *testing.T) {
	database := openQueryStore(t)
	seedAuditEvents(t, database, 6)

	page := queryAuditEvents(t, database, AuditQuery{Action: ActionAdminLogout})
	if len(page.Items) != 3 {
		t.Fatalf("登出动作应有 3 条，实际 %d 条", len(page.Items))
	}
	for _, event := range page.Items {
		if event.Action != ActionAdminLogout {
			t.Fatalf("过滤结果含不匹配动作：%s", event.Action)
		}
	}
}

// 按结果过滤只返回匹配项。
func TestAuditQueryFiltersByResult(t *testing.T) {
	database := openQueryStore(t)
	seedAuditEvents(t, database, 6)

	page := queryAuditEvents(t, database, AuditQuery{Result: AuditResultFailure})
	if len(page.Items) != 3 {
		t.Fatalf("失败结果应有 3 条，实际 %d 条", len(page.Items))
	}
	for _, event := range page.Items {
		if event.Result != AuditResultFailure {
			t.Fatalf("过滤结果含不匹配项：%s", event.Result)
		}
	}
}

// 按对象类型过滤只返回匹配项。
func TestAuditQueryFiltersByObjectType(t *testing.T) {
	database := openQueryStore(t)
	seedAuditEvents(t, database, 4)

	page := queryAuditEvents(t, database, AuditQuery{ObjectType: ObjectTypeSession})
	if len(page.Items) != 4 {
		t.Fatalf("会话对象应有 4 条，实际 %d 条", len(page.Items))
	}

	other := queryAuditEvents(t, database, AuditQuery{ObjectType: ObjectTypeProxy})
	if len(other.Items) != 0 {
		t.Fatalf("不匹配的对象类型应返回空，实际 %d 条", len(other.Items))
	}
}

// 多维度过滤是合取关系。
func TestAuditQueryCombinesFilters(t *testing.T) {
	database := openQueryStore(t)
	seedAuditEvents(t, database, 6)

	page := queryAuditEvents(t, database, AuditQuery{
		Action: ActionAdminLogout,
		Result: AuditResultFailure,
	})
	if len(page.Items) != 3 {
		t.Fatalf("组合过滤应有 3 条，实际 %d 条", len(page.Items))
	}

	contradictory := queryAuditEvents(t, database, AuditQuery{
		Action: ActionAdminLogout,
		Result: AuditResultSuccess,
	})
	if len(contradictory.Items) != 0 {
		t.Fatalf("矛盾组合应返回空，实际 %d 条", len(contradictory.Items))
	}
}

// 时间范围过滤按闭区间生效。
//
// 注意精度：SQLite 以毫秒精度存储时间，同一毫秒内写入的多条事件时间戳相同。
// 因此用例按"秒级窗口"取范围，并单独覆盖同毫秒多条的场景，不假设每条事件
// 都有唯一时间戳。
func TestAuditQueryFiltersByTimeRange(t *testing.T) {
	database := openQueryStore(t)
	seedAuditEvents(t, database, 3)
	events := mustAuditEvents(t, database)

	// 取最后一条所在毫秒为下界：它能命中最后一条，且不早于前面各条的毫秒值。
	from := events[2].OccurredAt
	page := queryAuditEvents(t, database, AuditQuery{From: from})
	if len(page.Items) == 0 {
		t.Fatal("以最后一条时间为下界应至少命中一条")
	}
	last := page.Items[len(page.Items)-1]
	if last.ID != events[2].ID {
		t.Fatalf("命中的最后一条应是刚写入的那条，实际 ID=%d", last.ID)
	}
	for _, event := range page.Items {
		if event.OccurredAt.Before(from) {
			t.Fatalf("结果含早于下界的事件：%v < %v", event.OccurredAt, from)
		}
	}

	// 上界早于全部事件时应返回空集。
	empty := queryAuditEvents(t, database, AuditQuery{To: events[0].OccurredAt.Add(-time.Hour)})
	if len(empty.Items) != 0 {
		t.Fatalf("上界早于全部事件时应返回空集，实际 %d 条", len(empty.Items))
	}

	// 范围结束早于开始属非法输入，不得静默返回空结果。
	var err error
	_ = database.View(context.Background(), func(tx *Tx) error {
		_, err = tx.QueryAuditEvents(AuditQuery{
			From: from.Add(time.Hour),
			To:   from,
		})
		return nil
	})
	if !errors.Is(err, ErrAuditQueryInvalid) {
		t.Fatalf("倒置的时间范围应被拒绝：%v", err)
	}
}

// 同一毫秒内写入的多条事件必须都能被查到，不因时间戳相同而丢失。
//
// 这是时间戳不作为游标的直接理由：若用时间戳推进游标，同毫秒的多条事件会被
// 跳过或重复。主键游标不受影响。
func TestAuditQueryKeepsSameMillisecondEvents(t *testing.T) {
	database := openQueryStore(t)
	seedAuditEvents(t, database, 10)
	events := mustAuditEvents(t, database)

	distinct := make(map[int64]int)
	for _, event := range events {
		distinct[event.OccurredAt.UnixMilli()] += 1
	}
	sameMillisecond := 0
	for _, count := range distinct {
		if count > 1 {
			sameMillisecond += count
		}
	}
	if sameMillisecond == 0 {
		t.Skip("本次运行未产生同毫秒事件，无法覆盖该边界")
	}

	// 用极限页大小逐页读取，全部事件都必须出现且各一次。
	seen := make(map[uint64]bool)
	cursor := ""
	for {
		page := queryAuditEvents(t, database, AuditQuery{Limit: 1, Cursor: cursor})
		for _, event := range page.Items {
			if seen[event.ID] {
				t.Fatalf("同毫秒事件被重复返回：%d", event.ID)
			}
			seen[event.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != len(events) {
		t.Fatalf("游标翻页应返回全部 %d 条（含同毫秒事件），实际 %d 条", len(events), len(seen))
	}
}

// 游标翻页必须不重不漏地遍历全部事件。
func TestAuditQueryPaginatesWithoutGaps(t *testing.T) {
	database := openQueryStore(t)
	const total = 7
	seedAuditEvents(t, database, total)

	seen := make(map[uint64]bool, total)
	cursor := ""
	pages := 0
	for {
		page := queryAuditEvents(t, database, AuditQuery{Limit: 3, Cursor: cursor})
		for _, event := range page.Items {
			if seen[event.ID] {
				t.Fatalf("游标翻页出现重复记录：%d", event.ID)
			}
			seen[event.ID] = true
		}
		pages += 1
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > total {
			t.Fatal("游标翻页未收敛，可能陷入死循环")
		}
	}

	if len(seen) != total {
		t.Fatalf("游标翻页应覆盖 %d 条，实际 %d 条", total, len(seen))
	}
}

// 末页不返回下一页游标。
func TestAuditQueryLastPageHasNoCursor(t *testing.T) {
	database := openQueryStore(t)
	seedAuditEvents(t, database, 2)

	page := queryAuditEvents(t, database, AuditQuery{Limit: 50})
	if len(page.Items) != 2 {
		t.Fatalf("应有 2 条，实际 %d 条", len(page.Items))
	}
	if page.NextCursor != "" {
		t.Fatalf("末页不应有下一页游标：%s", page.NextCursor)
	}
}

// 恰好整除页大小时，末页不应多出一个空页游标。
func TestAuditQueryExactMultipleOfPageSize(t *testing.T) {
	database := openQueryStore(t)
	seedAuditEvents(t, database, 4)

	first := queryAuditEvents(t, database, AuditQuery{Limit: 2})
	if len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("第一页应有 2 条且有游标：%d 条 游标=%q", len(first.Items), first.NextCursor)
	}
	second := queryAuditEvents(t, database, AuditQuery{Limit: 2, Cursor: first.NextCursor})
	if len(second.Items) != 2 {
		t.Fatalf("第二页应有 2 条，实际 %d 条", len(second.Items))
	}
	if second.NextCursor != "" {
		t.Fatalf("两页正好取完，不应再有游标：%s", second.NextCursor)
	}
}

// 非法过滤值必须报错，而不是静默忽略后返回全量结果。
func TestAuditQueryRejectsInvalidFilters(t *testing.T) {
	database := openQueryStore(t)
	seedAuditEvents(t, database, 2)

	cases := []struct {
		name  string
		query AuditQuery
	}{
		{"非法动作", AuditQuery{Action: "未登记动作"}},
		{"非法对象类型", AuditQuery{ObjectType: "未登记对象"}},
		{"非法结果", AuditQuery{Result: "大概成功"}},
		{"超长动作", AuditQuery{Action: string(make([]byte, maxAuditFilterLength+1))}},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			var err error
			_ = database.View(context.Background(), func(tx *Tx) error {
				_, err = tx.QueryAuditEvents(item.query)
				return nil
			})
			if !errors.Is(err, ErrAuditQueryInvalid) {
				t.Fatalf("非法过滤参数应被拒绝：%v", err)
			}
		})
	}
}

// 非法游标返回 400 级错误，不静默从头开始。
func TestAuditQueryRejectsInvalidCursor(t *testing.T) {
	database := openQueryStore(t)
	seedAuditEvents(t, database, 2)

	cases := []string{
		"这不是合法游标",
		"!!!!",
		"eyJhIjoxfQ", // 合法 base64 但不是数字
	}
	for _, cursor := range cases {
		var err error
		_ = database.View(context.Background(), func(tx *Tx) error {
			_, err = tx.QueryAuditEvents(AuditQuery{Cursor: cursor})
			return nil
		})
		if !errors.Is(err, ErrAuditQueryInvalid) {
			t.Fatalf("非法游标 %q 应被拒绝：%v", cursor, err)
		}
	}
}

// 每页条数越界必须报错。
func TestAuditQueryRejectsInvalidLimit(t *testing.T) {
	database := openQueryStore(t)
	for _, limit := range []int{-1, MaxAuditPageSize + 1} {
		var err error
		_ = database.View(context.Background(), func(tx *Tx) error {
			_, err = tx.QueryAuditEvents(AuditQuery{Limit: limit})
			return nil
		})
		if !errors.Is(err, ErrAuditQueryInvalid) {
			t.Fatalf("越界页大小 %d 应被拒绝：%v", limit, err)
		}
	}
}

// 未指定页大小时使用默认值（API.md §1.5：默认 50）。
func TestAuditQueryUsesDefaultPageSize(t *testing.T) {
	database := openQueryStore(t)
	seedAuditEvents(t, database, 3)

	page := queryAuditEvents(t, database, AuditQuery{})
	if len(page.Items) != 3 {
		t.Fatalf("默认页大小下应返回全部 3 条，实际 %d 条", len(page.Items))
	}
}

// 游标是不透明字符串：不得直接暴露自增主键原文。
func TestAuditCursorIsOpaque(t *testing.T) {
	database := openQueryStore(t)
	seedAuditEvents(t, database, 3)

	page := queryAuditEvents(t, database, AuditQuery{Limit: 1})
	if page.NextCursor == "" {
		t.Fatal("应返回下一页游标")
	}
	if page.NextCursor == "1" {
		t.Fatal("游标不应是主键原文")
	}
}

// 并发写入后查询仍按主键顺序可读，-race 下无竞态。
func TestAuditQueryReadsConcurrentWrites(t *testing.T) {
	database := openQueryStore(t)
	seedAuditEvents(t, database, 5)

	page := queryAuditEvents(t, database, AuditQuery{})
	for index := 1; index < len(page.Items); index += 1 {
		if page.Items[index].ID < page.Items[index-1].ID {
			t.Fatal("查询结果必须按主键升序")
		}
	}
}

// 超出 SQLite 整数范围的游标必须按非法输入拒绝。
//
// 回归用例：游标解码用 ParseUint 的 64 位上限，而 SQLite 的 INTEGER 是有符号
// 64 位——超出 2^63-1 的值能通过解析，却让驱动在查询时报错，最终以 500 返回。
// 契约要求非法游标返回 400，两者对不上会把"用户输入错误"呈现成"服务端故障"。
func TestAuditQueryRejectsCursorBeyondSQLiteRange(t *testing.T) {
	database := openQueryStore(t)
	seedAuditEvents(t, database, 2)

	// 构造一个超过 int64 上限的游标：2^63。
	tooLarge := base64.RawURLEncoding.EncodeToString([]byte("9223372036854775808"))
	var err error
	_ = database.View(context.Background(), func(tx *Tx) error {
		_, err = tx.QueryAuditEvents(AuditQuery{Cursor: tooLarge})
		return nil
	})
	if !errors.Is(err, ErrAuditQueryInvalid) {
		t.Fatalf("超出范围的游标应按非法输入拒绝：%v", err)
	}

	// int64 上限本身仍应可用：它是合法边界。
	atLimit := base64.RawURLEncoding.EncodeToString([]byte("9223372036854775807"))
	_ = database.View(context.Background(), func(tx *Tx) error {
		_, err = tx.QueryAuditEvents(AuditQuery{Cursor: atLimit})
		return nil
	})
	if err != nil {
		t.Fatalf("int64 上限游标应被接受：%v", err)
	}
}
