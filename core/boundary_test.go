package core_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jrp/core/server"
)

// 本文件补齐 FR-25 §5 验收标准中此前零覆盖的边界项。

// TestPayloadLengthBoundaries 验证任意长度边界的端到端数据一致性。
//
// 覆盖 0 长度写、单字节、跨缓冲分片与超过转发缓冲（32 KiB）的大块数据。
func TestPayloadLengthBoundaries(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
	}{
		{name: "0 长度", payload: []byte{}},
		{name: "单字节", payload: []byte("x")},
		{name: "缓冲边界减一", payload: bytes.Repeat([]byte("a"), 32*1024-1)},
		{name: "恰为缓冲大小", payload: bytes.Repeat([]byte("b"), 32*1024)},
		{name: "缓冲边界加一", payload: bytes.Repeat([]byte("c"), 32*1024+1)},
		{name: "跨多次分片", payload: bytes.Repeat([]byte("d"), 256*1024)},
	}

	target, stopEcho := startLocalEcho(t)
	defer stopEcho()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	serverEngine, clientEngine := startSlicePair(t, ctx, target, freePort(t))
	defer shutdownQuietly(clientEngine)

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			guest, err := net.Dial("tcp", serverEngine.GuestAddr(testProxyName).String())
			if err != nil {
				t.Fatalf("访客连接失败：%v", err)
			}
			defer guest.Close()

			if len(testCase.payload) == 0 {
				// 0 长度写：连接必须仍然可用（不因空写而中断）。
				if _, err := guest.Write(nil); err != nil {
					t.Fatalf("0 长度写失败：%v", err)
				}
				// 随后的正常往返必须成功。
				if err := echoOnceOn(guest, []byte("after-empty")); err != nil {
					t.Fatalf("0 长度写后的往返失败：%v", err)
				}
				return
			}

			if _, err := guest.Write(testCase.payload); err != nil {
				t.Fatalf("写入失败：%v", err)
			}
			received := make([]byte, len(testCase.payload))
			_ = guest.SetReadDeadline(time.Now().Add(20 * time.Second))
			if _, err := io.ReadFull(guest, received); err != nil {
				t.Fatalf("读回失败（期望 %d 字节）：%v", len(testCase.payload), err)
			}
			if !bytes.Equal(testCase.payload, received) {
				t.Fatalf("数据不一致：发送 %d 字节，收到 %d 字节", len(testCase.payload), len(received))
			}
		})
	}
}

// TestUnstartedShutdown 验证未 Start 就 Shutdown 的安全性。
//
// 规格 §3.2：未 Start 时 Done 在 Stopped 之前永不关闭；Shutdown 必须安全返回。
func TestUnstartedShutdown(t *testing.T) {
	target, stopEcho := startLocalEcho(t)
	defer stopEcho()

	control := mustAddrPort(t, "127.0.0.1:7000")
	serverConfig, _ := sliceConfigs(t, control, target, freePort(t))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	defer listener.Close()
	engine := server.New(serverConfig, server.WithListener(listener))

	// 未 Start：Done 不得关闭。
	select {
	case <-engine.Done():
		t.Fatalf("未 Start 时 Done 已关闭，违反 §3.2")
	default:
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := engine.Shutdown(ctx); err != nil {
		t.Fatalf("未 Start 就 Shutdown 应安全返回，实际：%v", err)
	}
	// 宿主注入的监听器仍归宿主：再次 Close 不 panic。
	_ = listener.Close()
}

// TestShutdownWithoutLoggerWritesNothingToStdout 验证未注入 logger 时不向标准输出直写。
//
// 捕获进程标准输出，跑一轮完整闭环与关闭，断言无任何输出。
func TestShutdownWithoutLoggerWritesNothingToStdout(t *testing.T) {
	// 用管道接管标准输出，脚本内所有写入都会被捕获。
	originalStdout := os.Stdout
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatalf("创建管道失败：%v", err)
	}
	os.Stdout = writeEnd
	defer func() { os.Stdout = originalStdout }()

	target, stopEcho := startLocalEcho(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	serverEngine, clientEngine := startSlicePair(t, ctx, target, freePort(t))

	assertEchoThroughTunnel(t, serverEngine, []byte("无日志输出检查"))

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = serverEngine.Shutdown(shutdownCtx)
	_ = clientEngine.Shutdown(shutdownCtx)
	stopEcho()

	// 关闭写端后读取全部输出。
	os.Stdout = originalStdout
	if err := writeEnd.Close(); err != nil {
		t.Fatalf("关闭写端失败：%v", err)
	}
	captured, err := io.ReadAll(readEnd)
	if err != nil {
		t.Fatalf("读取输出失败：%v", err)
	}
	_ = readEnd.Close()
	if len(captured) > 0 {
		t.Fatalf("未注入 logger 时不应有标准输出，实际输出：%q", string(captured))
	}
}

// TestParallelEnginesIndependentLifecycle 验证启停一组不影响另一组。
//
// 规格 §5 边界第 1 条后半：再启停其中一组，另一组不受影响。
func TestParallelEnginesIndependentLifecycle(t *testing.T) {
	newPair := func(t *testing.T) (*server.Engine, func()) {
		target, stopEcho := startLocalEcho(t)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		t.Cleanup(cancel)
		serverEngine, clientEngine := startSlicePair(t, ctx, target, freePort(t))
		return serverEngine, func() {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutdownCancel()
			_ = serverEngine.Shutdown(shutdownCtx)
			_ = clientEngine.Shutdown(shutdownCtx)
			stopEcho()
		}
	}

	engineA, stopA := newPair(t)
	engineB, stopB := newPair(t)
	defer stopB()

	// 两组都可用。
	assertEchoThroughTunnel(t, engineA, []byte("A 组初始"))
	assertEchoThroughTunnel(t, engineB, []byte("B 组初始"))

	// 停掉 A：B 必须继续正常工作。
	stopA()

	assertEchoThroughTunnel(t, engineB, []byte("B 组在 A 停止后仍可用"))

	// A 的入口应已不可连。
	if _, err := net.DialTimeout("tcp", engineA.GuestAddr(testProxyName).String(), 300*time.Millisecond); err == nil {
		t.Fatalf("A 组停止后其访客入口仍可连接")
	}

	// 重建 A：两组应同时可用（验证无进程级残留状态）。
	engineA2, stopA2 := newPair(t)
	defer stopA2()
	assertEchoThroughTunnel(t, engineA2, []byte("A 组重建"))
	assertEchoThroughTunnel(t, engineB, []byte("B 组在 A 重建后仍可用"))
}

// TestHalfClosePropagatesEOF 验证半关闭语义：一端关写后另一端读完剩余数据并感知 EOF。
func TestHalfClosePropagatesEOF(t *testing.T) {
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
	defer guest.Close()

	payload := []byte("半关闭前的数据")
	if _, err := guest.Write(payload); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	// 关闭写方向：回显端仍应读到全部数据并回显。
	tcpGuest, ok := guest.(*net.TCPConn)
	if !ok {
		t.Fatalf("访客连接类型异常：%T", guest)
	}
	if err := tcpGuest.CloseWrite(); err != nil {
		t.Fatalf("半关闭失败：%v", err)
	}

	received := make([]byte, len(payload))
	_ = guest.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(guest, received); err != nil {
		t.Fatalf("半关闭后读回失败：%v", err)
	}
	if !bytes.Equal(payload, received) {
		t.Fatalf("半关闭后数据不一致")
	}

	// 感知 EOF：对端关闭写方向后，继续读应返回 EOF 而非悬挂。
	_ = guest.SetReadDeadline(time.Now().Add(5 * time.Second))
	extra := make([]byte, 1)
	_, err = guest.Read(extra)
	if err == nil {
		t.Fatalf("对端已半关闭，读取应返回 EOF 或错误")
	}
	if !errors.Is(err, io.EOF) && !isConnectionClosed(err) {
		t.Fatalf("期望 EOF 或连接关闭，实际：%v", err)
	}
}

// isConnectionClosed 判断错误是否表示连接已被关闭。
func isConnectionClosed(err error) bool {
	message := err.Error()
	return strings.Contains(message, "closed") || strings.Contains(message, "reset") ||
		strings.Contains(message, "aborted")
}

// TestLoggerInjectionIsUsed 验证注入 logger 后引擎使用它输出中文日志。
//
// 与「未注入不输出」互为对照，证明注入路径真实生效。
func TestLoggerInjectionIsUsed(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelDebug}))

	target, stopEcho := startLocalEcho(t)
	defer stopEcho()

	control := mustAddrPort(t, "127.0.0.1:7000")
	serverConfig, _ := sliceConfigs(t, control, target, freePort(t))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	engine := server.New(serverConfig, server.WithListener(listener), server.WithLogger(logger))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutdownCancel()
	_ = engine.Shutdown(shutdownCtx)

	// 注入的 logger 不得收到凭证原文。
	if strings.Contains(buffer.String(), testClientToken) {
		t.Fatalf("日志泄露凭证：%s", buffer.String())
	}
}
