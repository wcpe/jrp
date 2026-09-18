package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
)

// TestPeekedReaderReplaysAllReadBytes 断言预读字节被完整回放。
func TestPeekedReaderReplaysAllReadBytes(t *testing.T) {
	payload := []byte("abcdefgh")
	peeker := NewPeekReader(bytes.NewReader(payload))

	peeked, err := peeker.Peek(4)
	if err != nil {
		t.Fatalf("预读失败：%v", err)
	}
	if string(peeked) != "abcd" {
		t.Fatalf("预读内容不一致：%q", peeked)
	}

	// 未消费时必须能读到全部原始字节，说明预读被回放而非丢弃。
	rest, err := io.ReadAll(peeker)
	if err != nil {
		t.Fatalf("读取剩余失败：%v", err)
	}
	if string(rest) != string(payload) {
		t.Fatalf("回放不完整：实际 %q，期望 %q", rest, payload)
	}
}

// TestDetectVersionDegradesToV1WhenPrefixIsNotMagic 断言非魔数前导按 v1 解析且不消费字节。
func TestDetectVersionDegradesToV1WhenPrefixIsNotMagic(t *testing.T) {
	loginFrame, err := EncodeV1Frame(Frame{Type: MessageTypeLogin, Payload: []byte(`{"version":"0.1.0"}`)})
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}

	guard := newTestGuard(t, Options{MaxWireVersion: VersionV1, V2Enabled: true})
	version, err := guard.DetectVersion(bytes.NewReader(loginFrame))
	if err != nil {
		t.Fatalf("版本判定失败：%v", err)
	}
	if version != VersionV1 {
		t.Fatalf("版本判定不一致：实际 %s", version)
	}

	// 回放后首帧必须仍是完整登录帧，类型与载荷都能解析。
	frame, err := guard.ReadFrame()
	if err != nil {
		t.Fatalf("回放后解析首帧失败：%v", err)
	}
	if frame.Type.Name != "login" {
		t.Fatalf("消息类型不一致：%s", frame.Type.Name)
	}
	if !bytes.Contains(frame.Payload, []byte(`"version":"0.1.0"`)) {
		t.Fatalf("载荷不一致：%s", frame.Payload)
	}
}

// TestDetectVersionRecognizesV2Magic 断言 v2 魔数被识别且不进入 v1 路径。
func TestDetectVersionRecognizesV2Magic(t *testing.T) {
	clientHello := loadGolden(t).V2[0]
	stream := append(append(append([]byte(nil), V2Magic...), mustHex(t, clientHello.Hex)...), 'x')

	guard := newTestGuard(t, Options{MaxWireVersion: VersionV2, V2Enabled: true})
	version, err := guard.DetectVersion(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("版本判定失败：%v", err)
	}
	if version != VersionV2 {
		t.Fatalf("版本判定不一致：实际 %s", version)
	}
}

// TestDetectVersionReportsEmptyConnection 断言空连接不计为协议错误也不计为成功。
func TestDetectVersionReportsEmptyConnection(t *testing.T) {
	guard := newTestGuard(t, Options{MaxWireVersion: VersionV2, V2Enabled: true})
	_, err := guard.DetectVersion(bytes.NewReader(nil))
	if !errors.Is(err, ErrEmptyConnection) {
		t.Fatalf("空连接应返回 ErrEmptyConnection，实际 %v", err)
	}
	if CategoryOf(err) != "" {
		t.Fatalf("空连接不应计为协议错误，实际类别 %s", CategoryOf(err))
	}
}

// TestDetectVersionRejectsV1WhenV2Required 断言「仅 v2」入口拒绝非魔数前导。
func TestDetectVersionRejectsV1WhenV2Required(t *testing.T) {
	loginFrame, err := EncodeV1Frame(Frame{Type: MessageTypeLogin, Payload: []byte(`{}`)})
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}

	guard := newTestGuard(t, Options{MaxWireVersion: VersionV2, V2Enabled: true})
	_, err = guard.DetectVersion(bytes.NewReader(loginFrame))
	if !IsCategory(err, CategoryVersionNotAccepted) {
		t.Fatalf("仅 v2 入口必须拒绝 v1，实际 %v", err)
	}
	if StageOf(err) != StageDetect {
		t.Fatalf("阶段标记不一致：%s", StageOf(err))
	}
}

// TestDetectVersionRejectsV2WhenDisabled 断言服务端整体关闭 v2 时拒绝 v2 魔数。
func TestDetectVersionRejectsV2WhenDisabled(t *testing.T) {
	guard := newTestGuard(t, Options{MaxWireVersion: VersionV2, V2Enabled: false})
	_, err := guard.DetectVersion(bytes.NewReader(V2Magic))
	if !IsCategory(err, CategoryVersionNotAccepted) {
		t.Fatalf("v2 关闭时必须拒绝，实际 %v", err)
	}
}

// TestClientHelloNegotiationSelectsIntersection 断言协商取交集并锁定上限。
func TestClientHelloNegotiationSelectsIntersection(t *testing.T) {
	request := clientHelloWith(
		[]string{"json"},
		[]string{"none", "jrp-stream-v1"},
		[]string{"none"},
		DefaultV2PayloadLimit,
	)

	result, err := Negotiate(request, DefaultCapabilities())
	if err != nil {
		t.Fatalf("协商失败：%v", err)
	}
	if result.MessageCodec != CodecJSON {
		t.Fatalf("消息编码不一致：%s", result.MessageCodec)
	}
	if result.CryptoAlgorithm != CryptoNone {
		t.Fatalf("加密算法不一致：%s", result.CryptoAlgorithm)
	}
	if result.CompressionAlgorithm != CompressionNone {
		t.Fatalf("压缩算法不一致：%s", result.CompressionAlgorithm)
	}
	if result.MaxPayload != DefaultV2PayloadLimit {
		t.Fatalf("载荷上限不一致：%d", result.MaxPayload)
	}
}

// TestNegotiationPayloadLimitTakesSmallerValue 断言上限取双方较小值。
func TestNegotiationPayloadLimitTakesSmallerValue(t *testing.T) {
	request := clientHelloWith([]string{"json"}, []string{"none"}, nil, DefaultV2PayloadLimit/2)
	result, err := Negotiate(request, DefaultCapabilities())
	if err != nil {
		t.Fatalf("协商失败：%v", err)
	}
	if result.MaxPayload != DefaultV2PayloadLimit/2 {
		t.Fatalf("应取较小上限，实际 %d", result.MaxPayload)
	}
}

// TestNegotiationRejectsEmptyIntersection 断言能力交集为空必须失败而不是降级。
func TestNegotiationRejectsEmptyIntersection(t *testing.T) {
	t.Run("编码交集为空", func(t *testing.T) {
		request := clientHelloWith([]string{"protobuf"}, []string{"none"}, nil, 0)
		_, err := Negotiate(request, DefaultCapabilities())
		if !IsCategory(err, CategoryCapabilityMismatch) {
			t.Fatalf("错误类别不一致：实际 %s", CategoryOf(err))
		}
	})

	t.Run("加密交集为空", func(t *testing.T) {
		request := clientHelloWith([]string{"json"}, []string{"rot13"}, nil, 0)
		_, err := Negotiate(request, DefaultCapabilities())
		if !IsCategory(err, CategoryCapabilityMismatch) {
			t.Fatalf("错误类别不一致：实际 %s", CategoryOf(err))
		}
	})

	t.Run("压缩无交集时退化为不压缩", func(t *testing.T) {
		request := clientHelloWith([]string{"json"}, []string{"none"}, []string{"zstd"}, 0)
		result, err := Negotiate(request, DefaultCapabilities())
		if err != nil {
			t.Fatalf("压缩无交集不应导致失败：%v", err)
		}
		if result.CompressionAlgorithm != CompressionNone {
			t.Fatalf("应退化为不压缩，实际 %s", result.CompressionAlgorithm)
		}
	})
}

// TestNegotiationRejectsHelloWithEmptyCapabilities 断言空 hello 属协商帧非法。
func TestNegotiationRejectsHelloWithEmptyCapabilities(t *testing.T) {
	_, err := Negotiate(ClientHello{}, DefaultCapabilities())
	if !IsCategory(err, CategoryCapabilityMismatch) {
		t.Fatalf("空 hello 必须失败，实际 %v", err)
	}
}

// TestServerHelloRoundTrip 断言 server hello 可往返，且不含密钥材料。
func TestServerHelloRoundTrip(t *testing.T) {
	result := NegotiationResult{
		MessageCodec:         CodecJSON,
		CryptoAlgorithm:      CryptoStreamV1,
		CompressionAlgorithm: CompressionNone,
		MaxPayload:           4096,
	}
	encoded, err := EncodeServerHello(result)
	if err != nil {
		t.Fatalf("编码 server hello 失败：%v", err)
	}
	decoded, err := DecodeServerHello(encoded)
	if err != nil {
		t.Fatalf("解码 server hello 失败：%v", err)
	}
	if decoded.MaxPayload != result.MaxPayload || decoded.CryptoAlgorithm != result.CryptoAlgorithm {
		t.Fatalf("server hello 往返不一致：%+v", decoded)
	}

	// 失败原因不得回显密钥材料或内部字段路径。
	reason := EncodeNegotiationFailure(CategoryCapabilityMismatch)
	if strings.Contains(reason, "key") || strings.Contains(reason, "/") {
		t.Fatalf("失败原因泄露内部信息：%s", reason)
	}
}

// TestHelloFrameTypeMismatchIsRejected 断言协商阶段收到非 hello 帧属协商帧非法。
func TestHelloFrameTypeMismatchIsRejected(t *testing.T) {
	messageFrame, err := EncodeV2Frame(V2FrameTypeMessage, []byte{0, 1, '{', '}'})
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	guard := newTestGuard(t, Options{MaxWireVersion: VersionV2, V2Enabled: true})
	// 类型不匹配的帧必须先被判为已定义帧类型，再在协商阶段被拒。
	if _, err := guard.DetectVersion(bytes.NewReader(append(append([]byte(nil), V2Magic...), messageFrame...))); err != nil {
		t.Fatalf("版本判定失败：%v", err)
	}
	_, err = guard.Negotiate()
	if !IsCategory(err, CategoryNegotiationFrameInvalid) {
		t.Fatalf("期望协商帧非法，实际 %v", err)
	}
	if StageOf(err) != StageNegotiate {
		t.Fatalf("阶段标记不一致：%s", StageOf(err))
	}
}

// TestNegotiateRejectsTruncatedHello 断言 hello 截断按协商帧非法处理。
func TestNegotiateRejectsTruncatedHello(t *testing.T) {
	hello := mustHex(t, loadGolden(t).V2[0].Hex)
	truncated := hello[:len(hello)/2]

	guard := newTestGuard(t, Options{MaxWireVersion: VersionV2, V2Enabled: true})
	if _, err := guard.DetectVersion(bytes.NewReader(append(append([]byte(nil), V2Magic...), truncated...))); err != nil {
		t.Fatalf("版本判定失败：%v", err)
	}
	_, err := guard.Negotiate()
	if !IsCategory(err, CategoryNegotiationFrameInvalid) {
		t.Fatalf("期望协商帧非法，实际 %v", err)
	}
}

// TestNegotiationRejectsMalformedHelloPayload 断言 hello 载荷结构畸形属协商帧非法。
func TestNegotiationRejectsMalformedHelloPayload(t *testing.T) {
	hello, err := EncodeV2Frame(V2FrameTypeClientHello, []byte(`{"capabilities":`))
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	guard := newTestGuard(t, Options{MaxWireVersion: VersionV2, V2Enabled: true})
	if _, err := guard.DetectVersion(bytes.NewReader(append(append([]byte(nil), V2Magic...), hello...))); err != nil {
		t.Fatalf("版本判定失败：%v", err)
	}
	if _, err := guard.Negotiate(); !IsCategory(err, CategoryNegotiationFrameInvalid) {
		t.Fatalf("期望协商帧非法，实际 %v", err)
	}
}

// TestExplicitV2FailureNeverFallsBackToV1 断言显式 v2 协商失败不回落 v1。
func TestExplicitV2FailureNeverFallsBackToV1(t *testing.T) {
	// 构造能力交集为空的 client hello。
	payload := []byte(`{"capabilities":{"message":{"codecs":["protobuf"]},"crypto":{"algorithms":["rot13"]}}}`)
	hello, err := EncodeV2Frame(V2FrameTypeClientHello, payload)
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	guard := newTestGuard(t, Options{MaxWireVersion: VersionV2, V2Enabled: true})
	if _, err := guard.DetectVersion(bytes.NewReader(append(append([]byte(nil), V2Magic...), hello...))); err != nil {
		t.Fatalf("版本判定失败：%v", err)
	}

	_, err = guard.Negotiate()
	if !IsCategory(err, CategoryCapabilityMismatch) {
		t.Fatalf("期望能力无法协商，实际 %v", err)
	}
	if guard.Version() == VersionV1 {
		t.Fatal("协商失败后不得回落 v1")
	}
}

// TestNegotiatedPayloadLimitBoundsMessageFrames 断言协商上限被双向约束到消息帧。
func TestNegotiatedPayloadLimitBoundsMessageFrames(t *testing.T) {
	// 客户端声明的上限低于服务端：协商结果必须收紧到该值。
	request := clientHelloWith([]string{"json"}, []string{"none"}, nil, 4096)
	result, err := Negotiate(request, DefaultCapabilities())
	if err != nil {
		t.Fatalf("协商失败：%v", err)
	}
	if result.MaxPayload != 4096 {
		t.Fatalf("上限未被收紧：%d", result.MaxPayload)
	}

	// 协商上限内的消息帧可编码；超过该上限的消息帧必须被拒绝。
	body := padPayload(result.MaxPayload - V2MessageTypeIDSize)
	payload := make([]byte, V2MessageTypeIDSize+len(body))
	binary.BigEndian.PutUint16(payload[0:V2MessageTypeIDSize], MessageTypeLogin.V2ID)
	copy(payload[V2MessageTypeIDSize:], body)

	if _, err := EncodeV2FrameWithLimit(V2FrameTypeMessage, payload, result.MaxPayload); err != nil {
		t.Fatalf("协商上限内的消息帧编码失败：%v", err)
	}
	overLimit := append(append([]byte(nil), payload...), 'x')
	if _, err := EncodeV2FrameWithLimit(V2FrameTypeMessage, overLimit, result.MaxPayload); !IsCategory(err, CategoryLengthExceeded) {
		t.Fatalf("超过协商上限必须被拒绝，实际 %v", err)
	}
}

// TestExplicitV2NegotiationFailurePublishesNegotiationStageEvent 断言显式 v2 协商失败的事件阶段。
func TestExplicitV2NegotiationFailurePublishesNegotiationStageEvent(t *testing.T) {
	// 能力交集为空的 client hello。
	hello := mustHello(t, clientHelloWith([]string{"protobuf"}, []string{"rot13"}, nil, 0))

	var events []CloseEvent
	pool, err := NewBufferPool(DefaultV2PayloadLimit, bufferedPoolSize)
	if err != nil {
		t.Fatalf("建立缓冲池失败：%v", err)
	}
	guard := NewConnectionGuard(nil, pool, Options{
		MaxWireVersion: VersionV2,
		V2Enabled:      true,
		EventSink:      func(event CloseEvent) { events = append(events, event) },
	})
	server, client := net.Pipe()
	defer client.Close()
	guard.conn = server

	if _, err := guard.DetectVersion(bytes.NewReader(append(append([]byte(nil), V2Magic...), hello...))); err != nil {
		t.Fatalf("版本判定失败：%v", err)
	}

	_, err = guard.Negotiate()
	if !IsCategory(err, CategoryCapabilityMismatch) {
		t.Fatalf("期望能力无法协商，实际 %v", err)
	}
	if guard.Version() == VersionV1 {
		t.Fatal("协商失败后不得回落 v1")
	}
	if guard.Closed() != true {
		t.Fatal("协商失败必须关闭连接")
	}
	if len(events) != 1 {
		t.Fatalf("必须发布一条结构化关闭事件，实际 %d", len(events))
	}
	if events[0].Stage != StageNegotiate {
		t.Fatalf("事件的 wire 阶段标记不是协商阶段：%s", events[0].Stage)
	}
	if events[0].Category != CategoryCapabilityMismatch {
		t.Fatalf("事件错误类别不一致：%s", events[0].Category)
	}
	if events[0].Succeeded {
		t.Fatal("协商失败事件不得标记为成功")
	}
}

// TestV2MessageFramesReadAfterNegotiation 断言协商成功后同一流可继续读取消息帧。
func TestV2MessageFramesReadAfterNegotiation(t *testing.T) {
	pool, err := NewBufferPool(DefaultV2PayloadLimit, bufferedPoolSize)
	if err != nil {
		t.Fatalf("建立缓冲池失败：%v", err)
	}

	// 构造协商后的完整流：魔数 + client hello + 一条登录消息帧。
	helloPayload, err := EncodeClientHello(clientHelloWith([]string{"json"}, []string{"none"}, nil, DefaultV2PayloadLimit))
	if err != nil {
		t.Fatalf("编码 client hello 失败：%v", err)
	}
	helloFrame, err := EncodeV2Frame(V2FrameTypeClientHello, helloPayload)
	if err != nil {
		t.Fatalf("编码 hello 帧失败：%v", err)
	}
	loginPayload := []byte(`{"version":"0.1.0","timestamp":1700000000}`)
	loginFrame, err := EncodeV2MessageFrame(MessageTypeLogin, loginPayload)
	if err != nil {
		t.Fatalf("编码登录消息帧失败：%v", err)
	}

	stream := append(append(append([]byte(nil), V2Magic...), helloFrame...), loginFrame...)
	guard := NewConnectionGuard(nil, pool, Options{MaxWireVersion: VersionV2, V2Enabled: true})

	version, err := guard.DetectVersion(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("版本判定失败：%v", err)
	}
	if version != VersionV2 {
		t.Fatalf("版本判定不一致：%s", version)
	}

	if _, err := guard.Negotiate(); err != nil {
		t.Fatalf("协商失败：%v", err)
	}

	// 协商后的消息帧必须从同一读取器读出，不能因重建读取器而丢失流位置。
	frame, err := guard.ReadFrame()
	if err != nil {
		t.Fatalf("读取协商后的消息帧失败：%v", err)
	}
	defer frame.Release()
	if frame.Type.Name != "login" {
		t.Fatalf("消息类型不一致：%s", frame.Type.Name)
	}
	if string(frame.Payload) != string(loginPayload) {
		t.Fatalf("消息载荷不一致：%s", frame.Payload)
	}
}

// TestV2ReaderRejectsNonMessageFrameInMessagePhase 断言消息阶段收到 hello 帧被拒绝。
func TestV2ReaderRejectsNonMessageFrameInMessagePhase(t *testing.T) {
	pool, err := NewBufferPool(DefaultV2PayloadLimit, bufferedPoolSize)
	if err != nil {
		t.Fatalf("建立缓冲池失败：%v", err)
	}
	serverHello, err := EncodeV2Frame(V2FrameTypeServerHello, []byte(`{}`))
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	guard := NewConnectionGuard(nil, pool, Options{MaxWireVersion: VersionV2, V2Enabled: true})
	reader := NewV2Reader(bytes.NewReader(serverHello), DefaultV2PayloadLimit)
	reader.SetPool(pool)
	guard.v2 = reader
	if err := guard.LockVersion(VersionV2); err != nil {
		t.Fatalf("锁定版本失败：%v", err)
	}

	_, err = guard.ReadFrame()
	if !IsCategory(err, CategoryFrameTypeInvalid) {
		t.Fatalf("期望帧类型非法，实际 %v", err)
	}
	if !guard.Closed() {
		t.Fatal("消息阶段的帧类型错误必须关闭连接")
	}
	if outstanding := pool.Outstanding(); outstanding != 0 {
		t.Fatalf("拒绝后仍有 %d 块缓冲未归还", outstanding)
	}
}

// mustHello 编码一段 client hello 帧载荷（不含帧头）。
func mustHello(t *testing.T, hello ClientHello) []byte {
	t.Helper()
	payload, err := EncodeClientHello(hello)
	if err != nil {
		t.Fatalf("编码 client hello 失败：%v", err)
	}
	frame, err := EncodeV2Frame(V2FrameTypeClientHello, payload)
	if err != nil {
		t.Fatalf("编码帧失败：%v", err)
	}
	return frame
}

// TestHelloSchemaUsesNestedCapabilityGroups 断言 hello 能力按线上嵌套结构分组。
//
// 结构按固定基线官方 frpc 的黑盒线上形状建立：能力位于 capabilities 之下，
// 协商结果位于 selected 之下。扁平化会改变线上契约，因此必须锁定。
func TestHelloSchemaUsesNestedCapabilityGroups(t *testing.T) {
	encoded, err := EncodeClientHello(clientHelloWith([]string{"json"}, []string{"none"}, nil, 4096))
	if err != nil {
		t.Fatalf("编码 client hello 失败：%v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	capabilities, ok := decoded["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("client hello 缺少 capabilities 分组：%s", encoded)
	}
	message, ok := capabilities["message"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities 缺少 message 分组：%s", encoded)
	}
	if _, ok := message["codecs"].([]any); !ok {
		t.Fatalf("message 缺少 codecs 字段：%s", encoded)
	}

	serverEncoded, err := EncodeServerHello(NegotiationResult{
		MessageCodec:         CodecJSON,
		CryptoAlgorithm:      CryptoNone,
		CompressionAlgorithm: CompressionNone,
		MaxPayload:           4096,
	})
	if err != nil {
		t.Fatalf("编码 server hello 失败：%v", err)
	}
	var serverDecoded map[string]any
	if err := json.Unmarshal(serverEncoded, &serverDecoded); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if _, ok := serverDecoded["selected"].(map[string]any); !ok {
		t.Fatalf("server hello 缺少 selected 分组：%s", serverEncoded)
	}
}

// TestDecodeRealShapedHello 断言能读懂按线上结构构造的 hello 载荷。
func TestDecodeRealShapedHello(t *testing.T) {
	// 结构与黑盒观测到的线上形状一致，取值由 JRP 合成。
	payload := []byte(`{"bootstrap":{"transport":"tcp"},"capabilities":{"message":{"codecs":["json"]},"crypto":{"algorithms":["none","jrp-stream-v1"],"clientRandom":"AAAA"},"maxPayload":8192}}`)
	request, err := DecodeClientHello(payload)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if request.Bootstrap.Transport != "tcp" {
		t.Fatalf("传输摘要不一致：%s", request.Bootstrap.Transport)
	}
	if len(request.Capabilities.Message.Codecs) != 1 || request.Capabilities.Message.Codecs[0] != "json" {
		t.Fatalf("编码集合不一致：%v", request.Capabilities.Message.Codecs)
	}
	if len(request.Capabilities.Crypto.Algorithms) != 2 {
		t.Fatalf("加密集合不一致：%v", request.Capabilities.Crypto.Algorithms)
	}

	result, err := Negotiate(request, DefaultCapabilities())
	if err != nil {
		t.Fatalf("协商失败：%v", err)
	}
	if result.MaxPayload != 8192 {
		t.Fatalf("上限未被收紧：%d", result.MaxPayload)
	}
	// 协商材料不得进入协商结果：结果只承载选定算法与上限。
	rendered := string(mustJSON(t, result))
	if strings.Contains(rendered, "clientRandom") || strings.Contains(rendered, "AAAA") {
		t.Fatalf("协商结果泄露了对端随机材料：%s", rendered)
	}
}

// mustJSON 把值编码为 JSON，供断言使用。
func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	return encoded
}

// newTestGuard 建立测试用的连接守卫。
func newTestGuard(t *testing.T, options Options) *ConnectionGuard {
	t.Helper()
	pool, err := NewBufferPool(DefaultV2PayloadLimit, bufferedPoolSize)
	if err != nil {
		t.Fatalf("建立缓冲池失败：%v", err)
	}
	return NewConnectionGuard(&recordingConn{Conn: &bufferConn{buffer: &bytes.Buffer{}}}, pool, options)
}

// TestNegotiatedLimitAppliesToConnectionReads 断言协商上限被回灌到连接读取路径。
//
// 这是验收发现的缺口：协商结果此前只停留在返回值上，连接读取器仍按实现上限
// 解析，导致对端声明的较小上限形同虚设。
func TestNegotiatedLimitAppliesToConnectionReads(t *testing.T) {
	// 客户端声明 4096 上限，低于服务端实现上限。
	// mustHello 返回的是完整帧（含帧头），不再二次封装。
	helloFrame := mustHello(t, clientHelloWith([]string{"json"}, []string{"none"}, nil, 4096))

	// 构造一条载荷超过协商上限（4096）但低于实现上限（64 KiB）的消息帧。
	oversized := make([]byte, 8192)
	binary.BigEndian.PutUint16(oversized[0:V2MessageTypeIDSize], MessageTypeLogin.V2ID)
	oversizedFrame, err := EncodeV2FrameWithLimit(V2FrameTypeMessage, oversized, DefaultV2PayloadLimit)
	if err != nil {
		t.Fatalf("编码超限帧失败：%v", err)
	}

	// 同一条流：魔数 + hello + 超限消息帧。
	stream := append(append(append([]byte(nil), V2Magic...), helloFrame...), oversizedFrame...)
	pool, err := NewBufferPool(DefaultV2PayloadLimit, bufferedPoolSize)
	if err != nil {
		t.Fatalf("建立缓冲池失败：%v", err)
	}
	guard := NewConnectionGuard(nil, pool, Options{MaxWireVersion: VersionV2, V2Enabled: true})

	if _, err := guard.DetectVersion(bytes.NewReader(stream)); err != nil {
		t.Fatalf("版本判定失败：%v", err)
	}
	result, err := guard.Negotiate()
	if err != nil {
		t.Fatalf("协商失败：%v", err)
	}
	if result.MaxPayload != 4096 {
		t.Fatalf("协商上限应为 4096，实际 %d", result.MaxPayload)
	}

	// 消息读取必须受协商上限约束：超限帧被拒绝而非按实现上限放行。
	if _, err := guard.ReadFrame(); !IsCategory(err, CategoryLengthExceeded) {
		t.Fatalf("超过协商上限的消息帧必须被拒绝，实际：%v", err)
	}
}
