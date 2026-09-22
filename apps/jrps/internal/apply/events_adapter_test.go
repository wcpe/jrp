package apply

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jrp/apps/jrps/internal/store"
	"github.com/wcpe/jrp/core"
)

// fakeSubscriber 是事件订阅端口的测试替身：持有真实 EventHub，可受控发布。
type fakeSubscriber struct {
	hub *core.EventHub
}

func newFakeSubscriber() *fakeSubscriber {
	return &fakeSubscriber{hub: core.NewEventHub()}
}

func (f *fakeSubscriber) Subscribe(options core.Options) *core.Subscription {
	return f.hub.Subscribe(options)
}

// waitForLogEvents 等待日志通道落库并读回全部事件。
func waitForLogEvents(t *testing.T, database *store.Store) []store.LogEvent {
	t.Helper()
	database.FlushLogEvents()
	var events []store.LogEvent
	cursor := ""
	for {
		var page store.LogPage
		if err := database.View(context.Background(), func(tx *store.Tx) error {
			var err error
			page, err = tx.QueryLogEvents(store.LogQuery{Component: "events", Limit: 100, Cursor: cursor})
			return err
		}); err != nil {
			t.Fatalf("读取事件日志失败：%v", err)
		}
		events = append(events, page.Items...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return events
}

// 适配器把 ClientConnected 映射为 INFO 运行日志并携带客户端标识。
func TestEventAdapterMapsClientConnected(t *testing.T) {
	database, err := store.Open(store.Config{
		Path:   filepath.Join(t.TempDir(), "jrps.db"),
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("打开数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	fake := newFakeSubscriber()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartEventAdapter(ctx, fake, database, slog.New(slog.DiscardHandler))

	fake.hub.Publish(core.ClientConnected{
		ClientID:   "c-acc",
		RemoteAddr: "127.0.0.1:1234",
		EventMeta:  core.NewEventMeta(),
	})

	deadline := time.Now().Add(3 * time.Second)
	var events []store.LogEvent
	for time.Now().Before(deadline) {
		events = waitForLogEvents(t, database)
		if len(events) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(events) != 1 {
		t.Fatalf("应有一条连接日志，实际 %d 条", len(events))
	}
	if events[0].Level != "INFO" || events[0].Event != "client-connected" {
		t.Fatalf("映射不符：%+v", events[0])
	}
	if events[0].ClientID != "c-acc" {
		t.Fatalf("日志应携带客户端标识：%+v", events[0])
	}
}

// ApplyResultEvent 失败映射为 WARN，成功映射为 INFO。
func TestEventAdapterMapsApplyResult(t *testing.T) {
	database, err := store.Open(store.Config{
		Path:   filepath.Join(t.TempDir(), "jrps.db"),
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("打开数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	fake := newFakeSubscriber()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartEventAdapter(ctx, fake, database, slog.New(slog.DiscardHandler))

	fake.hub.PublishApply(core.ApplyResultEvent{
		Revision: 3, Stage: core.StageHealthCheck,
		Err:       core.NewApplyError(core.StageHealthCheck, errors.New("端口被占用")),
		EventMeta: core.NewEventMeta(),
	})
	fake.hub.PublishApply(core.ApplyResultEvent{
		Revision: 4, Stage: core.StageDrained, EventMeta: core.NewEventMeta(),
	})

	deadline := time.Now().Add(3 * time.Second)
	var events []store.LogEvent
	for time.Now().Before(deadline) {
		events = waitForLogEvents(t, database)
		if len(events) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(events) != 2 {
		t.Fatalf("应有两条应用结果日志，实际 %d 条", len(events))
	}
	if events[0].Level != "WARN" || !strings.Contains(events[0].Message, "3") {
		t.Fatalf("失败应用应映射为 WARN 且含版本号：%+v", events[0])
	}
	if events[1].Level != "INFO" || !strings.Contains(events[1].Message, "4") {
		t.Fatalf("成功应用应映射为 INFO 且含版本号：%+v", events[1])
	}
}

// ResyncRequired 映射为一条 WARN：记录溢出与丢弃数，不逐条猜写缺失事件。
func TestEventAdapterMapsResyncRequired(t *testing.T) {
	database, err := store.Open(store.Config{
		Path:   filepath.Join(t.TempDir(), "jrps.db"),
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("打开数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	fake := newFakeSubscriber()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartEventAdapter(ctx, fake, database, slog.New(slog.DiscardHandler))

	fake.hub.Publish(core.ResyncRequired{Dropped: 7, EventMeta: core.NewEventMeta()})

	deadline := time.Now().Add(3 * time.Second)
	var events []store.LogEvent
	for time.Now().Before(deadline) {
		events = waitForLogEvents(t, database)
		if len(events) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(events) != 1 {
		t.Fatalf("溢出应产生恰好一条 WARN，实际 %d 条", len(events))
	}
	if events[0].Level != "WARN" || events[0].Event != "event-resync-required" {
		t.Fatalf("映射不符：%+v", events[0])
	}
	if !strings.Contains(events[0].Message, "7") {
		t.Fatalf("WARN 应记录丢弃计数：%s", events[0].Message)
	}
}

// EngineStopped 异常停止映射为 ERROR（PublishStop 会关闭全部订阅，故单发验证）。
func TestEventAdapterMapsEngineStopped(t *testing.T) {
	database, err := store.Open(store.Config{
		Path:   filepath.Join(t.TempDir(), "jrps.db"),
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("打开数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	fake := newFakeSubscriber()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartEventAdapter(ctx, fake, database, slog.New(slog.DiscardHandler))

	// PublishStop 发布 EngineStopped 后会关闭所有订阅（规格 §3.4：停止事件
	// 优先送达，通道随之关闭），因此只发布一次。
	fake.hub.PublishStop(errors.New("致命错误"))

	deadline := time.Now().Add(3 * time.Second)
	var events []store.LogEvent
	for time.Now().Before(deadline) {
		events = waitForLogEvents(t, database)
		if len(events) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(events) != 1 {
		t.Fatalf("应有一条停止日志，实际 %d 条", len(events))
	}
	if events[0].Level != "ERROR" || events[0].Event != "engine-stopped-abnormal" {
		t.Fatalf("异常停止应为 ERROR：%+v", events[0])
	}
}

// 未知事件类型被忽略：事件模型扩展不应让旧适配器崩溃或输出噪声。
func TestEventAdapterIgnoresUnknownEvent(t *testing.T) {
	database, err := store.Open(store.Config{
		Path:   filepath.Join(t.TempDir(), "jrps.db"),
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("打开数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	fake := newFakeSubscriber()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartEventAdapter(ctx, fake, database, slog.New(slog.DiscardHandler))

	fake.hub.Publish(unknownTestEvent{EventMeta: core.NewEventMeta()})
	time.Sleep(100 * time.Millisecond)

	if events := waitForLogEvents(t, database); len(events) != 0 {
		t.Fatalf("未知事件不应产生日志，实际 %d 条", len(events))
	}
}

// unknownTestEvent 是适配器不认识的测试事件。
type unknownTestEvent struct{ core.EventMeta }

func (unknownTestEvent) Type() core.Type   { return "unknown-test" }
func (unknownTestEvent) Level() core.Level { return core.LevelNormal }

// 编译期使用检查。
var _ = fmt.Sprintf
