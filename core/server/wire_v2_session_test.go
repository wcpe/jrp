package server

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/wcpe/jrp/core/internal/wire"
)

// v2TestSession 是以 wire v2 建立的控制会话，模拟官方 frpc 的 v2 路径。
//
// v2 的入站读取是流式的：hello 与后续消息帧共用同一个读取器，
// 不能像 v1 那样每帧重建。
type v2TestSession struct {
	t      *testing.T
	raw    net.Conn
	reader *wire.V2Reader
}

// openV2TestSession 完成 v2 握手（magic + hello 往返）并登录。
func openV2TestSession(t *testing.T, listener net.Listener, clientID, token string) *v2TestSession {
	t.Helper()
	raw, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("连接失败：%v", err)
	}
	session := &v2TestSession{t: t, raw: raw, reader: wire.NewV2Reader(raw, wire.DefaultV2PayloadLimit)}
	session.writeHello([]string{wire.CodecJSON})
	session.readServerHello()
	session.writeMessage(wire.MessageTypeLogin, map[string]string{"clientID": clientID, "token": token})
	var response loginResponsePayload
	session.readMessage(wire.MessageTypeLoginResponse, &response)
	if !response.OK {
		t.Fatalf("v2 登录被拒：%s", response.Error)
	}
	return session
}

// writeHello 发送 magic 与 client hello（FR-04 §协商）。
func (s *v2TestSession) writeHello(codecs []string) {
	s.t.Helper()
	hello := wire.ClientHello{
		Bootstrap: wire.BootstrapSummary{Transport: "tcp"},
		Capabilities: wire.ClientCaps{
			Message:     wire.MessageCodecs{Codecs: codecs},
			Crypto:      wire.CryptoOffer{Algorithms: []string{wire.CryptoNone}},
			Compression: &wire.CompressionOffer{Algorithms: []string{wire.CompressionNone}},
			MaxPayload:  wire.DefaultV2PayloadLimit,
		},
	}
	payload, err := wire.EncodeClientHello(hello)
	if err != nil {
		s.t.Fatalf("编码 client hello 失败：%v", err)
	}
	frame, err := wire.EncodeV2Frame(wire.V2FrameTypeClientHello, payload)
	if err != nil {
		s.t.Fatalf("编码 hello 帧失败：%v", err)
	}
	if _, err := s.raw.Write(append(append([]byte{}, wire.V2Magic...), frame...)); err != nil {
		s.t.Fatalf("发送 hello 失败：%v", err)
	}
}

// readServerHello 读取并校验服务端 hello（协商结果）。
func (s *v2TestSession) readServerHello() {
	s.t.Helper()
	frame := s.readFrame(3 * time.Second)
	defer frame.Release()
	if frame.FrameType != wire.V2FrameTypeServerHello {
		s.t.Fatalf("期望 server hello，实际帧类型 %d", frame.FrameType)
	}
	var hello wire.ServerHello
	if err := json.Unmarshal(frame.Message.Payload, &hello); err != nil {
		s.t.Fatalf("解析 server hello 失败：%v", err)
	}
	if hello.Selected.Message.Codec != wire.CodecJSON {
		s.t.Fatalf("协商编码意外：%s", hello.Selected.Message.Codec)
	}
	if hello.Selected.Crypto.Algorithm == "" {
		s.t.Fatalf("协商结果不完整：%+v", hello.Selected)
	}
}

// writeMessage 发送一条 v2 消息帧。
func (s *v2TestSession) writeMessage(messageType wire.MessageType, payload any) {
	s.t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		s.t.Fatalf("编码 %s 载荷失败：%v", messageType.Name, err)
	}
	frame, err := wire.EncodeV2MessageFrame(messageType, body)
	if err != nil {
		s.t.Fatalf("编码 %s 帧失败：%v", messageType.Name, err)
	}
	if _, err := s.raw.Write(frame); err != nil {
		s.t.Fatalf("发送 %s 失败：%v", messageType.Name, err)
	}
}

// readMessage 读取一条消息帧并解入 target。
func (s *v2TestSession) readMessage(expected wire.MessageType, target any) {
	s.t.Helper()
	frame := s.readFrame(5 * time.Second)
	defer frame.Release()
	if frame.FrameType != wire.V2FrameTypeMessage {
		s.t.Fatalf("期望消息帧，实际帧类型 %d", frame.FrameType)
	}
	if frame.Message.Type.Name != expected.Name {
		s.t.Fatalf("期望 %s，实际 %s", expected.Name, frame.Message.Type.Name)
	}
	if err := json.Unmarshal(frame.Message.Payload, target); err != nil {
		s.t.Fatalf("解析 %s 响应失败：%v", expected.Name, err)
	}
}

// readFrame 在给定超时内读取一条 v2 帧。
func (s *v2TestSession) readFrame(timeout time.Duration) wire.V2Frame {
	s.t.Helper()
	_ = s.raw.SetReadDeadline(time.Now().Add(timeout))
	defer func() { _ = s.raw.SetReadDeadline(time.Time{}) }()
	frame, err := s.reader.ReadFrame()
	if err != nil {
		s.t.Fatalf("读取 v2 帧失败：%v", err)
	}
	return frame
}

// v2 会话完整生命周期：协商 → 登录 → 心跳往返 → 代理注册（FR-03 §3.4-§3.6）。
//
// 服务端出站编码必须按会话版本分流：v2 会话收到 v1 形状的响应会解析失败，
// 因此本用例同时覆盖协商结果、心跳响应与注册响应三类出站帧。
func TestV2SessionLoginHeartbeatAndProxyRegistration(t *testing.T) {
	engine, listener := startEngineForRuntimeProxy(t)
	target, stopEcho := startTestEcho(t)
	defer stopEcho()

	session := openV2TestSession(t, listener, "rt", "rt-token")
	defer session.raw.Close()

	session.writeMessage(wire.MessageTypePing, map[string]any{})
	var pong map[string]any
	session.readMessage(wire.MessageTypePong, &pong)

	remotePort := reserveTestPortForRuntime(t)
	session.writeMessage(wire.MessageTypeNewProxy, map[string]any{
		"proxyName":  "v2-proxy",
		"proxyType":  "tcp",
		"remotePort": remotePort,
		"target":     target.String(),
	})
	var response proxyOperationResponse
	session.readMessage(wire.MessageTypeNewProxyResponse, &response)
	if !response.OK {
		t.Fatalf("v2 代理注册被拒：%s", response.Error)
	}
	if engine.GuestAddr("v2-proxy") == nil {
		t.Fatal("v2 会话注册的代理未创建入口")
	}
}

// v2 协商失败只影响本会话：引擎继续服务，后续 v1 会话不受影响（FR-03 §3.4）。
func TestV2HelloFailureKeepsEngineServing(t *testing.T) {
	engine, listener := startEngineForRuntimeProxy(t)

	raw, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("连接失败：%v", err)
	}
	defer raw.Close()
	failing := &v2TestSession{t: t, raw: raw, reader: wire.NewV2Reader(raw, wire.DefaultV2PayloadLimit)}
	// 客户端只声明服务端不支持的编码：协商必须失败而非降级到默认编码。
	failing.writeHello([]string{"cbor"})

	_ = raw.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, readErr := raw.Read(make([]byte, 1)); readErr == nil {
		t.Fatal("协商失败的连接应被服务端关闭")
	} else if isReadDeadline(readErr) {
		t.Fatal("协商失败的连接应被关闭（读超时说明连接仍开着）")
	}

	if err := engine.Err(); err != nil {
		t.Fatalf("会话级协商失败不应停止引擎：%v", err)
	}
	// 引擎仍在服务：v1 会话照常登录。
	fallback := openTestSession(t, listener, "rt", "rt-token")
	defer fallback.raw.Close()
	port := reserveTestPortForRuntime(t)
	target, stopEcho := startTestEcho(t)
	defer stopEcho()
	if err := fallback.registerProxy("fallback-proxy", port, target); err != nil {
		t.Fatalf("协商失败后 v1 会话注册失败：%v", err)
	}
}
