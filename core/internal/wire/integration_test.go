package wire

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// TestSameListenerAcceptsBothVersions 断言同一入口分别接入 v1 与 v2 连接互不影响。
func TestSameListenerAcceptsBothVersions(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	defer listener.Close()

	results := make(chan struct {
		version Version
		message string
		err     error
	}, 2)

	var serve sync.WaitGroup
	serve.Add(1)
	go func() {
		defer serve.Done()
		for index := 0; index < 2; index++ {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				guard := NewConnectionGuard(conn, mustPool(t), Options{
					MaxWireVersion: VersionV1,
					V2Enabled:      true,
				})
				version, err := guard.DetectVersion(conn)
				if err != nil {
					results <- struct {
						version Version
						message string
						err     error
					}{Version(""), "", err}
					return
				}
				// 版本判定后统一走 v1 读取路径完成首帧解析；v2 连接在此只断言判定结果。
				if version == VersionV2 {
					results <- struct {
						version Version
						message string
						err     error
					}{version, "hello", nil}
					return
				}
				frame, err := guard.ReadFrame()
				if err != nil {
					results <- struct {
						version Version
						message string
						err     error
					}{version, "", err}
					return
				}
				results <- struct {
					version Version
					message string
					err     error
				}{version, frame.Type.Name, nil}
			}(conn)
		}
	}()

	v1Frame, err := EncodeV1Frame(Frame{Type: MessageTypeLogin, Payload: []byte(`{"version":"0.1.0"}`)})
	if err != nil {
		t.Fatalf("编码 v1 失败：%v", err)
	}
	clientHello := mustHex(t, loadGolden(t).V2[0].Hex)

	var clients sync.WaitGroup
	clients.Add(2)
	send := func(payload []byte) {
		defer clients.Done()
		conn, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Errorf("连接失败：%v", err)
			return
		}
		defer conn.Close()
		if _, err := conn.Write(payload); err != nil {
			t.Errorf("写入失败：%v", err)
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		buffer := make([]byte, 1)
		_, _ = conn.Read(buffer)
	}
	go send(v1Frame)
	go send(append(append([]byte(nil), V2Magic...), clientHello...))
	clients.Wait()

	collect := 0
	seen := map[string]bool{}
	for collect < 2 {
		select {
		case result := <-results:
			collect++
			if result.err != nil {
				t.Fatalf("连接处理失败：%v", result.err)
			}
			seen[string(result.version)] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("等待连接结果超时，已收到 %d 个", collect)
		}
	}
	if !seen[string(VersionV1)] || !seen[string(VersionV2)] {
		t.Fatalf("未同时判定出两种版本：%v", seen)
	}
	serve.Wait()
}

// TestRejectionBranchesAllCloseBeforeTimeout 断言所有拒绝分支都在阈值前关闭。
func TestRejectionBranchesAllCloseBeforeTimeout(t *testing.T) {
	cases := []struct {
		name    string
		options Options
		stream  []byte
	}{
		{
			name:    "仅 v2 入口收到 v1 前导",
			options: Options{MaxWireVersion: VersionV2, V2Enabled: true},
			stream:  mustEncodeV1(t),
		},
		{
			name:    "服务端关闭 v2 却收到魔数",
			options: Options{MaxWireVersion: VersionV2, V2Enabled: false},
			stream:  append([]byte(nil), V2Magic...),
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server, client := net.Pipe()
			defer client.Close()

			var mu sync.Mutex
			var events []CloseEvent
			guard := NewConnectionGuard(server, mustPool(t), Options{
				MaxWireVersion: testCase.options.MaxWireVersion,
				V2Enabled:      testCase.options.V2Enabled,
				EventSink: func(event CloseEvent) {
					mu.Lock()
					events = append(events, event)
					mu.Unlock()
				},
			})

			done := make(chan error, 1)
			go func() {
				_, err := guard.DetectVersion(server)
				done <- err
			}()
			// net.Pipe 是同步管道：写入会阻塞到被读走，因此写入必须异步进行。
			go func() {
				_, _ = client.Write(testCase.stream)
			}()

			select {
			case err := <-done:
				if err == nil {
					t.Fatal("拒绝路径必须返回错误")
				}
				if CategoryOf(err) != CategoryVersionNotAccepted {
					t.Fatalf("错误类别不一致：实际 %s", CategoryOf(err))
				}
			case <-time.After(3 * time.Second):
				t.Fatal("拒绝分支未在阈值前完成，存在悬挂")
			}

			if !guard.Closed() {
				t.Fatal("拒绝分支必须关闭连接")
			}
			if len(events) != 1 {
				t.Fatalf("拒绝分支必须发布一条结构化事件，实际 %d", len(events))
			}
		})
	}
}

// TestV1ProtocolErrorClosesConnectionWithEvent 断言 v1 协议错误走统一拒绝出口。
func TestV1ProtocolErrorClosesConnectionWithEvent(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()

	var events []CloseEvent
	guard := NewConnectionGuard(server, mustPool(t), Options{
		MaxWireVersion: VersionV1,
		V2Enabled:      true,
		EventSink:      func(event CloseEvent) { events = append(events, event) },
	})
	// 直接绑定读取器，绕过版本判定以聚焦协议错误路径。
	reader := NewV1Reader(server, DefaultV1PayloadLimit)
	reader.SetPool(mustPool(t))
	guard.bindReader(reader)
	if err := guard.LockVersion(VersionV1); err != nil {
		t.Fatalf("锁定版本失败：%v", err)
	}

	go func() {
		// 未知类型字节必须凑满帧头，否则读取方会阻塞在帧头读取上。
		header := make([]byte, V1HeaderSize)
		header[0] = 0x7f
		header[V1HeaderSize-1] = 0
		_, _ = client.Write(header)
	}()

	_, err := guard.ReadFrame()
	if err == nil {
		t.Fatal("未知类型必须返回错误")
	}
	if !IsCategory(err, CategoryTypeInvalid) {
		t.Fatalf("错误类别不一致：实际 %s", CategoryOf(err))
	}
	if len(events) != 1 {
		t.Fatalf("必须发布一条关闭事件，实际 %d", len(events))
	}
	if events[0].Stage != StageMessage {
		t.Fatalf("事件的 wire 阶段标记不一致：%s", events[0].Stage)
	}
	if events[0].Succeeded {
		t.Fatal("拒绝事件不得标记为成功")
	}
	if events[0].PeerDigest == "" {
		t.Fatal("事件必须携带脱敏对端地址摘要")
	}
}

// TestV1EOFIsNotProtocolError 断言帧边界 EOF 不产生关闭事件。
func TestV1EOFIsNotProtocolError(t *testing.T) {
	guard := NewConnectionGuard(&recordingConn{Conn: &bufferConn{buffer: &bytes.Buffer{}}},
		mustPool(t), Options{MaxWireVersion: VersionV1, V2Enabled: true})
	reader := NewV1Reader(bytes.NewReader(nil), DefaultV1PayloadLimit)
	reader.SetPool(mustPool(t))
	guard.bindReader(reader)
	if err := guard.LockVersion(VersionV1); err != nil {
		t.Fatalf("锁定版本失败：%v", err)
	}

	if _, err := guard.ReadFrame(); !errors.Is(err, io.EOF) {
		t.Fatalf("帧边界应返回 io.EOF，实际 %v", err)
	}
	if guard.Closed() {
		t.Fatal("干净的流结束不应触发拒绝关闭")
	}
}

// TestCloseEventDoesNotLeakPayloadOrInternals 断言事件字段不含载荷与内部信息。
func TestCloseEventDoesNotLeakPayloadOrInternals(t *testing.T) {
	secret := "super-secret-key-material-0001"
	server, client := net.Pipe()
	defer client.Close()

	var events []CloseEvent
	guard := NewConnectionGuard(server, mustPool(t), Options{
		MaxWireVersion: VersionV1,
		V2Enabled:      true,
		EventSink:      func(event CloseEvent) { events = append(events, event) },
	})
	reader := NewV1Reader(server, DefaultV1PayloadLimit)
	guard.bindReader(reader)
	if err := guard.LockVersion(VersionV1); err != nil {
		t.Fatalf("锁定版本失败：%v", err)
	}

	go func() {
		// 畸形 JSON 载荷内含秘密材料，事件不得回显。
		payload := []byte(`{"key":"` + secret + `"`)
		frame, _ := EncodeV1Frame(Frame{Type: MessageTypeLogin, Payload: payload})
		_, _ = client.Write(frame)
	}()

	_, err := guard.ReadFrame()
	if err == nil {
		t.Fatal("畸形 JSON 必须被拒绝")
	}
	for _, event := range events {
		rendered := string(event.Category) + string(event.Stage) + event.PeerDigest
		if bytes.Contains([]byte(rendered), []byte(secret)) {
			t.Fatalf("事件泄露了载荷内容：%s", rendered)
		}
	}
	if bytes.Contains([]byte(err.Error()), []byte(secret)) {
		t.Fatalf("错误文案泄露了载荷内容：%v", err)
	}
}

func mustPool(t *testing.T) *BufferPool {
	t.Helper()
	pool, err := NewBufferPool(DefaultV2PayloadLimit, bufferedPoolSize)
	if err != nil {
		t.Fatalf("建立缓冲池失败：%v", err)
	}
	return pool
}

func mustEncodeV1(t *testing.T) []byte {
	t.Helper()
	encoded, err := EncodeV1Frame(Frame{Type: MessageTypeLogin, Payload: []byte(`{"version":"0.1.0"}`)})
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	return encoded
}
