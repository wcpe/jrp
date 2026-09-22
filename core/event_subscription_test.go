package core_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/wcpe/jrp/core"
	"github.com/wcpe/jrp/core/client"
	"github.com/wcpe/jrp/core/server"
)

// 本文件是 FR-27「有界类型化事件订阅与只读状态快照」的首批失败测试（先红后绿）。
//
// 规格路径：docs/specs/core-event-subscription.md。按 ADR-0012，P1 仅有 core、
// core/server、core/client 三个公共导入路径：事件类型与订阅能力并入根包 core，
// 规格 §3.2 示例中的 event.* 前缀在代码里落为 core.*；Subscribe 挂在 core/server
// 与 core/client 的 Engine 上；State() 快照类型位于各自 Engine 包（服务端与客户端
// 快照内容不同，只读与深复制语义共用同一套约定），本文件一律推断快照类型、不点名，
// 快照内容断言只依赖规格 §3.5 列明的访问器。
//
// 关于 Apply 结果事件：规格 §3.3 将该事件命名为 ApplyResult，但该名字已被 FR-26
// 的 Apply 返回值类型（core.ApplyResult）占用，因此实现为独立事件类型
// core.ApplyResultEvent（字段 Revision、Stage、Err：错误取安全可公开摘要）。
// 本文件按实现形态断言；「切换已生效」按 Stage 直接判定，不依赖 FR-26 的 Published。
//
// 事件能力由并行实现落地（core/event.go 及两端 Engine 接线）。在实现完整接入
// Subscribe 与 State 之前，本文件的红表现为编译失败或断言失败，均属预期。
// 禁止为实现方便修改生产代码或弱化断言。

// 事件测试缓冲规模与超时常量：口径只在本文件使用，不与生产常量耦合。
const (
	// echoThroughputTolerance 是吞吐对比的宽裕容差：有订阅耗时不得超过无订阅的 5 倍。
	// 慢订阅者若阻塞数据面，回环代理耗时会成倍劣化，5 倍足以把它与 CI 抖动区分开。
	echoThroughputTolerance = 5
	// shutdownNoSlowConsumerTimeout 断言 Shutdown 不被慢消费者拖延的上限。
	// 数据面零流量时正常 Shutdown 为毫秒级；若实现同步投递或等待消费者，会被慢
	// 消费者的 sleep 阻塞远超该值。
	shutdownNoSlowConsumerTimeout = 5 * time.Second
	// eventWaitTimeout 是「事件应已产生」类轮询的总上限：连接建立与换代均为亚秒级，
	// 该值远大于正常耗时，只兜底实现彻底不发事件的情形。
	eventWaitTimeout = 10 * time.Second
	// eventPollInterval 是事件等待的轮询间隔。
	eventPollInterval = 20 * time.Millisecond
	// concurrentApplyRoundCount 是并发 Apply 期间快照一致性的换代轮数。
	concurrentApplyRoundCount = 20
	// eventCapacityOverLimit 是订阅容量的超上限取值：远超任何合理实现上限，
	// 用于验证 Capacity 收敛到 Core 上限而不是照单全收。
	eventCapacityOverLimit = 1 << 20
	// eventResyncWindowRounds 是连续溢出窗口内的换代次数：期间宿主不消费，
	// 构成同一个连续溢出窗口。
	eventResyncWindowRounds = 8
)

// resyncDrainTimeout 是连续溢出窗口观察期的时长；观察期结束后按窗口语义断言。
const resyncDrainTimeout = 5 * time.Second

// 断言消息由多个方向共用，集中定义避免文案漂移。
var (
	errEngineStopEventMissing = errors.New("engine stopped 后未收到 EngineStopped 事件")
	errEngineStopEventWrong   = errors.New("收到非 EngineStopped 的其他事件")
	errEngineStopErrMissing   = errors.New("EngineStopped 事件应携带 Err() 返回的最终错误")
)

// captureStdout 截获进程标准输出并执行 fn，返回期间直写的全部内容。
//
// 用 os.Pipe 替换 os.Stdout，读 goroutine 持续排空管道防止写端阻塞。
// 本文件不用 t.Parallel，串行替换标准输出是安全的。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("创建捕获管道失败：%v", err)
	}
	original := os.Stdout
	os.Stdout = writer
	defer func() {
		os.Stdout = original
	}()
	done := make(chan string, 1)
	go func() {
		contents, _ := io.ReadAll(reader)
		done <- string(contents)
	}()
	fn()
	_ = writer.Close()
	contents := <-done
	_ = reader.Close()
	return contents
}

// waitForEvent 轮询读取事件流，直到 found 对最新事件返回 true 或超时。
//
// 通过时返回已读事件的完整列表，供用例做后续断言；实现不发事件时以零长度列表
// 返回，由调用方决定失败口径。
func waitForEvent(t *testing.T, events <-chan core.Event, found func(core.Event) bool) []core.Event {
	t.Helper()
	var seen []core.Event
	deadline := time.Now().Add(eventWaitTimeout)
	for {
		if len(seen) > 0 && found(seen[len(seen)-1]) {
			return seen
		}
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatalf("订阅通道被提前关闭：已见 %d 个事件", len(seen))
			}
			seen = append(seen, event)
		case <-time.After(eventPollInterval):
			if time.Now().After(deadline) {
				return seen
			}
		}
	}
}

// drainUntilClosed 排空事件通道直到其关闭，返回期间读到的全部事件。
//
// 仅在引擎已停止、事件已定形后调用；超时未关闭即失败，避免实现缺陷让测试挂死。
func drainUntilClosed(t *testing.T, events <-chan core.Event, timeout time.Duration) []core.Event {
	t.Helper()
	var remaining []core.Event
	deadline := time.After(timeout)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return remaining
			}
			remaining = append(remaining, event)
		case <-deadline:
			t.Fatalf("%.2f 秒内订阅通道未关闭，排空中止", timeout.Seconds())
		}
	}
}

// waitForClosed 等待订阅通道关闭，返回从调用到关闭的耗时；关闭前收到的残余事件
// 被丢弃（顺序由调用方另行断言）。通道超时未关闭即失败，不会挂死。
func waitForClosed(t *testing.T, events <-chan core.Event) time.Duration {
	t.Helper()
	return drainLatency(t, events, shutdownNoSlowConsumerTimeout)
}

// drainLatency 排空通道直到关闭并返回耗时；超过 timeout 即失败。
func drainLatency(t *testing.T, events <-chan core.Event, timeout time.Duration) time.Duration {
	t.Helper()
	started := time.Now()
	deadline := time.After(timeout + 5*time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return time.Since(started)
			}
		case <-deadline:
			t.Fatalf("%.2f 秒内订阅通道未关闭", time.Since(started).Seconds())
		}
	}
}

// isEventStopped 判断事件是否为 EngineStopped。
func isEventStopped(event core.Event) bool {
	_, stopped := event.(core.EngineStopped)
	return stopped
}

// isEventResync 判断事件是否为 ResyncRequired。
func isEventResync(event core.Event) bool {
	_, resync := event.(core.ResyncRequired)
	return resync
}

// scanEvents 统计事件流中各类型的出现次数，读到 until 关闭或通道关闭为止。
func scanEvents(t *testing.T, events <-chan core.Event, until <-chan struct{}) (connected, proxyChanged, resync int) {
	t.Helper()
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return
			}
			switch event.(type) {
			case core.ClientConnected:
				connected++
			case core.ProxyStatusChanged:
				proxyChanged++
			case core.ResyncRequired:
				resync++
			}
		case <-until:
			return
		}
	}
}

// checkEventEnvelope 校验事件信封契约：等级取值在三级枚举内、类型标识非空、
// 时间戳非零（规格 §3.3）。
//
// 等级与具体类型的归属由实现判定并写入长期文档（规格 §6 待定项），因此只断言
// 成员资格，不断言具体映射。
func checkEventEnvelope(t *testing.T, event core.Event) {
	t.Helper()
	switch event.Level() {
	case core.LevelCritical, core.LevelNormal, core.LevelDiagnostic:
	default:
		t.Fatalf("事件 %T 的等级 %v 不在三级枚举内", event, event.Level())
	}
	if event.Type() == "" {
		t.Fatalf("事件 %T 的类型标识为空", event)
	}
	if event.At().IsZero() {
		t.Fatalf("事件 %T 的时间戳为零值", event)
	}
}

// measureEchoThroughput 完成一次完整 TCP 代理回显（建连、双向多块数据、半关闭读回），
// 返回总耗时。内容逐字节校验，防止把「连不上也算快」误判为高吞吐。
func measureEchoThroughput(t *testing.T, serverEngine *server.Engine, payloadChunk []byte, blockCount int) time.Duration {
	t.Helper()
	started := time.Now()
	guest, err := net.Dial("tcp", serverEngine.GuestAddr(testProxyName).String())
	if err != nil {
		t.Fatalf("访客连接失败：%v", err)
	}
	defer func() { _ = guest.Close() }()
	if tcpConn, ok := guest.(*net.TCPConn); ok {
		if err := tcpConn.SetNoDelay(true); err != nil {
			t.Fatalf("关闭 Nagle 失败：%v", err)
		}
	}
	for block := 0; block < blockCount; block++ {
		if _, err := guest.Write(payloadChunk); err != nil {
			t.Fatalf("第 %d 块写入失败：%v", block, err)
		}
	}
	if tcpConn, ok := guest.(*net.TCPConn); ok {
		if err := tcpConn.CloseWrite(); err != nil {
			t.Fatalf("半关闭失败：%v", err)
		}
	}
	received := make([]byte, len(payloadChunk)*blockCount)
	if _, err := io.ReadFull(guest, received); err != nil {
		t.Fatalf("读回失败：%v", err)
	}
	for index, value := range received {
		if value != payloadChunk[index%len(payloadChunk)] {
			t.Fatalf("第 %d 字节回显内容不一致", index)
		}
	}
	return time.Since(started)
}

// applyRevisionOnBoth 让两端各应用给定 revision 的换代配置并等两者返回。
// 同一引擎上不做并发 Apply；两端的 Apply 互相独立，与 apply_slice_test 同型。
func applyRevisionOnBoth(t *testing.T, serverEngine *server.Engine, clientEngine *client.Engine, serverConfig core.ServerConfig, clientConfig core.ClientConfig, newRevision uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), eventWaitTimeout)
	defer cancel()
	applyDone := make(chan error, 2)
	go func() {
		_, applyErr := serverEngine.Apply(ctx, server.Deployment{Revision: newRevision, Config: serverConfig})
		applyDone <- applyErr
	}()
	go func() {
		_, applyErr := clientEngine.Apply(ctx, client.Deployment{Revision: newRevision, Config: clientConfig})
		applyDone <- applyErr
	}()
	for range 2 {
		select {
		case applyErr := <-applyDone:
			if applyErr != nil {
				t.Fatalf("应用配置失败：%v", applyErr)
			}
		case <-time.After(eventWaitTimeout):
			t.Fatal("应用配置超时未返回")
		}
	}
}

// eventPair 是事件测试的引擎对上下文：引擎句柄与真实控制、回显目标地址。
// 换代配置据此重建（sliceConfigs），内容等价、仅访客端口轮换。
type eventPair struct {
	control netip.AddrPort
	target  netip.AddrPort
	server  *server.Engine
	client  *client.Engine
}

// applyRevisionBumpOnBoth 对两端各执行一次 revision 递增的换代：每次换代换一个新的
// 访客端口，其余内容保持等价。返回实际应用的 revision。
func applyRevisionBumpOnBoth(t *testing.T, pair *eventPair) uint64 {
	t.Helper()
	newRevision := pair.server.ActiveRevision() + 1
	serverConfig, clientConfig := sliceConfigs(t, pair.control, pair.target, freePort(t))
	applyRevisionOnBoth(t, pair.server, pair.client, serverConfig, clientConfig, newRevision)
	return newRevision
}

// eventPairTest 启动一对引擎并对其两端各创建一个订阅，回调里完成用例主体。
//
// 与 startSlicePair 的区别：本助手把真实控制地址与回显目标地址一并交给用例，
// 供换代配置重建使用。收尾时保证关闭订阅与引擎。
func eventPairTest(t *testing.T, body func(t *testing.T, pair *eventPair, serverSub *core.Subscription, clientSub *core.Subscription)) {
	t.Helper()
	target, stopEcho := startLocalEcho(t)
	defer stopEcho()
	ctx, cancel := context.WithTimeout(context.Background(), eventWaitTimeout)
	defer cancel()

	controlListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听控制端口失败：%v", err)
	}
	control := mustAddrPort(t, controlListener.Addr().String())
	serverConfig, clientConfig := sliceConfigs(t, control, target, freePort(t))

	serverEngine := server.New(serverConfig, server.WithListener(controlListener))
	if err := serverEngine.Start(ctx); err != nil {
		t.Fatalf("服务端启动失败：%v", err)
	}
	t.Cleanup(func() { _ = serverEngine.Shutdown(context.Background()) })

	// sliceConfigs 的客户端控制地址即真实监听地址，可直接使用。
	clientEngine := client.New(clientConfig)
	if err := clientEngine.Start(ctx); err != nil {
		t.Fatalf("客户端启动失败：%v", err)
	}
	t.Cleanup(func() { _ = clientEngine.Shutdown(context.Background()) })

	serverSub := serverEngine.Subscribe(core.Options{Capacity: 64})
	clientSub := clientEngine.Subscribe(core.Options{Capacity: 64})
	defer func() {
		serverSub.Close()
		clientSub.Close()
	}()
	body(t, &eventPair{control: control, target: target, server: serverEngine, client: clientEngine}, serverSub, clientSub)
}

// TestEventSlowSubscriberDoesNotThrottleProxy 验证慢订阅者不降低代理吞吐
// （规格 §5 错误路径第一条 + 正常路径第一、二条）。
//
// 断言强度：数据面在投递路径上做任何阻塞等待（等缓冲空、等消费者、锁内通知宿主）
// 都会让有订阅耗时显著劣化，5 倍宽裕容差内不红说明观测通道确实不拖累转发；
// Apply 结果事件若缺失或 revision 不符也会红。
func TestEventSlowSubscriberDoesNotThrottleProxy(t *testing.T) {
	eventPairTest(t, func(t *testing.T, pair *eventPair, serverSub *core.Subscription, clientSub *core.Subscription) {
		payloadChunk := bytes.Repeat([]byte("事件订阅吞吐压测-"), 1200) // 约 24 KiB
		blockCount := 16

		// 预热一轮：排除首次建连与路径冷启动对基准的干扰。
		if warmup := measureEchoThroughput(t, pair.server, payloadChunk, 1); warmup <= 0 {
			t.Fatal("预热回显耗时异常")
		}

		baseline := measureEchoThroughput(t, pair.server, payloadChunk, blockCount)

		// 慢订阅者：订阅后完全不消费，投递只依赖订阅缓冲自身容量。
		slowSub := pair.server.Subscribe(core.Options{Capacity: 64})
		defer slowSub.Close()

		withSubscription := measureEchoThroughput(t, pair.server, payloadChunk, blockCount)
		t.Logf("回显耗时：无订阅 %v，带慢订阅 %v（容差 %d 倍）", baseline, withSubscription, echoThroughputTolerance)
		if withSubscription > baseline*time.Duration(echoThroughputTolerance) {
			t.Fatalf("慢订阅者拖慢了数据面：无订阅 %v，带订阅 %v，超过 %d 倍容差",
				baseline, withSubscription, echoThroughputTolerance)
		}

		// 同一订阅窗口内的 Apply 结果事件必须送达且 revision 一致（§5 正常路径第一、二条）。
		revision := applyRevisionBumpOnBoth(t, pair)
		seen := waitForEvent(t, serverSub.Events(), func(event core.Event) bool {
			_, ok := event.(core.ApplyResultEvent)
			return ok
		})
		if len(seen) == 0 {
			t.Fatal("未收到任何事件：Apply 结果事件缺失")
		}
		applyEvent, ok := seen[len(seen)-1].(core.ApplyResultEvent)
		if !ok {
			t.Fatalf("最新事件不是 ApplyResult：%T", seen[len(seen)-1])
		}
		if applyEvent.Revision != revision {
			t.Fatalf("ApplyResult revision = %d，应为 %d", applyEvent.Revision, revision)
		}
		if applyEvent.Stage != core.StageApplied && applyEvent.Stage != core.StageDrained {
			t.Fatalf("ApplyResultEvent 阶段 %s 应表示切换已生效", applyEvent.Stage)
		}
		checkEventEnvelope(t, applyEvent)
	})
}

// TestEventOverflowTriggersResyncAndConsistentState 验证溢出降级与重同步
// （规格 §3.4 + §5 错误路径第二条）。
//
// 断言强度：溢出未发布 ResyncRequired、丢弃计数为 0、投递阻塞或 panic、或 State()
// 给出与 Apply 结果不一致的 revision，任一情况都会红。
func TestEventOverflowTriggersResyncAndConsistentState(t *testing.T) {
	eventPairTest(t, func(t *testing.T, pair *eventPair, serverSub *core.Subscription, clientSub *core.Subscription) {
		// 仅保留 1 个缓冲位：第一次 Apply 结果恰好装满缓冲，第二次必然溢出。
		overflowSub := pair.server.Subscribe(core.Options{Capacity: 1})
		defer overflowSub.Close()

		var revision uint64
		for range 2 {
			revision = applyRevisionBumpOnBoth(t, pair)
		}

		seen := waitForEvent(t, overflowSub.Events(), isEventResync)
		resyncCount := 0
		var dropped uint64
		for _, event := range seen {
			if resync, ok := event.(core.ResyncRequired); ok {
				resyncCount++
				dropped = resync.Dropped
			}
		}
		if resyncCount == 0 {
			t.Fatalf("缓冲溢出后未收到 ResyncRequired，实际事件数 %d", len(seen))
		}
		if dropped == 0 {
			t.Fatal("ResyncRequired 携带的丢弃计数为 0")
		}

		// 宿主据此调用 State() 重建视图：必须与 Apply 实际结果一致。
		state := pair.server.State()
		if state.ActiveRevision() != revision || state.LastGoodRevision() != revision {
			t.Fatalf("重同步视图不一致：active=%d last-good=%d，Apply 结果为 %d",
				state.ActiveRevision(), state.LastGoodRevision(), revision)
		}
	})
}

// TestEventResyncPublishedOncePerOverflowWindow 验证同一连续溢出窗口内
// ResyncRequired 只发布一次（规格 §3.4 + §5 错误路径第三条）。
//
// 断言强度：实现若对每次丢弃都发布一条重同步事件，resync 计数会远大于 1 而红。
func TestEventResyncPublishedOncePerOverflowWindow(t *testing.T) {
	eventPairTest(t, func(t *testing.T, pair *eventPair, serverSub *core.Subscription, clientSub *core.Subscription) {
		overflowSub := pair.server.Subscribe(core.Options{Capacity: 1})
		defer overflowSub.Close()

		// 连续多次换代，期间宿主不消费：构成同一个连续溢出窗口。
		for range eventResyncWindowRounds {
			applyRevisionBumpOnBoth(t, pair)
		}

		until := make(chan struct{})
		go func() {
			time.Sleep(resyncDrainTimeout)
			close(until)
		}()
		_, _, resyncCount := scanEvents(t, overflowSub.Events(), until)
		if resyncCount == 0 {
			t.Fatal("连续溢出后未收到 ResyncRequired")
		}
		if resyncCount != 1 {
			t.Fatalf("同一连续溢出窗口内 ResyncRequired 发布了 %d 次，应为 1 次", resyncCount)
		}
	})
}

// TestEventCriticalPrioritySurvivesFullBuffer 验证关键级事件优先
// （规格 §3.4 + §5 错误路径第四条）。
//
// 断言强度：实现若按「缓冲满即丢新事件」降级，EngineStopped 将缺失而红；若丢弃时
// 不区分等级（不保证关键级存活），EngineStopped 同样缺失；若降级时清空常规级而不
// 是只腾一个位置，常规级将无一存活。换代期间代理状态事件的个数与等级归属由实现
// 判定（规格 §6 待定项），因此对常规级存活数只设下界而不精确计数。
func TestEventCriticalPrioritySurvivesFullBuffer(t *testing.T) {
	target, stopEcho := startLocalEcho(t)
	defer stopEcho()
	controlListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听控制端口失败：%v", err)
	}
	control := mustAddrPort(t, controlListener.Addr().String())
	serverConfig, clientConfig := sliceConfigs(t, control, target, freePort(t))

	// 手工组装而非 eventPairTest：本用例依赖服务端干净 Shutdown 产生关键级事件，
	// 需要自行控制关闭时机。
	serverEngine := server.New(serverConfig, server.WithListener(controlListener))
	ctx, cancel := context.WithTimeout(context.Background(), eventWaitTimeout)
	defer cancel()
	if err := serverEngine.Start(ctx); err != nil {
		t.Fatalf("服务端启动失败：%v", err)
	}
	clientEngine := client.New(clientConfig)
	if err := clientEngine.Start(ctx); err != nil {
		t.Fatalf("客户端启动失败：%v", err)
	}
	t.Cleanup(func() { _ = clientEngine.Shutdown(context.Background()) })
	pair := &eventPair{control: control, target: target, server: serverEngine, client: clientEngine}

	// 引擎启动后再订阅：启动过程的连接事件已过，不进入本用例的缓冲填充计数。
	sub := serverEngine.Subscribe(core.Options{Capacity: 4})
	defer sub.Close()

	// 停止消费并用换代结果填满缓冲（成功应用结果为常规级事件）。
	for range 4 {
		applyRevisionBumpOnBoth(t, pair)
	}

	// 干净 Shutdown 发布关键级 EngineStopped：缓冲已满，关键级必须挤掉最旧的
	// 常规级事件存活。
	if err := serverEngine.Shutdown(ctx); err != nil {
		t.Fatalf("服务端关闭失败：%v", err)
	}

	// 引擎已停止、事件已定形，此时开闸消费，读到通道关闭为止。
	seen := drainUntilClosed(t, sub.Events(), shutdownNoSlowConsumerTimeout)

	stoppedAt := -1
	for index, event := range seen {
		if isEventStopped(event) {
			stoppedAt = index
		}
	}
	if stoppedAt < 0 {
		t.Fatalf("缓冲已满时关键级事件未送达，实际事件数 %d", len(seen))
	}
	if stoppedAt != len(seen)-1 {
		t.Fatalf("EngineStopped 应是最后一条事件（位置 %d，共 %d 个）", stoppedAt, len(seen))
	}
	// 关键级入队按规格只挤掉一个最旧的常规级：缓冲内的其余常规级事件必须存活，
	// 整体清空即红（重同步事件不占常规级名额）。
	retainedNormal := 0
	for _, event := range seen {
		switch event.(type) {
		case core.ApplyResultEvent, core.ProxyStatusChanged:
			retainedNormal++
		}
	}
	if retainedNormal == 0 {
		t.Fatalf("常规级事件无一存活（共 %d 个事件）：降级不应整体清空常规级", len(seen))
	}
}

// TestEventStateSnapshotDeepCopyNoAlias 验证快照深复制无别名
// （规格 §3.5 + §5 资源与并发第三条）。
//
// 断言强度：快照若与 Core 内部结构共享切片/映射，或复用缓存对象，修改返回值或
// Core 后续变化就会反映到已取到的快照上而红；快照缺 Running、连接计数等字段同样红。
func TestEventStateSnapshotDeepCopyNoAlias(t *testing.T) {
	eventPairTest(t, func(t *testing.T, pair *eventPair, serverSub *core.Subscription, clientSub *core.Subscription) {
		// 换代前取快照：Core 后续变化不得影响已取到的值。
		serverBefore := pair.server.State()
		clientBefore := pair.client.State()

		// 快照内容完整性（规格 §3.5）：运行状态、revision、客户端与代理摘要、连接计数。
		if !serverBefore.Running || !clientBefore.Running {
			t.Fatal("运行中的引擎快照 Running 不为 true")
		}
		if serverBefore.ActiveRevision() != pair.server.ActiveRevision() ||
			clientBefore.ActiveRevision() != pair.client.ActiveRevision() {
			t.Fatal("快照 active revision 与引擎不一致")
		}
		if serverBefore.LastGoodRevision() != pair.server.LastGoodRevision() ||
			clientBefore.LastGoodRevision() != pair.client.LastGoodRevision() {
			t.Fatal("快照 last-good revision 与引擎不一致")
		}
		if len(serverBefore.Proxies) == 0 || len(clientBefore.Proxies) == 0 {
			t.Fatal("快照代理摘要为空")
		}
		if got := serverBefore.Proxies[0].Name; got != testProxyName {
			t.Fatalf("服务端快照代理名 %q 应为 %q", got, testProxyName)
		}
		if got := clientBefore.Proxies[0].Name; got != testProxyName {
			t.Fatalf("客户端快照代理名 %q 应为 %q", got, testProxyName)
		}
		// 客户端摘要断言只放在服务端视角：规格 §3.5 的「客户端摘要列表（标识、
		// 连接状态、代理数）」在服务端无歧义（已连接的客户端），客户端引擎视角
		// 的列表含义未定（规格允许两端快照内容不同），不在此断言其内容。
		if serverClients := serverBefore.Clients; len(serverClients) == 0 {
			t.Fatal("服务端快照客户端摘要为空")
		} else if got := serverClients[0].ID; got != testClientID {
			t.Fatalf("服务端快照客户端标识 %q 应为 %q", got, testClientID)
		}
		if serverBefore.ConnectionCount() < 0 || clientBefore.ConnectionCount() < 0 {
			t.Fatal("快照活动连接计数为负值")
		}

		// 宿主破坏返回值：改名与篡改摘要字段。客户端视角的列表语义未定，只在
		// 非空时污染其首元素，避免对合法空列表越界。
		serverBefore.Proxies[0].Name = "被污染的名称"
		serverBefore.Proxies[0].Status = "被污染的状态"
		if serverClients := serverBefore.Clients; len(serverClients) > 0 {
			serverClients[0].ID = "被污染的标识"
		}
		clientBefore.Proxies[0].Name = "被污染的名称"
		if clientClients := clientBefore.Clients; len(clientClients) > 0 {
			clientClients[0].ID = "被污染的标识"
		}

		// Core 推进：换代后引擎 revision 前移，旧快照必须保持换代前的值。
		revision := applyRevisionBumpOnBoth(t, pair)
		if serverBefore.ActiveRevision() == revision || clientBefore.ActiveRevision() == revision {
			t.Fatal("换代后旧快照的 revision 被连带更新：快照与 Core 内部存在别名共享")
		}

		// 再次读取：必须仍是 Core 的真实值，宿主此前的修改不得残留。
		freshServer := pair.server.State()
		if got := freshServer.Proxies[0].Name; got != testProxyName {
			t.Fatalf("修改返回值后服务端代理名被污染：%q，应为 %q", got, testProxyName)
		}
		if freshClients := freshServer.Clients; len(freshClients) > 0 {
			if got := freshClients[0].ID; got != testClientID {
				t.Fatalf("修改返回值后服务端客户端标识被污染：%q，应为 %q", got, testClientID)
			}
		}
		if freshServer.ActiveRevision() != revision || freshServer.LastGoodRevision() != revision {
			t.Fatalf("换代后新快照未反映真实状态：active=%d last-good=%d，应为 %d",
				freshServer.ActiveRevision(), freshServer.LastGoodRevision(), revision)
		}
		freshClient := pair.client.State()
		if got := freshClient.Proxies[0].Name; got != testProxyName {
			t.Fatalf("修改返回值后客户端代理名被污染：%q，应为 %q", got, testProxyName)
		}
		if freshClient.ActiveRevision() != revision || freshClient.LastGoodRevision() != revision {
			t.Fatalf("换代后客户端新快照未反映真实状态：active=%d last-good=%d，应为 %d",
				freshClient.ActiveRevision(), freshClient.LastGoodRevision(), revision)
		}
	})
}

// TestEventMultiSubscriberConcurrentCloseIsolated 验证多订阅者并发创建与关闭互不影响、
// 重复 Close 安全、关闭后通道关闭（规格 §3.2/§3.6 + §5 正常路径第三条 + 资源与并发第一条）。
//
// 断言强度：关闭竞争若影响其他订阅（通道提前关闭、丢事件）或让存活订阅收不到
// Apply 结果，都会红；-race 下数据竞争直接失败。
func TestEventMultiSubscriberConcurrentCloseIsolated(t *testing.T) {
	eventPairTest(t, func(t *testing.T, pair *eventPair, serverSub *core.Subscription, clientSub *core.Subscription) {
		subA := pair.server.Subscribe(core.Options{Capacity: 64})
		subB := pair.server.Subscribe(core.Options{Capacity: 64})
		subC := pair.server.Subscribe(core.Options{Capacity: 64})

		var closeGroup sync.WaitGroup
		for _, sub := range []*core.Subscription{subA, subB, subC} {
			closeGroup.Add(1)
			go func(target *core.Subscription) {
				defer closeGroup.Done()
				target.Close()
				target.Close() // 重复 Close 必须安全
			}(sub)
		}
		closeGroup.Wait()

		// 并发关闭风暴后，与本次风暴无关的存量订阅必须照常收到后续事件。
		revision := applyRevisionBumpOnBoth(t, pair)
		seen := waitForEvent(t, serverSub.Events(), func(event core.Event) bool {
			apply, ok := event.(core.ApplyResultEvent)
			return ok && apply.Revision == revision
		})
		if len(seen) == 0 {
			t.Fatal("并发关闭其他订阅者后，存量订阅未再收到 ApplyResult 事件")
		}

		// 已关闭订阅的通道必须已关闭且不再写入：waitForClosed 带超时，不会挂死。
		for index, sub := range []*core.Subscription{subA, subB, subC} {
			waitForClosed(t, sub.Events())
			select {
			case event, ok := <-sub.Events():
				if ok {
					t.Fatalf("第 %d 个已关闭订阅仍收到事件 %T", index+1, event)
				}
			default:
			}
		}
	})
}

// TestEventEngineStoppedOnShutdownAndNotDelayed 验证 Shutdown 时订阅先收到
// EngineStopped 再收到通道关闭、EngineStopped 携带最终错误、且 Shutdown 不被慢
// 消费者拖延（规格 §3.6 + §5 正常路径第四条 + 错误路径第一、五条）。
//
// 断言强度：顺序颠倒（先关通道后发事件）会让方向一红；Shutdown 等待慢消费者会超过
// 5 秒上限而红；EngineStopped 不携带 Err() 的最终错误会让方向二红。
//
// 注：EngineStopped 错误字段按 Go 惯例记为 Err；实现若用其他字段名需同步调整。
func TestEventEngineStoppedOnShutdownAndNotDelayed(t *testing.T) {
	target, stopEcho := startLocalEcho(t)
	defer stopEcho()
	controlListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听控制端口失败：%v", err)
	}
	control := mustAddrPort(t, controlListener.Addr().String())
	serverConfig, clientConfig := sliceConfigs(t, control, target, freePort(t))

	// 手工组装：Shutdown 语义用例自行控制关闭时机，不交给 t.Cleanup 兜底。
	serverEngine := server.New(serverConfig, server.WithListener(controlListener))
	ctx, cancel := context.WithTimeout(context.Background(), eventWaitTimeout)
	defer cancel()
	if err := serverEngine.Start(ctx); err != nil {
		t.Fatalf("服务端启动失败：%v", err)
	}
	clientEngine := client.New(clientConfig)
	if err := clientEngine.Start(ctx); err != nil {
		t.Fatalf("客户端启动失败：%v", err)
	}

	// 引擎运行期间订阅：不要求回放启动过程事件，顺序断言只看停止路径。
	serverSub := serverEngine.Subscribe(core.Options{Capacity: 64})
	clientSub := clientEngine.Subscribe(core.Options{Capacity: 64})
	slowSub := serverEngine.Subscribe(core.Options{Capacity: 64}) // 慢消费者：全程不出读

	// 服务端干净关闭：慢消费者完全不出读，Shutdown 不得等它。
	shutdownStarted := time.Now()
	if err := serverEngine.Shutdown(ctx); err != nil {
		t.Fatalf("服务端关闭失败：%v", err)
	}
	shutdownElapsed := time.Since(shutdownStarted)
	if shutdownElapsed > shutdownNoSlowConsumerTimeout {
		t.Fatalf("Shutdown 被慢消费者拖延：耗时 %v，超过上限 %v",
			shutdownElapsed, shutdownNoSlowConsumerTimeout)
	}

	// 方向一：干净关闭的顺序——EngineStopped 是最后一条事件，随后通道才关闭。
	serverSeen := drainUntilClosed(t, serverSub.Events(), shutdownNoSlowConsumerTimeout)
	stoppedServerAt := -1
	for index, event := range serverSeen {
		if isEventStopped(event) {
			stoppedServerAt = index
		}
	}
	if stoppedServerAt < 0 {
		t.Fatalf("服务端 %s（共 %d 个事件）", errEngineStopEventMissing.Error(), len(serverSeen))
	}
	if stoppedServerAt != len(serverSeen)-1 {
		t.Fatalf("服务端 EngineStopped 之后仍有事件（位置 %d，共 %d 个）：关闭顺序违反先通知后关通道",
			stoppedServerAt, len(serverSeen))
	}
	if elapsed := waitForClosed(t, serverSub.Events()); elapsed > shutdownNoSlowConsumerTimeout {
		t.Fatalf("服务端订阅通道关闭被拖延：%.2f 秒，超过上限 %v",
			elapsed.Seconds(), shutdownNoSlowConsumerTimeout)
	}

	// 慢消费者的通道同样要关闭，且缓冲内的 EngineStopped 仍可读出（先发布后关闭）。
	slowSeen := drainUntilClosed(t, slowSub.Events(), shutdownNoSlowConsumerTimeout)
	if len(slowSeen) == 0 || !isEventStopped(slowSeen[len(slowSeen)-1]) {
		t.Fatalf("慢消费者未在通道关闭前收到 EngineStopped，实际事件数 %d", len(slowSeen))
	}

	// 方向二：服务端关闭杀掉客户端控制会话，客户端异常停止——EngineStopped 必须携带
	// Err() 的最终错误（规格 §5 错误路径第五条）。
	clientSeen := waitForEvent(t, clientSub.Events(), isEventStopped)
	if len(clientSeen) == 0 {
		t.Fatal("客户端 " + errEngineStopEventMissing.Error())
	}
	stoppedClient, ok := clientSeen[len(clientSeen)-1].(core.EngineStopped)
	if !ok {
		t.Fatalf("客户端 %s：%T", errEngineStopEventWrong.Error(), clientSeen[len(clientSeen)-1])
	}
	clientFinal := clientEngine.Err()
	if clientFinal == nil {
		t.Fatal("客户端异常停止后 Err() 为 nil，无法验证 EngineStopped 携带最终错误")
	}
	if stoppedClient.Err == nil {
		t.Fatalf("%s：事件未携带错误", errEngineStopErrMissing.Error())
	}
	if !errors.Is(stoppedClient.Err, clientFinal) && stoppedClient.Err.Error() != clientFinal.Error() {
		t.Fatalf("%s：事件错误 %v 与 Err() %v 不一致", errEngineStopErrMissing.Error(),
			stoppedClient.Err, clientFinal)
	}

	// 收尾：客户端引擎已自行异常停止，显式 Shutdown 释放残余资源（幂等）。
	// 通道关闭时机对异常停止存在两种合法实现（异常停止时关闭，或随 Shutdown 关闭），
	// 因此在显式 Shutdown 之后再断言关闭：两种实现下都必须已关闭。
	if err := clientEngine.Shutdown(ctx); err != nil {
		t.Fatalf("客户端关闭失败：%v", err)
	}
	if elapsed := waitForClosed(t, clientSub.Events()); elapsed > shutdownNoSlowConsumerTimeout {
		t.Fatalf("客户端订阅通道关闭被拖延：%.2f 秒，超过上限 %v",
			elapsed.Seconds(), shutdownNoSlowConsumerTimeout)
	}
}

// TestEventStateSnapshotRevisionDuringConcurrentApply 验证快照在并发 Apply 期间的
// 一致性（规格 §5 边界第四条）。
//
// 断言强度：快照若读到半更新状态（revision 推进但代理摘要未切换，或反之），观察
// 集合检查与代理名检查会红；快照返回非法 revision 也会红。-race 覆盖并发安全。
func TestEventStateSnapshotRevisionDuringConcurrentApply(t *testing.T) {
	eventPairTest(t, func(t *testing.T, pair *eventPair, serverSub *core.Subscription, clientSub *core.Subscription) {
		// 合法 revision 集合预先算全：换代序列固定为 0..轮数，观察期间集合只读，
		// 观察者读到的 revision 只能是切换前或切换后的值。
		validRevisions := make(map[uint64]bool, concurrentApplyRoundCount+1)
		for revision := 0; revision <= concurrentApplyRoundCount; revision++ {
			validRevisions[uint64(revision)] = true
		}

		stop := make(chan struct{})
		var observeGroup sync.WaitGroup
		observe := func(name string, snapshot func() (uint64, string)) {
			defer observeGroup.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				revision, proxyName := snapshot()
				if !validRevisions[revision] {
					t.Errorf("%s快照读到非法 revision %d：只允许切换前或切换后的值", name, revision)
					return
				}
				// 摘要与 revision 同源：换代序列的代理名恒定，快照不得给出半更新结果。
				if proxyName != testProxyName {
					t.Errorf("%s快照读到半更新状态：revision %d 时代理名 %q", name, revision, proxyName)
					return
				}
			}
		}
		observeGroup.Add(2)
		go observe("服务端", func() (uint64, string) {
			state := pair.server.State()
			return state.ActiveRevision(), state.Proxies[0].Name
		})
		go observe("客户端", func() (uint64, string) {
			state := pair.client.State()
			return state.ActiveRevision(), state.Proxies[0].Name
		})

		for range concurrentApplyRoundCount {
			applyRevisionBumpOnBoth(t, pair)
		}
		close(stop)
		observeGroup.Wait()

		// 收尾一致性：两端引擎与快照收敛到同一 revision。
		if pair.server.ActiveRevision() != pair.client.ActiveRevision() {
			t.Fatalf("两端最终 revision 不一致：服务端 %d，客户端 %d",
				pair.server.ActiveRevision(), pair.client.ActiveRevision())
		}
		final := pair.server.ActiveRevision()
		if pair.server.State().ActiveRevision() != final || pair.client.State().ActiveRevision() != final {
			t.Fatalf("快照与引擎 revision 不一致：引擎 %d，服务端快照 %d，客户端快照 %d",
				final, pair.server.State().ActiveRevision(), pair.client.State().ActiveRevision())
		}
	})
}

// TestEventCapacityDefaultsAndClampToLimit 验证 Capacity 语义
// （规格 §3.2 + §5 边界第二条）。
//
// 断言强度：Capacity=0 报错、panic 或拒绝投递会红；超上限值不收敛导致缓冲无界
// 无法直接观测，通过「收敛后订阅仍正常收发」约束其行为。Core 上限与默认常量
// 尚不存在，实现时补齐常量后本用例无需改动（用例只依赖规格字面语义）。
func TestEventCapacityDefaultsAndClampToLimit(t *testing.T) {
	eventPairTest(t, func(t *testing.T, pair *eventPair, serverSub *core.Subscription, clientSub *core.Subscription) {
		// Capacity=0：使用 Core 默认常量，不返回错误（构造即成功）。
		defaultSub := pair.server.Subscribe(core.Options{})
		defer defaultSub.Close()
		// 超上限：收敛到上限，订阅仍可用。
		clampedSub := pair.server.Subscribe(core.Options{Capacity: eventCapacityOverLimit})
		defer clampedSub.Close()

		revision := applyRevisionBumpOnBoth(t, pair)
		for index, sub := range []*core.Subscription{defaultSub, clampedSub} {
			seen := waitForEvent(t, sub.Events(), func(event core.Event) bool {
				apply, ok := event.(core.ApplyResultEvent)
				return ok && apply.Revision == revision
			})
			if len(seen) == 0 {
				t.Fatalf("第 %d 个订阅未收到 ApplyResult 事件", index+1)
			}
		}
	})
}

// TestEventNoLoggerNoStdout 验证未注入 logger 时不向标准输出直写
// （规格 §3.6 + §5 边界第五条）。
//
// 断言强度：实现若在订阅创建、事件发布、订阅关闭或引擎停止路径上用
// fmt.Println/print 直写标准输出，捕获内容非空即红。
func TestEventNoLoggerNoStdout(t *testing.T) {
	target, stopEcho := startLocalEcho(t)
	defer stopEcho()
	ctx, cancel := context.WithTimeout(context.Background(), eventWaitTimeout)
	defer cancel()

	stdout := captureStdout(t, func() {
		controlListener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Errorf("监听控制端口失败：%v", err)
			return
		}
		control := mustAddrPort(t, controlListener.Addr().String())
		serverConfig, clientConfig := sliceConfigs(t, control, target, freePort(t))

		// 两端都不注入 logger：FR-25 引擎默认丢弃日志器，事件路径同样不得直写。
		serverEngine := server.New(serverConfig, server.WithListener(controlListener))
		if err := serverEngine.Start(ctx); err != nil {
			t.Errorf("服务端启动失败：%v", err)
			return
		}
		defer func() { _ = serverEngine.Shutdown(context.Background()) }()

		// 服务端运行期间创建订阅（覆盖订阅创建路径）。
		serverSub := serverEngine.Subscribe(core.Options{Capacity: 64})

		// sliceConfigs 的客户端控制地址即真实监听地址，可直接启动。
		clientEngine := client.New(clientConfig)
		if err := clientEngine.Start(ctx); err != nil {
			t.Errorf("客户端启动失败：%v", err)
			return
		}
		defer func() { _ = clientEngine.Shutdown(context.Background()) }()
		clientSub := clientEngine.Subscribe(core.Options{Capacity: 64})

		// 产生一轮真实事件：连接、Apply 结果、订阅关闭（覆盖事件发布与关闭路径）。
		_, _ = clientEngine.Apply(ctx, client.Deployment{Revision: 1, Config: clientConfig})
		serverSub.Close()
		clientSub.Close()
	})

	if stdout != "" {
		t.Fatalf("未注入 logger 时向标准输出直写了 %d 字节：%q", len(stdout), stdout)
	}
}
