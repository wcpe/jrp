package core_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/wcpe/jrp/core/server"
)

// 本文件为验收发现的阻塞项提供确定性复现。实现修复前这些测试必须失败。

// 阻塞 P1：正常 Shutdown 后 Err() 必须为 nil。
//
// 缺陷行为：连接关闭触发的读错误被 failAbnormal 抢占为 finalErr，
// 而 closeDone 不清理它，导致 Shutdown 后 Err() 返回传输错误而非 nil。
func TestNormalShutdownKeepsErrNil(t *testing.T) {
	target, stopEcho := startLocalEcho(t)
	defer stopEcho()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverEngine, clientEngine := startSlicePair(t, ctx, target, freePort(t))

	// 跑一轮真实流量，让桥接与转发路径都激活。
	assertEchoThroughTunnel(t, serverEngine, []byte("排水前流量"))

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := serverEngine.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("服务端 Shutdown 失败：%v", err)
	}
	if err := clientEngine.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("客户端 Shutdown 失败：%v", err)
	}
	if err := serverEngine.Err(); err != nil {
		t.Fatalf("正常 Shutdown 后服务端 Err() 应为 nil，实际：%v", err)
	}
	if err := clientEngine.Err(); err != nil {
		t.Fatalf("正常 Shutdown 后客户端 Err() 应为 nil，实际：%v", err)
	}
}

// 阻塞 P2：Shutdown 期间活动流应保持连续，直到流自然结束或到达排水上限。
//
// 缺陷行为：stopListenersLocked 在 waitDrained 之前就 closeConns，
// 活动流在 Shutdown 发起瞬间即断裂。
//
// 本测试的期望分两段：排水期内逐次回显必须成功；随后主动结束流，
// Shutdown 应在流结束后正常收敛（不空等到排水上限）。
func TestShutdownDrainKeepsActiveStream(t *testing.T) {
	target, stopEcho := startLocalEcho(t)
	defer stopEcho()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	serverEngine, clientEngine := startSlicePair(t, ctx, target, freePort(t))
	defer shutdownQuietly(clientEngine)

	guest, err := net.Dial("tcp", serverEngine.GuestAddr(testProxyName).String())
	if err != nil {
		t.Fatalf("访客连接失败：%v", err)
	}
	if err := echoOnceOn(guest, []byte("A")); err != nil {
		_ = guest.Close()
		t.Fatalf("首轮回显失败：%v", err)
	}

	// 异步 Shutdown：排水期内活动流必须持续可用。
	shutdownDone := make(chan error, 1)
	go func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer shutdownCancel()
		shutdownDone <- serverEngine.Shutdown(shutdownCtx)
	}()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if err := echoOnceOn(guest, []byte("B")); err != nil {
			_ = guest.Close()
			t.Fatalf("排水期活动流断裂：%v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// 排水期验证完毕：主动结束流，Shutdown 应随之收敛而非空等。
	_ = guest.Close()

	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("流结束后 Shutdown 应正常收敛，实际：%v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatalf("流已结束但 Shutdown 未在排水上限内收敛")
	}
}

// 阻塞 P3：Start 失败必须返回可判定的包装哨兵错误。
//
// 缺陷行为：监听器不可用时返回裸 errors.New，errors.Is 无法判定。
func TestStartFailureReturnsWrappedSentinel(t *testing.T) {
	target, stopEcho := startLocalEcho(t)
	defer stopEcho()

	control := mustAddrPort(t, "127.0.0.1:7000")
	serverConfig, _ := sliceConfigs(t, control, target, freePort(t))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	_ = listener.Close()

	engine := server.New(serverConfig, server.WithListener(listener))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	startErr := engine.Start(ctx)
	if startErr == nil {
		t.Fatalf("已关闭的监听器应导致 Start 失败")
	}
	if !errors.Is(startErr, server.ErrNotStarted) {
		t.Fatalf("Start 失败应包装哨兵 ErrNotStarted，实际：%v", startErr)
	}
}

// assertEchoThroughTunnel 经隧道做一次回显校验。
func assertEchoThroughTunnel(t *testing.T, engine *server.Engine, payload []byte) {
	t.Helper()
	guest, err := net.Dial("tcp", engine.GuestAddr(testProxyName).String())
	if err != nil {
		t.Fatalf("访客连接失败：%v", err)
	}
	defer guest.Close()
	if err := echoOnceOn(guest, payload); err != nil {
		t.Fatalf("隧道回显失败：%v", err)
	}
}

// echoOnceOn 在已建立连接上写入并读回等长数据，带读超时。
func echoOnceOn(conn net.Conn, payload []byte) error {
	if _, err := conn.Write(payload); err != nil {
		return err
	}
	received := make([]byte, len(payload))
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	if _, err := io.ReadFull(conn, received); err != nil {
		return err
	}
	if !bytes.Equal(payload, received) {
		return errors.New("回显内容不一致")
	}
	return nil
}

// shutdownQuietly 在测试清理阶段静默关闭引擎。
func shutdownQuietly(engine interface{ Shutdown(context.Context) error }) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = engine.Shutdown(ctx)
}
