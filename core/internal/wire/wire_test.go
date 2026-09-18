package wire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestV1PayloadLimitBoundaries 覆盖上限减一、等于上限、上限加一三组值。
func TestV1PayloadLimitBoundaries(t *testing.T) {
	limit := loadGolden(t).Boundary.V1Limit
	cases := []struct {
		name     string
		size     int
		wantErr  bool
		category ErrorCategory
	}{
		{name: "上限减一", size: limit - 1},
		{name: "等于上限", size: limit},
		{name: "等于上限加一", size: limit + 1, wantErr: true, category: CategoryLengthExceeded},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			payload := padPayload(testCase.size)
			frame := Frame{Type: MessageTypePing, Payload: payload}

			// 编码侧：超限必须拒绝，不得产出帧。
			encoded, err := EncodeV1FrameWithLimit(frame, limit)
			if testCase.wantErr {
				if err == nil {
					t.Fatal("超限载荷编码必须失败")
				}
				if !IsCategory(err, testCase.category) {
					t.Fatalf("错误类别不一致：实际 %s，期望 %s", CategoryOf(err), testCase.category)
				}
				return
			}
			if err != nil {
				t.Fatalf("合法载荷编码失败：%v", err)
			}

			// 解码侧：恰好等于上限应接受，加一应拒绝且不分配。
			decoded, err := DecodeV1Frame(encoded, limit)
			if err != nil {
				t.Fatalf("合法载荷解码失败：%v", err)
			}
			if len(decoded.Payload) != testCase.size {
				t.Fatalf("载荷长度不一致：实际 %d，期望 %d", len(decoded.Payload), testCase.size)
			}

			// 把长度字段声明为超过上限的值：必须在读取前判超限，而不是等待或分配。
			overLimit := append([]byte(nil), encoded...)
			binary.BigEndian.PutUint64(overLimit[1:9], uint64(limit+1))
			_, err = DecodeV1Frame(overLimit, limit)
			if err == nil {
				t.Fatal("声明长度超过上限必须失败")
			}
			if !IsCategory(err, CategoryLengthExceeded) {
				t.Fatalf("错误类别不一致：实际 %s，期望 %s", CategoryOf(err), CategoryLengthExceeded)
			}
		})
	}
}

// TestV1NegativeLengthIsRejectedWithoutAllocation 断言负长度等不可能取值被拒绝。
func TestV1NegativeLengthIsRejectedWithoutAllocation(t *testing.T) {
	cases := []struct {
		name   string
		length int64
	}{
		{name: "负一", length: -1},
		{name: "最小值", length: -1 << 62},
		{name: "负的任意值", length: -12345},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			raw := make([]byte, 9)
			raw[0] = MessageTypePing.V1Byte
			binary.BigEndian.PutUint64(raw[1:9], uint64(testCase.length))

			_, err := DecodeV1Frame(raw, DefaultV1PayloadLimit)
			if err == nil {
				t.Fatal("负长度必须被拒绝")
			}
			if !IsCategory(err, CategoryLengthInvalid) {
				t.Fatalf("错误类别不一致：实际 %s，期望 %s", CategoryOf(err), CategoryLengthInvalid)
			}
		})
	}
}

// TestV1TruncatedInputIsRejected 覆盖载荷逐字节截断的每个前缀。
func TestV1TruncatedInputIsRejected(t *testing.T) {
	encoded, err := EncodeV1Frame(Frame{Type: MessageTypeLogin, Payload: []byte(`{"version":"0.1.0"}`)})
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}

	for length := 0; length < len(encoded); length++ {
		prefix := encoded[:length]
		_, err := DecodeV1Frame(prefix, DefaultV1PayloadLimit)
		if err == nil {
			t.Fatalf("截断到 %d 字节必须报错而不是悬挂", length)
		}
		var protocolErr *ProtocolError
		if !errors.As(err, &protocolErr) {
			t.Fatalf("截断到 %d 字节的错误必须是协议错误：%v", length, err)
		}
	}
}

// TestV1UnknownMessageTypeIsRejected 断言未知类型字节返回不支持类别。
func TestV1UnknownMessageTypeIsRejected(t *testing.T) {
	// 0x7f 不在已登记类型表中。
	raw := make([]byte, 9+2)
	raw[0] = 0x7f
	binary.BigEndian.PutUint64(raw[1:9], 2)
	copy(raw[9:], "{}")

	_, err := DecodeV1Frame(raw, DefaultV1PayloadLimit)
	if err == nil {
		t.Fatal("未知类型必须被拒绝")
	}
	if !IsCategory(err, CategoryTypeInvalid) {
		t.Fatalf("错误类别不一致：实际 %s，期望 %s", CategoryOf(err), CategoryTypeInvalid)
	}
}

// TestV1MalformedJSONIsRejected 断言畸形 JSON 被拒绝且不补猜。
func TestV1MalformedJSONIsRejected(t *testing.T) {
	cases := []string{`{`, `{"a":}`, `not-json`, `{"a":1,}`, `[1,2`}
	for _, payload := range cases {
		t.Run(payload, func(t *testing.T) {
			encoded, err := EncodeV1Frame(Frame{Type: MessageTypeLogin, Payload: []byte(payload)})
			if err != nil {
				t.Fatalf("编码失败：%v", err)
			}
			_, err = DecodeV1Frame(encoded, DefaultV1PayloadLimit)
			if err == nil {
				t.Fatal("畸形 JSON 必须被拒绝")
			}
			if !IsCategory(err, CategoryJSONInvalid) {
				t.Fatalf("错误类别不一致：实际 %s，期望 %s", CategoryOf(err), CategoryJSONInvalid)
			}
		})
	}
}

// TestV1UnknownJSONFieldsAreIgnored 断言未知字段被忽略且不改写载荷。
func TestV1UnknownJSONFieldsAreIgnored(t *testing.T) {
	payload := `{"version":"0.1.0","jrp_unknown_field":{"nested":[1,2,3]},"timestamp":1700000000}`
	encoded, err := EncodeV1Frame(Frame{Type: MessageTypeLogin, Payload: []byte(payload)})
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	decoded, err := DecodeV1Frame(encoded, DefaultV1PayloadLimit)
	if err != nil {
		t.Fatalf("未知字段不应导致失败：%v", err)
	}
	if string(decoded.Payload) != payload {
		t.Fatalf("载荷被改写：实际 %s", decoded.Payload)
	}
}

// TestV1ExtraBytesBelongToNextFrame 断言多余部分按边界切开，视为下一帧。
func TestV1ExtraBytesBelongToNextFrame(t *testing.T) {
	first, err := EncodeV1Frame(Frame{Type: MessageTypePing, Payload: []byte(`{"timestamp":1}`)})
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	second, err := EncodeV1Frame(Frame{Type: MessageTypePong, Payload: []byte(`{"timestamp":2}`)})
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	stream := append(append([]byte(nil), first...), second...)

	reader := NewV1Reader(bytes.NewReader(stream), DefaultV1PayloadLimit)
	decodedFirst, err := reader.ReadFrame()
	if err != nil {
		t.Fatalf("读取首帧失败：%v", err)
	}
	if decodedFirst.Type.Name != "ping" {
		t.Fatalf("首帧类型不一致：%s", decodedFirst.Type.Name)
	}
	decodedSecond, err := reader.ReadFrame()
	if err != nil {
		t.Fatalf("读取次帧失败：%v", err)
	}
	if decodedSecond.Type.Name != "pong" {
		t.Fatalf("次帧类型不一致：%s", decodedSecond.Type.Name)
	}
	if _, err := reader.ReadFrame(); !errors.Is(err, io.EOF) {
		t.Fatalf("流结束后应返回 EOF，实际 %v", err)
	}
}

// TestV1ReaderTreatsPartialHeaderAsProtocolError 断言帧中截断按协议错误处理。
// 只有「在帧边界处读到 EOF」才是干净结束；类型或长度字段缺一字节都属于协议错误。
func TestV1ReaderTreatsPartialHeaderAsProtocolError(t *testing.T) {
	t.Run("帧边界处读到 EOF 是干净结束", func(t *testing.T) {
		reader := NewV1Reader(bytes.NewReader(nil), DefaultV1PayloadLimit)
		if _, err := reader.ReadFrame(); !errors.Is(err, io.EOF) {
			t.Fatalf("帧边界 EOF 应返回 io.EOF，实际 %v", err)
		}
	})

	cases := []struct {
		name string
		raw  []byte
	}{
		{name: "仅类型字节", raw: []byte{MessageTypePing.V1Byte}},
		{name: "长度字段缺一字节", raw: append([]byte{MessageTypePing.V1Byte}, make([]byte, 7)...)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			reader := NewV1Reader(bytes.NewReader(testCase.raw), DefaultV1PayloadLimit)
			_, err := reader.ReadFrame()
			if !IsCategory(err, CategoryPayloadTruncated) {
				t.Fatalf("缺字段必须按协议错误处理，实际 %v", err)
			}
		})
	}
}

// TestV2PayloadLimitBoundaries 覆盖 v2 上限减一、等于上限、上限加一。
func TestV2PayloadLimitBoundaries(t *testing.T) {
	limit := loadGolden(t).Boundary.V2Limit
	cases := []struct {
		name     string
		size     int
		category ErrorCategory
	}{
		{name: "上限减一", size: limit - 1},
		{name: "等于上限", size: limit},
		{name: "等于上限加一", size: limit + 1, category: CategoryLengthExceeded},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// 消息帧载荷 = 2 字节类型 ID + JSON 正文，正文长度由边界值确定性构造。
			body := padPayload(testCase.size - V2MessageTypeIDSize)
			payload := make([]byte, V2MessageTypeIDSize+len(body))
			binary.BigEndian.PutUint16(payload[0:V2MessageTypeIDSize], MessageTypeLogin.V2ID)
			copy(payload[V2MessageTypeIDSize:], body)

			// 直接构造帧字节，避免编码侧的限额校验掩盖解码侧的边界判定。
			raw := make([]byte, V2HeaderSize+len(payload))
			binary.BigEndian.PutUint16(raw[0:2], V2FrameTypeMessage)
			binary.BigEndian.PutUint32(raw[4:V2HeaderSize], uint32(len(payload)))
			copy(raw[V2HeaderSize:], payload)

			frame, err := DecodeV2Frame(raw, limit)
			if testCase.category != "" {
				if err == nil {
					t.Fatal("超限载荷必须被拒绝")
				}
				if !IsCategory(err, testCase.category) {
					t.Fatalf("错误类别不一致：实际 %s，期望 %s", CategoryOf(err), testCase.category)
				}
				return
			}
			if err != nil {
				t.Fatalf("合法载荷解码失败：%v", err)
			}
			if len(frame.Message.Payload) != len(body) {
				t.Fatalf("载荷长度不一致：实际 %d，期望 %d", len(frame.Message.Payload), len(body))
			}
			// 编码侧对同一载荷必须给出与上限一致的判定。
			if _, err := EncodeV2Frame(V2FrameTypeMessage, payload); err != nil {
				t.Fatalf("合法载荷编码失败：%v", err)
			}
		})
	}
}

// TestV2HeaderFieldBoundaries 断言 v2 三个头部字段各自独立完成校验。
func TestV2HeaderFieldBoundaries(t *testing.T) {
	t.Run("非零 flags 被拒绝", func(t *testing.T) {
		raw := make([]byte, 8)
		binary.BigEndian.PutUint16(raw[0:2], V2FrameTypeMessage)
		binary.BigEndian.PutUint16(raw[2:4], 1)
		_, err := DecodeV2Frame(raw, DefaultV2PayloadLimit)
		if !IsCategory(err, CategoryFlagsUnsupported) {
			t.Fatalf("错误类别不一致：实际 %s", CategoryOf(err))
		}
	})

	t.Run("未知帧类型被拒绝", func(t *testing.T) {
		raw := make([]byte, 8)
		binary.BigEndian.PutUint16(raw[0:2], 0xffff)
		_, err := DecodeV2Frame(raw, DefaultV2PayloadLimit)
		if !IsCategory(err, CategoryFrameTypeInvalid) {
			t.Fatalf("错误类别不一致：实际 %s", CategoryOf(err))
		}
	})

	t.Run("截断帧头被拒绝", func(t *testing.T) {
		for length := 0; length < 8; length++ {
			_, err := DecodeV2Frame(make([]byte, length), DefaultV2PayloadLimit)
			if err == nil {
				t.Fatalf("帧头截断到 %d 字节必须报错", length)
			}
		}
	})

	t.Run("截断载荷被拒绝", func(t *testing.T) {
		body := padPayload(64)
		raw, err := EncodeV2Frame(V2FrameTypeMessage, body)
		if err != nil {
			t.Fatalf("编码失败：%v", err)
		}
		for length := 8; length < len(raw); length++ {
			if _, err := DecodeV2Frame(raw[:length], DefaultV2PayloadLimit); err == nil {
				t.Fatalf("载荷截断到 %d 字节必须报错", length)
			}
		}
	})

	t.Run("消息帧载荷不足类型 ID 宽度", func(t *testing.T) {
		for _, size := range []int{0, 1} {
			raw, err := EncodeV2Frame(V2FrameTypeMessage, make([]byte, size))
			if err != nil {
				t.Fatalf("编码失败：%v", err)
			}
			_, err = DecodeV2Frame(raw, DefaultV2PayloadLimit)
			if !IsCategory(err, CategoryTypeInvalid) {
				t.Fatalf("载荷 %d 字节的错误类别不一致：实际 %s", size, CategoryOf(err))
			}
		}
	})
}

// TestBufferPoolRejectsOversizedRequest 断言有界池拒绝超限请求且不做分配。
func TestBufferPoolRejectsOversizedRequest(t *testing.T) {
	pool, err := NewBufferPool(DefaultV1PayloadLimit, bufferedPoolSize)
	if err != nil {
		t.Fatalf("建立缓冲池失败：%v", err)
	}
	if _, err := pool.Get(DefaultV1PayloadLimit + 1); !errors.Is(err, ErrBufferTooLarge) {
		t.Fatalf("超限请求必须失败，实际 %v", err)
	}
	if _, err := pool.Get(-1); !errors.Is(err, ErrBufferTooLarge) {
		t.Fatalf("负长度请求必须失败，实际 %v", err)
	}
	buf, err := pool.Get(64)
	if err != nil {
		t.Fatalf("合法请求失败：%v", err)
	}
	if len(buf) != 64 {
		t.Fatalf("缓冲长度不一致：%d", len(buf))
	}
	pool.Put(buf)
}

// TestBufferPoolReusesBuffers 断言归还后缓冲被复用，热路径不做独立大块分配。
func TestBufferPoolReusesBuffers(t *testing.T) {
	pool, err := NewBufferPool(DefaultV1PayloadLimit, bufferedPoolSize)
	if err != nil {
		t.Fatalf("建立缓冲池失败：%v", err)
	}
	first, err := pool.Get(256)
	if err != nil {
		t.Fatalf("取缓冲失败：%v", err)
	}
	first[0] = 0x5a
	pool.Put(first)

	second, err := pool.Get(256)
	if err != nil {
		t.Fatalf("二次取缓冲失败：%v", err)
	}
	if &second[0] != &first[0] {
		t.Fatal("归还的缓冲未被复用")
	}
	pool.Put(second)
}

// TestV2ReaderReleaseReturnsPooledBuffers 断言池化载荷可显式归还。
func TestV2ReaderReleaseReturnsPooledBuffers(t *testing.T) {
	pool, err := NewBufferPool(DefaultV2PayloadLimit, bufferedPoolSize)
	if err != nil {
		t.Fatalf("建立缓冲池失败：%v", err)
	}
	body := padPayload(256)
	payload := make([]byte, V2MessageTypeIDSize+len(body))
	binary.BigEndian.PutUint16(payload[0:V2MessageTypeIDSize], MessageTypeLogin.V2ID)
	copy(payload[V2MessageTypeIDSize:], body)

	raw, err := EncodeV2Frame(V2FrameTypeMessage, payload)
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}

	reader := NewV2Reader(bytes.NewReader(raw), DefaultV2PayloadLimit)
	reader.SetPool(pool)
	frame, err := reader.ReadFrame()
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	if outstanding := pool.Outstanding(); outstanding != 1 {
		t.Fatalf("读取后应有一块缓冲被借出，实际 %d", outstanding)
	}
	if string(frame.Message.Payload) != string(body) {
		t.Fatalf("正文与类型 ID 切分错误：%s", frame.Message.Payload)
	}

	frame.Release()
	if outstanding := pool.Outstanding(); outstanding != 0 {
		t.Fatalf("归还后仍有 %d 块缓冲未收回", outstanding)
	}
}

// TestV2ReaderLimitRejectsBeforeReading 断言超限声明在读缓冲前被拒绝。
func TestV2ReaderLimitRejectsBeforeReading(t *testing.T) {
	pool, err := NewBufferPool(DefaultV2PayloadLimit, bufferedPoolSize)
	if err != nil {
		t.Fatalf("建立缓冲池失败：%v", err)
	}
	reader := NewV2Reader(bytes.NewReader(v2HeaderWithLength(1<<30)), DefaultV2PayloadLimit)
	reader.SetPool(pool)

	if _, err := reader.ReadFrame(); !IsCategory(err, CategoryLengthExceeded) {
		t.Fatalf("错误类别不一致：实际 %s", CategoryOf(err))
	}
	if outstanding := pool.Outstanding(); outstanding != 0 {
		t.Fatalf("超限声明不得借出缓冲，实际 %d", outstanding)
	}
}

// TestV1ReaderPayloadLimitIsEnforcedBeforeAllocation 断言超限时不分配该长度缓冲。
func TestV1ReaderPayloadLimitIsEnforcedBeforeAllocation(t *testing.T) {
	var header [9]byte
	header[0] = MessageTypeLogin.V1Byte
	// 声明 512 MiB 载荷，但只提供帧头。
	binary.BigEndian.PutUint64(header[1:9], 512<<20)

	reader := NewV1Reader(bytes.NewReader(header[:]), DefaultV1PayloadLimit)
	start := allocationSnapshot()
	_, err := reader.ReadFrame()
	grew := allocationSnapshot() - start

	if !IsCategory(err, CategoryLengthExceeded) {
		t.Fatalf("错误类别不一致：实际 %s", CategoryOf(err))
	}
	if grew > 1<<20 {
		t.Fatalf("超限声明触发了无界分配：增长 %d 字节", grew)
	}
}

// TestV2ReaderPayloadLimitIsEnforcedBeforeAllocation 断言 v2 超限同样不分配。
func TestV2ReaderPayloadLimitIsEnforcedBeforeAllocation(t *testing.T) {
	var header [8]byte
	binary.BigEndian.PutUint16(header[0:2], V2FrameTypeMessage)
	binary.BigEndian.PutUint32(header[4:8], 1<<31)

	start := allocationSnapshot()
	_, err := DecodeV2Frame(header[:], DefaultV2PayloadLimit)
	grew := allocationSnapshot() - start

	if !IsCategory(err, CategoryLengthExceeded) {
		t.Fatalf("错误类别不一致：实际 %s", CategoryOf(err))
	}
	if grew > 1<<20 {
		t.Fatalf("超限声明触发了无界分配：增长 %d 字节", grew)
	}
}

// TestRejectionClosesConnectionAndReleasesResources 断言拒绝路径关闭连接并回收缓冲。
func TestRejectionClosesConnectionAndReleasesResources(t *testing.T) {
	pool, err := NewBufferPool(DefaultV1PayloadLimit, bufferedPoolSize)
	if err != nil {
		t.Fatalf("建立缓冲池失败：%v", err)
	}

	var sink bytes.Buffer
	recorder := &recordingConn{Conn: &bufferConn{buffer: &sink}}
	guard := NewConnectionGuard(recorder, pool, Options{MaxWireVersion: VersionV1, V2Enabled: true})

	// 首帧合法，随后是声明超限的帧：连接必须在首次协议错误时关闭。
	valid, err := EncodeV1Frame(Frame{Type: MessageTypePing, Payload: []byte(`{"timestamp":1}`)})
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	var overLimit [V1HeaderSize]byte
	overLimit[0] = MessageTypePing.V1Byte
	binary.BigEndian.PutUint64(overLimit[1:V1HeaderSize], uint64(DefaultV1PayloadLimit+1))

	reader := NewV1Reader(bytes.NewReader(append(valid, overLimit[:]...)), DefaultV1PayloadLimit)
	reader.SetPool(pool)
	guard.bindReader(reader)
	if err := guard.LockVersion(VersionV1); err != nil {
		t.Fatalf("锁定版本失败：%v", err)
	}

	for {
		frame, err := guard.ReadFrame()
		if err != nil {
			break
		}
		// 池化载荷必须显式归还，热路径不得依赖 GC 回收。
		frame.Release()
	}
	if !recorder.wasClosed() {
		t.Fatal("协议错误后连接必须被关闭")
	}
	// 拒绝路径必须释放已从池中取出的载荷，否则热路径会持续泄漏池化缓冲。
	if outstanding := pool.Outstanding(); outstanding != 0 {
		t.Fatalf("拒绝后仍有 %d 块缓冲未归还", outstanding)
	}
}

// TestConnectionGuardRejectsV2MagicOnV1Connection 断言版本一经选定不得切换。
func TestConnectionGuardRejectsV2MagicOnV1Connection(t *testing.T) {
	guard := NewConnectionGuard(&recordingConn{Conn: &bufferConn{buffer: &bytes.Buffer{}}},
		nil, Options{MaxWireVersion: VersionV1, V2Enabled: true})

	if err := guard.LockVersion(VersionV1); err != nil {
		t.Fatalf("锁定 v1 失败：%v", err)
	}
	if err := guard.requireVersion(VersionV2); !IsCategory(err, CategoryVersionNotAccepted) {
		t.Fatalf("v1 连接上出现 v2 必须被拒绝，实际 %v", err)
	}
}

// TestProtocolErrorInvariants 断言错误分类与阶段可被上层稳定判定。
func TestProtocolErrorInvariants(t *testing.T) {
	raw := make([]byte, 9)
	raw[0] = MessageTypePing.V1Byte
	binary.BigEndian.PutUint64(raw[1:9], 1)
	raw = append(raw, '{')

	_, err := DecodeV1Frame(raw, DefaultV1PayloadLimit)
	if err == nil {
		t.Fatal("期望协议错误")
	}
	if CategoryOf(err) == "" {
		t.Fatal("协议错误必须带稳定类别")
	}
	if StageOf(err) != StageMessage {
		t.Fatalf("阶段标记不一致：%s", StageOf(err))
	}
	if strings.Contains(err.Error(), "0x") {
		t.Fatalf("错误文案不应回显原始字节：%v", err)
	}
}

// TestV2UnknownMessageTypeIsRejected 断言 v2 未登记的消息类型 ID 被拒绝。
//
// 与 v1 的同类测试对称：v2 用 2 字节网络字节序 ID，未登记取值必须归为类型错误，
// 不得猜测语义放行。
func TestV2UnknownMessageTypeIsRejected(t *testing.T) {
	// 登记表最大 ID 为 13，取其加一作为未登记取值。
	unknownID := uint16(MessageTypeUDPPacket.V2ID + 1)
	if _, ok := MessageTypeByV2ID(unknownID); ok {
		t.Fatalf("测试前提失效：ID %d 已登记", unknownID)
	}

	body := []byte(`{"probe":true}`)
	payload := make([]byte, V2MessageTypeIDSize+len(body))
	binary.BigEndian.PutUint16(payload[0:V2MessageTypeIDSize], unknownID)
	copy(payload[V2MessageTypeIDSize:], body)

	raw := make([]byte, V2HeaderSize+len(payload))
	binary.BigEndian.PutUint16(raw[0:2], V2FrameTypeMessage)
	binary.BigEndian.PutUint32(raw[4:V2HeaderSize], uint32(len(payload)))
	copy(raw[V2HeaderSize:], payload)

	_, err := DecodeV2Frame(raw, DefaultV2PayloadLimit)
	if !IsCategory(err, CategoryTypeInvalid) {
		t.Fatalf("未登记类型 ID 必须归为类型错误，实际：%v", err)
	}

	// 编码侧同样拒绝：不得产出对端无法识别的帧。
	unregistered := MessageType{Name: "unregistered", V1Byte: '?', V2ID: unknownID}
	if _, err := EncodeV2MessageFrame(unregistered, body); !IsCategory(err, CategoryTypeInvalid) {
		t.Fatalf("编码未登记类型必须被拒绝，实际：%v", err)
	}
}

// TestV2ReaderTruncatedStreamIsRejected 断言 v2 流式读取的截断与传输错误被正确分类。
//
// 三个分支必须互相区分：帧头部分到达（截断）、载荷未读满（截断）、
// 底层非 EOF 读错误（传输失败）。
func TestV2ReaderTruncatedStreamIsRejected(t *testing.T) {
	buildFrame := func(payloadSize int) []byte {
		payload := make([]byte, V2MessageTypeIDSize+payloadSize)
		binary.BigEndian.PutUint16(payload[0:V2MessageTypeIDSize], MessageTypeLogin.V2ID)
		copy(payload[V2MessageTypeIDSize:], padPayload(payloadSize))
		raw := make([]byte, V2HeaderSize+len(payload))
		binary.BigEndian.PutUint16(raw[0:2], V2FrameTypeMessage)
		binary.BigEndian.PutUint32(raw[4:V2HeaderSize], uint32(len(payload)))
		copy(raw[V2HeaderSize:], payload)
		return raw
	}

	t.Run("帧头部分到达", func(t *testing.T) {
		full := buildFrame(16)
		reader := NewV2Reader(bytes.NewReader(full[:V2HeaderSize-3]), DefaultV2PayloadLimit)
		if _, err := reader.ReadFrame(); !IsCategory(err, CategoryPayloadTruncated) {
			t.Fatalf("帧头截断必须归为载荷截断，实际：%v", err)
		}
	})

	t.Run("载荷未读满", func(t *testing.T) {
		full := buildFrame(64)
		reader := NewV2Reader(bytes.NewReader(full[:len(full)-20]), DefaultV2PayloadLimit)
		if _, err := reader.ReadFrame(); !IsCategory(err, CategoryPayloadTruncated) {
			t.Fatalf("载荷截断必须归为载荷截断，实际：%v", err)
		}
	})

	t.Run("帧边界 EOF 与截断可区分", func(t *testing.T) {
		reader := NewV2Reader(bytes.NewReader(nil), DefaultV2PayloadLimit)
		if _, err := reader.ReadFrame(); err != io.EOF {
			t.Fatalf("空流在帧边界应返回 io.EOF，实际：%v", err)
		}
	})

	t.Run("底层读错误归为传输失败", func(t *testing.T) {
		full := buildFrame(32)
		// 头部读到后注入非 EOF 错误，触发传输失败分支。
		source := &failingReader{data: full, failAfter: V2HeaderSize}
		reader := NewV2Reader(source, DefaultV2PayloadLimit)
		if _, err := reader.ReadFrame(); !IsCategory(err, CategoryTransportFailure) {
			t.Fatalf("底层读错误必须归为传输失败，实际：%v", err)
		}
	})
}

// failingReader 在读出 failAfter 字节后返回非 EOF 错误，用于覆盖传输失败分支。
type failingReader struct {
	data      []byte
	offset    int
	failAfter int
}

// Read 实现 io.Reader；越过门槛后返回确定错误而非 EOF。
func (reader *failingReader) Read(p []byte) (int, error) {
	if reader.offset >= reader.failAfter {
		return 0, errInjectedTransport
	}
	remaining := reader.failAfter - reader.offset
	if remaining > len(p) {
		remaining = len(p)
	}
	copied := copy(p, reader.data[reader.offset:reader.offset+remaining])
	reader.offset += copied
	return copied, nil
}

// errInjectedTransport 是测试注入的传输层错误。
var errInjectedTransport = errors.New("注入的传输层错误")
