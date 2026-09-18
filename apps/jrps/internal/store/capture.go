package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// 请求元数据的等级：队列满时按等级降级，低等级统计先被丢弃。
const (
	MetadataLevelStatistic = "statistic"
	MetadataLevelCritical  = "critical"
)

// Metadata 是数据面回流的请求元数据；只在采集开启时产生（ADR-0007）。
type Metadata struct {
	ClientID      string
	ProxyID       string
	OccurredAt    time.Time
	Method        string
	Host          string
	PathDigest    string
	StatusCode    int
	RequestBytes  int64
	ResponseBytes int64
	DurationMS    int64
	ResultClass   string
	Level         string
}

// BatchWriter 是把一批元数据写入 SQLite 的落库函数。
type BatchWriter func(ctx context.Context, records []RequestRecord) error

// RequestWriterConfig 是请求记录写入通道的配置。
type RequestWriterConfig struct {
	// Enabled 对应代理级采集开关；关闭时写入路径完全不启用。
	Enabled bool
	// QueueSize 是有界队列容量，必须为正。
	QueueSize int
	// BatchSize 是单批最大条数。
	BatchSize int
	// FlushInterval 是按时间刷写的间隔。
	FlushInterval time.Duration
	// Logger 是外壳日志器。
	Logger *slog.Logger
	// Writer 是实际落库函数。
	Writer BatchWriter
}

// RequestWriter 用有界队列与批处理把请求元数据写入 SQLite，与数据面隔离。
//
// 关键性质：Submit 永不阻塞转发。采集关闭时 Submit 直接返回且不接触任何落库路径，
// 因此数据转发路径不会产生任何请求元数据行。
type RequestWriter struct {
	enabled       bool
	queue         chan Metadata
	batchSize     int
	flushInterval time.Duration
	writer        BatchWriter
	logger        *slog.Logger

	started   atomic.Bool
	closed    atomic.Bool
	stop      chan struct{}
	done      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once

	droppedStatistics atomic.Uint64
	droppedCritical   atomic.Uint64
	writtenRecords    atomic.Uint64
	failedBatches     atomic.Uint64
}

// 单批落库的超时上限，避免数据库抖动无限拖住后台协程。
const flushTimeout = 10 * time.Second

// NewRequestWriter 构造写入通道；非法配置返回中文错误。
func NewRequestWriter(cfg RequestWriterConfig) (*RequestWriter, error) {
	if cfg.QueueSize <= 0 {
		return nil, errors.New("请求记录队列容量必须为正数")
	}
	if cfg.BatchSize <= 0 {
		return nil, errors.New("请求记录批量大小必须为正数")
	}
	if cfg.FlushInterval <= 0 {
		return nil, errors.New("请求记录刷写间隔必须为正数")
	}
	if cfg.Writer == nil {
		return nil, errors.New("请求记录落库函数不能为空")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &RequestWriter{
		enabled:       cfg.Enabled,
		queue:         make(chan Metadata, cfg.QueueSize),
		batchSize:     cfg.BatchSize,
		flushInterval: cfg.FlushInterval,
		writer:        cfg.Writer,
		logger:        logger,
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
	}, nil
}

// Enabled 返回采集开关状态。
func (w *RequestWriter) Enabled() bool { return w != nil && w.enabled }

// Start 启动后台批量写入协程；采集关闭时不做任何事。
func (w *RequestWriter) Start() {
	if !w.Enabled() || !w.started.CompareAndSwap(false, true) {
		return
	}
	w.wg.Add(1)
	go w.run()
}

// Submit 尝试入队一条元数据；永不阻塞。
//
// 采集关闭或已停止时直接丢弃且不计数，保证"关闭即不产生请求元数据行"。
// 队列满时按等级降级：低等级统计被丢弃并计数，关键记录被丢弃时额外写 WARN 日志。
func (w *RequestWriter) Submit(meta Metadata) bool {
	if !w.Enabled() || w.closed.Load() {
		return false
	}
	if meta.Level == "" {
		meta.Level = MetadataLevelStatistic
	}
	select {
	case w.queue <- meta:
		return true
	default:
		w.recordDrop(meta)
		return false
	}
}

// recordDrop 记录一次降级丢弃，不阻塞调用方。
func (w *RequestWriter) recordDrop(meta Metadata) {
	if meta.Level == MetadataLevelCritical {
		total := w.droppedCritical.Add(1)
		w.logger.Warn("请求元数据队列已满，关键记录被丢弃", "代理", meta.ProxyID, "累计丢弃", total)
		return
	}
	w.droppedStatistics.Add(1)
}

// DroppedStatistics 返回因队列满被丢弃的低等级统计条数。
func (w *RequestWriter) DroppedStatistics() uint64 {
	if w == nil {
		return 0
	}
	return w.droppedStatistics.Load()
}

// DroppedCritical 返回因队列满被丢弃的关键记录条数。
func (w *RequestWriter) DroppedCritical() uint64 {
	if w == nil {
		return 0
	}
	return w.droppedCritical.Load()
}

// WrittenRecords 返回已成功落库的条数。
func (w *RequestWriter) WrittenRecords() uint64 {
	if w == nil {
		return 0
	}
	return w.writtenRecords.Load()
}

// FailedBatches 返回落库失败的批次数，供指标与告警使用。
func (w *RequestWriter) FailedBatches() uint64 {
	if w == nil {
		return 0
	}
	return w.failedBatches.Load()
}

// run 循环按条数或时间间隔成批刷写；收到停止信号后把剩余记录落库。
func (w *RequestWriter) run() {
	defer w.wg.Done()
	defer close(w.done)
	ticker := time.NewTicker(w.flushInterval)
	defer ticker.Stop()

	batch := make([]Metadata, 0, w.batchSize)
	for {
		select {
		case <-w.stop:
			w.flush(batch)
			w.drainRemaining()
			return
		case <-ticker.C:
			w.flush(batch)
			batch = batch[:0]
		case meta := <-w.queue:
			batch = append(batch, meta)
			if len(batch) >= w.batchSize {
				w.flush(batch)
				batch = batch[:0]
			}
		}
	}
}

// drainRemaining 在停止时把队列中剩余记录一次性落库，避免静默丢弃已入队数据。
func (w *RequestWriter) drainRemaining() {
	remaining := make([]Metadata, 0, w.batchSize)
	for {
		select {
		case meta := <-w.queue:
			remaining = append(remaining, meta)
		default:
			w.flush(remaining)
			return
		}
	}
}

// flush 落库一批；失败只记录错误并计数，绝不阻塞或中断转发。
func (w *RequestWriter) flush(batch []Metadata) {
	if len(batch) == 0 {
		return
	}
	records := make([]RequestRecord, 0, len(batch))
	for _, meta := range batch {
		records = append(records, toRequestRecord(meta))
	}
	ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
	defer cancel()
	if err := w.writer(ctx, records); err != nil {
		w.failedBatches.Add(1)
		w.logger.Warn("请求元数据批量写入失败，本次丢弃", "条数", len(records), "错误", err)
		return
	}
	w.writtenRecords.Add(uint64(len(records)))
}

// Close 停止写入通道并把剩余记录落库；幂等。
func (w *RequestWriter) Close(_ context.Context) error {
	if w == nil {
		return nil
	}
	w.closeOnce.Do(func() {
		w.closed.Store(true)
		if !w.Enabled() {
			return
		}
		if !w.started.Load() {
			// 未启动后台协程时同步排空队列，避免已入队记录被静默丢弃。
			w.drainRemaining()
			return
		}
		close(w.stop)
		w.wg.Wait()
	})
	return nil
}

// toRequestRecord 把元数据转换为落库记录；正文段引用由采集实现补充。
func toRequestRecord(meta Metadata) RequestRecord {
	occurredAt := meta.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	return RequestRecord{
		ClientID:      meta.ClientID,
		ProxyID:       meta.ProxyID,
		OccurredAt:    occurredAt,
		Method:        meta.Method,
		Host:          meta.Host,
		PathDigest:    meta.PathDigest,
		StatusCode:    meta.StatusCode,
		RequestBytes:  meta.RequestBytes,
		ResponseBytes: meta.ResponseBytes,
		DurationMS:    meta.DurationMS,
		ResultClass:   meta.ResultClass,
	}
}

// SaveRequestRecords 批量写入请求元数据行。
func (tx *Tx) SaveRequestRecords(_ context.Context, records []RequestRecord) error {
	if len(records) == 0 {
		return nil
	}
	if err := tx.db.CreateInBatches(records, 100).Error; err != nil {
		return fmt.Errorf("写入请求元数据失败：%w", translateSQLError(err))
	}
	return nil
}

// CountRequestRecords 返回请求元数据行数，供采集隔离测试断言。
func (tx *Tx) CountRequestRecords() (int64, error) {
	var count int64
	if err := tx.db.Model(&RequestRecord{}).Count(&count).Error; err != nil {
		return 0, fmt.Errorf("统计请求元数据失败：%w", translateSQLError(err))
	}
	return count, nil
}

// CountBodySegments 返回正文分段索引行数，供采集隔离测试断言。
func (tx *Tx) CountBodySegments() (int64, error) {
	var count int64
	if err := tx.db.Model(&BodySegment{}).Count(&count).Error; err != nil {
		return 0, fmt.Errorf("统计正文分段失败：%w", translateSQLError(err))
	}
	return count, nil
}
