package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// 采集关闭时，数据面产生的元数据不得落库，且转发路径不因数据库不可用受影响。
func TestCaptureDisabledProducesNoRequestRecords(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")

	writer, err := NewRequestWriter(RequestWriterConfig{
		Enabled:       false,
		QueueSize:     16,
		BatchSize:     4,
		FlushInterval: 10 * time.Millisecond,
		Logger:        quietLogger(),
		Writer: func(ctx context.Context, records []RequestRecord) error {
			return store.Transaction(ctx, func(tx *Tx) error {
				return tx.SaveRequestRecords(ctx, records)
			})
		},
	})
	if err != nil {
		t.Fatalf("构造写入通道失败：%v", err)
	}
	writer.Start()

	// 关闭采集时，即使持续产生元数据也不得落库，且 Submit 不得阻塞。
	for index := 0; index < 1000; index++ {
		if writer.Submit(Metadata{ProxyID: "p1", Method: "GET", Level: MetadataLevelStatistic}) {
			t.Fatal("采集关闭时 Submit 不应接受记录")
		}
	}
	if err := writer.Close(context.Background()); err != nil {
		t.Fatalf("关闭写入通道失败：%v", err)
	}

	if count := mustCountRequestRecords(t, store); count != 0 {
		t.Fatalf("采集关闭时请求元数据表不得新增行，实际为 %d", count)
	}
	if count := mustCountBodySegments(t, store); count != 0 {
		t.Fatalf("采集关闭时正文分段索引不得新增行，实际为 %d", count)
	}
}

// 采集关闭时数据面必须完全不进入落库路径：即使落库函数必然失败也不受影响。
func TestCaptureDisabledDoesNotTouchSQLite(t *testing.T) {
	calls := 0
	writer, err := NewRequestWriter(RequestWriterConfig{
		Enabled:       false,
		QueueSize:     4,
		BatchSize:     2,
		FlushInterval: 5 * time.Millisecond,
		Logger:        quietLogger(),
		Writer: func(context.Context, []RequestRecord) error {
			calls++
			return errors.New("请求记录写入器不可用")
		},
	})
	if err != nil {
		t.Fatalf("构造写入通道失败：%v", err)
	}
	writer.Start()
	for index := 0; index < 200; index++ {
		writer.Submit(Metadata{ProxyID: "p1", Method: "GET"})
	}
	if err := writer.Close(context.Background()); err != nil {
		t.Fatalf("关闭写入通道失败：%v", err)
	}
	if calls != 0 {
		t.Fatalf("采集关闭时不得调用请求记录落库路径，实际调用 %d 次", calls)
	}
}

// 采集开启时按批写入；队列满时低等级统计降级丢弃，Submit 永不阻塞。
func TestCaptureEnabledBatchesAndDegradesWithoutBlocking(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")

	writer, err := NewRequestWriter(RequestWriterConfig{
		Enabled:       true,
		QueueSize:     8,
		BatchSize:     4,
		FlushInterval: 5 * time.Millisecond,
		Logger:        quietLogger(),
		Writer: func(ctx context.Context, records []RequestRecord) error {
			return store.Transaction(ctx, func(tx *Tx) error {
				return tx.SaveRequestRecords(ctx, records)
			})
		},
	})
	if err != nil {
		t.Fatalf("构造写入通道失败：%v", err)
	}
	// 不启动后台协程，队列必然被填满，用于验证 Submit 不阻塞且按等级降级。
	accepted := 0
	var waitGroup sync.WaitGroup
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()
		for index := 0; index < 5000; index++ {
			if writer.Submit(Metadata{ProxyID: "p1", Method: "GET", Level: MetadataLevelStatistic}) {
				accepted++
			}
		}
	}()

	done := make(chan struct{})
	go func() { waitGroup.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("队列满时 Submit 阻塞了数据面")
	}

	if writer.DroppedStatistics() == 0 {
		t.Fatal("队列满时应发生低等级统计降级丢弃并计数")
	}
	if writer.DroppedCritical() != 0 {
		t.Fatal("本用例只提交了低等级统计，关键记录不应被丢弃")
	}
	if accepted == 0 {
		t.Fatal("队列未满前应接受记录")
	}
	if accepted > 8 {
		t.Fatalf("有界队列容量为 8，接受条数不得超出：%d", accepted)
	}

	if err := writer.Close(context.Background()); err != nil {
		t.Fatalf("关闭写入通道失败：%v", err)
	}
	if count := mustCountRequestRecords(t, store); count != int64(accepted) {
		t.Fatalf("已入队记录应在关闭时落库：期望 %d，实际 %d", accepted, count)
	}
}

// 采集开启且启动后台协程时，元数据必须批量写入并计入统计。
func TestCaptureEnabledFlushesBatches(t *testing.T) {
	store := openServerStore(t, t.TempDir()+"/jrps.db")
	writer, err := NewRequestWriter(RequestWriterConfig{
		Enabled:       true,
		QueueSize:     64,
		BatchSize:     8,
		FlushInterval: 5 * time.Millisecond,
		Logger:        quietLogger(),
		Writer: func(ctx context.Context, records []RequestRecord) error {
			return store.Transaction(ctx, func(tx *Tx) error {
				return tx.SaveRequestRecords(ctx, records)
			})
		},
	})
	if err != nil {
		t.Fatalf("构造写入通道失败：%v", err)
	}
	writer.Start()

	total := 100
	accepted := 0
	for index := 0; index < total; index++ {
		if writer.Submit(Metadata{
			ProxyID: "p1", Method: "GET", Host: "example.invalid",
			OccurredAt: time.Now().UTC(), Level: MetadataLevelStatistic,
		}) {
			accepted++
		}
	}
	if err := writer.Close(context.Background()); err != nil {
		t.Fatalf("关闭写入通道失败：%v", err)
	}
	if accepted+int(writer.DroppedStatistics()) != total {
		t.Fatalf("接受与丢弃之和应等于提交总数：接受 %d，丢弃 %d，总数 %d",
			accepted, writer.DroppedStatistics(), total)
	}
	if writer.WrittenRecords() != uint64(accepted) {
		t.Fatalf("已入队记录应全部落库：接受 %d，落库 %d", accepted, writer.WrittenRecords())
	}
	if count := mustCountRequestRecords(t, store); count != int64(accepted) {
		t.Fatalf("请求元数据表行数不匹配：期望 %d，实际 %d", accepted, count)
	}
}

// 落库失败必须降级为计数与日志，不得阻塞或中断调用方。
func TestCaptureWriteFailureDoesNotBlock(t *testing.T) {
	writer, err := NewRequestWriter(RequestWriterConfig{
		Enabled:       true,
		QueueSize:     32,
		BatchSize:     4,
		FlushInterval: 5 * time.Millisecond,
		Logger:        quietLogger(),
		Writer: func(context.Context, []RequestRecord) error {
			return errors.New("磁盘空间不足，写入失败")
		},
	})
	if err != nil {
		t.Fatalf("构造写入通道失败：%v", err)
	}
	writer.Start()
	for index := 0; index < 32; index++ {
		writer.Submit(Metadata{ProxyID: "p1", Level: MetadataLevelStatistic})
	}
	if err := writer.Close(context.Background()); err != nil {
		t.Fatalf("关闭写入通道失败：%v", err)
	}
	if writer.FailedBatches() == 0 {
		t.Fatal("落库失败应被计数以便监控告警")
	}
}

func mustCountRequestRecords(t *testing.T, store *Store) int64 {
	t.Helper()
	var count int64
	if err := store.View(context.Background(), func(tx *Tx) error {
		var err error
		count, err = tx.CountRequestRecords()
		return err
	}); err != nil {
		t.Fatalf("统计请求元数据失败：%v", err)
	}
	return count
}

func mustCountBodySegments(t *testing.T, store *Store) int64 {
	t.Helper()
	var count int64
	if err := store.View(context.Background(), func(tx *Tx) error {
		var err error
		count, err = tx.CountBodySegments()
		return err
	}); err != nil {
		t.Fatalf("统计正文分段失败：%v", err)
	}
	return count
}

// 混用 desired 与 active：若 active 记录没有对应的成功 publish，恢复必须失败。
func TestRecoveryRejectsActiveWithoutSuccessfulPublish(t *testing.T) {
	path := t.TempDir() + "/jrps.db"
	store := openServerStore(t, path)

	if err := store.Transaction(context.Background(), func(tx *Tx) error {
		if _, err := tx.AppendRevision(RevisionInput{
			Content: "内容", Actor: ActorAdmin("admin"), Origin: OriginProxyCreate,
		}); err != nil {
			return err
		}
		// 只记录了一次未成功的 prepare，随后绕过 publish 直接推进 active。
		return tx.RecordApplyResult(ApplyResultInput{
			Revision: 1, Phase: PhasePrepare, Succeeded: false,
			ErrorDetail: "准备失败", Actor: ActorAdmin("admin"),
		})
	}); err != nil {
		t.Fatalf("准备不一致状态失败：%v", err)
	}
	if err := store.DB().Exec(
		"UPDATE revision_state SET active_revision = 1, last_good_revision = 1",
	).Error; err != nil {
		t.Fatalf("制造不一致 active 记录失败：%v", err)
	}

	_, err := store.LoadRevisionForRecovery(context.Background())
	if err == nil {
		t.Fatal("active 没有成功 publish 记录时恢复必须失败")
	}
	if !strings.Contains(err.Error(), "publish") {
		t.Fatalf("失败原因应指明缺少成功 publish：%v", err)
	}
}
