package store

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// LogSubmitter 是运行日志通道的提交口（FR-12 规格 §3.2）。
//
// 生产点只看到 SubmitLogEvent；缓冲满时按等级降级——DEBUG 与 INFO 丢弃并计数，
// WARN 与 ERROR 保留（挤掉最旧的低等级事件）。提交永远不阻塞：日志通道不得
// 拖慢数据面与管理面（规格 §2）。
type LogSubmitter struct {
	buffer   chan LogEvent
	batch    int
	notify   chan struct{}
	dropped  syncCounter
	flushNow chan chan struct{}

	closeOnce sync.Once
	closed    chan struct{}
	wg        sync.WaitGroup
}

// syncCounter 是极简的原子计数器封装（保持与 dropped 语义的单一出处）。
type syncCounter struct {
	mu sync.Mutex
	v  uint64
}

func (c *syncCounter) add(delta uint64) {
	c.mu.Lock()
	c.v += delta
	c.mu.Unlock()
}

func (c *syncCounter) load() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.v
}

// LogSubmitterConfig 是日志提交口的构造参数。
type LogSubmitterConfig struct {
	// BufferSize 是缓冲容量；为零取 DefaultLogBufferSize。
	BufferSize int
	// BatchSize 是单批落库条数；为零取 DefaultLogBatchSize。
	BatchSize int
	// Interval 是两次刷写的最大间隔。
	Interval time.Duration
}

// NewLogSubmitter 构造日志提交口并启动批量落库循环。
func NewLogSubmitter(config LogSubmitterConfig, flush func([]LogEvent) error) (*LogSubmitter, error) {
	if flush == nil {
		return nil, errors.New("日志落库回调不能为空")
	}
	bufferSize := config.BufferSize
	if bufferSize <= 0 {
		bufferSize = DefaultLogBufferSize
	}
	batchSize := config.BatchSize
	if batchSize <= 0 {
		batchSize = DefaultLogBatchSize
	}
	interval := config.Interval
	if interval <= 0 {
		interval = time.Second
	}

	submitter := &LogSubmitter{
		buffer:   make(chan LogEvent, bufferSize),
		batch:    batchSize,
		notify:   make(chan struct{}, 1),
		flushNow: make(chan chan struct{}, 1),
		closed:   make(chan struct{}),
	}
	submitter.wg.Add(1)
	go submitter.loop(flush, interval)
	return submitter, nil
}

// SubmitLogEvent 提交一条日志；返回假表示该事件被降级丢弃。
//
// 等级不在四级枚举内的事件直接拒绝：等级是查询契约的一部分，脏等级会让
// 过滤形同虚设。缓冲满时按等级降级（规格 §3.2）。
func (submitter *LogSubmitter) SubmitLogEvent(event LogEvent) bool {
	if submitter == nil {
		return false
	}
	if _, ok := logLevels[event.Level]; !ok {
		return false
	}
	logEventOccurredAtNow(&event)
	select {
	case <-submitter.closed:
		return false
	default:
	}
	select {
	case submitter.buffer <- event:
		submitter.kick()
		return true
	default:
	}
	// 缓冲满：低等级丢弃，高等级挤掉最旧的低等级事件。
	switch event.Level {
	case LogLevelDebug, LogLevelInfo:
		submitter.dropped.add(1)
		return false
	case LogLevelWarn, LogLevelError:
		submitter.evictLowLevel(event.Level)
		select {
		case submitter.buffer <- event:
			submitter.kick()
			return true
		default:
			submitter.dropped.add(1)
			return false
		}
	}
	return false
}

// evictLowLevel 从缓冲头部挤掉低等级事件为高等级事件腾位。
//
// 必须在缓冲满时调用：先取后放之间存在窗口，腾位失败时调用方兜底丢弃。
func (submitter *LogSubmitter) evictLowLevel(incoming string) {
	for {
		select {
		case buffered := <-submitter.buffer:
			if buffered.Level == LogLevelDebug || buffered.Level == LogLevelInfo {
				submitter.dropped.add(1)
				return
			}
			// 高等级事件保留原序：放回队尾不影响告警可见性。
			select {
			case submitter.buffer <- buffered:
			default:
				submitter.dropped.add(1)
			}
		default:
			return
		}
	}
}

// LogDropped 返回累计丢弃的低等级事件数（规格 §5：丢弃计数可见）。
func (submitter *LogSubmitter) LogDropped() uint64 {
	if submitter == nil {
		return 0
	}
	return submitter.dropped.load()
}

// FlushLogEvents 阻塞等待通道内已有事件全部落库（测试与关闭路径使用）。
func (submitter *LogSubmitter) FlushLogEvents() {
	if submitter == nil {
		return
	}
	done := make(chan struct{})
	select {
	case submitter.flushNow <- done:
		<-done
	case <-submitter.closed:
	}
}

// Close 停止接收并刷写剩余事件；重复调用安全。
func (submitter *LogSubmitter) Close() {
	if submitter == nil {
		return
	}
	submitter.closeOnce.Do(func() {
		close(submitter.closed)
		submitter.wg.Wait()
	})
}

// kick 通知批处理循环有新事件。
func (submitter *LogSubmitter) kick() {
	select {
	case submitter.notify <- struct{}{}:
	default:
	}
}

// loop 是批量落库循环：凑满一批或到间隔即刷写。
func (submitter *LogSubmitter) loop(flush func([]LogEvent) error, interval time.Duration) {
	defer submitter.wg.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	batch := make([]LogEvent, 0, submitter.batch)
	drain := func() {
		for len(batch) < submitter.batch {
			select {
			case event := <-submitter.buffer:
				batch = append(batch, event)
			default:
				return
			}
		}
	}
	write := func(events []LogEvent) {
		if len(events) == 0 {
			return
		}
		if err := flush(events); err != nil {
			// 落库失败不阻塞生产：按降级计数暴露，不重试（规格 §3.2）。
			submitter.dropped.add(uint64(len(events)))
		}
	}
	for {
		select {
		case <-submitter.notify:
			// 事件已入缓冲：等凑满一批或到间隔再刷写（批量落库语义）。
		case <-ticker.C:
			drain()
			write(batch)
			batch = batch[:0]
		case done := <-submitter.flushNow:
			// 排空整个缓冲：测试与关闭路径要求"缓冲内事件全部落库"。
			for {
				drain()
				if len(batch) == 0 {
					break
				}
				write(batch)
				batch = batch[:0]
			}
			close(done)
		case <-submitter.closed:
			drain()
			write(batch)
			return
		}
	}
}

// LogEventValidationError 是日志事件校验失败的可判定错误。
type LogEventValidationError struct{ Detail string }

func (err LogEventValidationError) Error() string { return err.Detail }

// ValidateLogEvent 校验一条日志事件的必填字段与等级合法性。
func ValidateLogEvent(event LogEvent) error {
	if _, ok := logLevels[event.Level]; !ok {
		return LogEventValidationError{Detail: fmt.Sprintf("等级 %q 不在四级枚举内", event.Level)}
	}
	if event.Component == "" {
		return LogEventValidationError{Detail: "组件名不能为空"}
	}
	if event.Event == "" {
		return LogEventValidationError{Detail: "事件名不能为空"}
	}
	if event.Message == "" {
		return LogEventValidationError{Detail: "消息不能为空"}
	}
	return nil
}
