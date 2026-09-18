package wire

import (
	"encoding/binary"
	"io"
)

// V2 线上常量。
const (
	// V2HeaderSize 是 wire v2 固定帧头宽度：类型 2 + flags 2 + 载荷长度 4。
	V2HeaderSize = 8
	// V2TypeFieldSize 是帧类型字段宽度。
	V2TypeFieldSize = 2
	// V2FlagsFieldSize 是 flags 字段宽度。
	V2FlagsFieldSize = 2
	// V2LengthFieldSize 是载荷长度字段宽度。
	V2LengthFieldSize = 4
	// V2MessageTypeIDSize 是消息帧载荷起始的类型 ID 宽度。
	V2MessageTypeIDSize = 2
	// DefaultV2PayloadLimit 是 wire v2 单帧载荷上限。
	DefaultV2PayloadLimit = 65536
)

// V2 帧类型。
const (
	// V2FrameTypeClientHello 是客户端 hello。
	V2FrameTypeClientHello uint16 = 1
	// V2FrameTypeServerHello 是服务端 hello。
	V2FrameTypeServerHello uint16 = 2
	// V2FrameTypeMessage 是消息帧。
	V2FrameTypeMessage uint16 = 16
)

// V2Magic 是 wire v2 版本魔数。
//
// 判定失败时全部已预读字节必须回放，因此魔数是唯一的版本判据。
var V2Magic = []byte{0x46, 0x52, 0x50, 0x00, 0x02, 0x0d, 0x0a}

// V2Frame 是一条 wire v2 帧。
//
// 消息帧的载荷以类型 ID 起始，payload 字段是不含类型 ID 的正文。
type V2Frame struct {
	FrameType uint16
	Flags     uint16
	Message   MessageFrame

	release func()
}

// MessageFrame 是消息帧的类型与正文。
type MessageFrame struct {
	Type    MessageType
	Payload []byte
}

// EncodeV2Frame 编码一条 wire v2 帧。
func EncodeV2Frame(frameType uint16, payload []byte) ([]byte, error) {
	return EncodeV2FrameWithLimit(frameType, payload, DefaultV2PayloadLimit)
}

// EncodeV2FrameWithLimit 按给定上限编码 wire v2 帧。
// flags 恒为 0：P1 尚未定义任何 flags 位语义，编码侧不得写入未知位。
func EncodeV2FrameWithLimit(frameType uint16, payload []byte, limit int) ([]byte, error) {
	if !isDefinedV2FrameType(frameType) {
		return nil, protocolError(CategoryFrameTypeInvalid, StageMessage, "帧类型未定义")
	}
	if err := ensurePayloadWithinLimit(len(payload), limit, StageMessage); err != nil {
		return nil, err
	}

	encoded := make([]byte, V2HeaderSize+len(payload))
	binary.BigEndian.PutUint16(encoded[0:2], frameType)
	binary.BigEndian.PutUint16(encoded[2:4], 0)
	binary.BigEndian.PutUint32(encoded[4:V2HeaderSize], uint32(len(payload)))
	copy(encoded[V2HeaderSize:], payload)
	return encoded, nil
}

// DecodeV2Frame 解码一条完整的 wire v2 帧。
//
// 校验顺序固定为：帧头完整 → 帧类型已定义 → flags 为零 → 长度不超上限 → 载荷读满。
func DecodeV2Frame(raw []byte, limit int) (V2Frame, error) {
	if len(raw) < V2HeaderSize {
		return V2Frame{}, protocolError(CategoryPayloadTruncated, StageMessage, "帧头不完整")
	}
	frameType := binary.BigEndian.Uint16(raw[0:2])
	if !isDefinedV2FrameType(frameType) {
		return V2Frame{}, protocolError(CategoryFrameTypeInvalid, StageMessage, "帧类型未定义")
	}

	flags := binary.BigEndian.Uint16(raw[2:4])
	if flags != 0 {
		return V2Frame{}, protocolError(CategoryFlagsUnsupported, StageMessage, "收到非零 flags")
	}

	declared := int(binary.BigEndian.Uint32(raw[4:V2HeaderSize]))
	if err := ensurePayloadWithinLimit(declared, limit, StageMessage); err != nil {
		return V2Frame{}, err
	}

	body := raw[V2HeaderSize:]
	if len(body) < declared {
		return V2Frame{}, protocolError(CategoryPayloadTruncated, StageMessage, "载荷未读满声明长度")
	}
	payload := body[:declared]

	frame := V2Frame{FrameType: frameType, Flags: flags}
	if frameType != V2FrameTypeMessage {
		frame.Message.Payload = payload
		return frame, nil
	}
	message, err := decodeMessagePayload(payload)
	if err != nil {
		return V2Frame{}, err
	}
	frame.Message = message
	return frame, nil
}

// decodeMessagePayload 解析消息帧载荷：2 字节网络字节序类型 ID + JSON 正文。
func decodeMessagePayload(payload []byte) (MessageFrame, error) {
	if len(payload) < V2MessageTypeIDSize {
		return MessageFrame{}, protocolError(CategoryTypeInvalid, StageMessage, "消息帧载荷不足类型 ID 宽度")
	}
	messageType, ok := MessageTypeByV2ID(binary.BigEndian.Uint16(payload[0:V2MessageTypeIDSize]))
	if !ok {
		return MessageFrame{}, protocolError(CategoryTypeInvalid, StageMessage, "消息类型 ID 未登记")
	}
	body := payload[V2MessageTypeIDSize:]
	if err := validateJSONObject(body); err != nil {
		return MessageFrame{}, err
	}
	return MessageFrame{Type: messageType, Payload: body}, nil
}

// isDefinedV2FrameType 判断帧类型是否已定义。
func isDefinedV2FrameType(frameType uint16) bool {
	switch frameType {
	case V2FrameTypeClientHello, V2FrameTypeServerHello, V2FrameTypeMessage:
		return true
	default:
		return false
	}
}

// EncodeV2MessageFrame 编码一条消息帧：类型 ID 前缀 + JSON 正文。
func EncodeV2MessageFrame(messageType MessageType, payload []byte) ([]byte, error) {
	if _, ok := MessageTypeByV2ID(messageType.V2ID); !ok {
		return nil, protocolError(CategoryTypeInvalid, StageMessage, "消息类型未登记")
	}
	if err := validateJSONObject(payload); err != nil {
		return nil, err
	}

	body := make([]byte, V2MessageTypeIDSize+len(payload))
	binary.BigEndian.PutUint16(body[0:V2MessageTypeIDSize], messageType.V2ID)
	copy(body[V2MessageTypeIDSize:], payload)
	return EncodeV2Frame(V2FrameTypeMessage, body)
}

// V2Reader 按帧边界从字节流读取 wire v2 帧。
type V2Reader struct {
	source io.Reader
	limit  int
	header []byte
	pool   *BufferPool
}

// SetLimit 收紧读取器的载荷上限。
//
// 协商达成后的上限取双方较小值：只能收紧不能放宽，放宽请求被忽略，
// 避免对端通过声明超大上限绕过 Core 自身的实现上限。
func (reader *V2Reader) SetLimit(limit int) {
	if limit <= 0 || limit >= reader.limit {
		return
	}
	reader.limit = limit
}

// NewV2Reader 建立 wire v2 读取器。
func NewV2Reader(source io.Reader, limit int) *V2Reader {
	return &V2Reader{source: source, limit: limit, header: make([]byte, V2HeaderSize)}
}

// SetPool 设置载荷缓冲来源。未设置时按需分配，设置后从有界池取用。
func (reader *V2Reader) SetPool(pool *BufferPool) {
	reader.pool = pool
}

// ReadFrame 读取一条 v2 帧；帧边界处的 EOF 返回 io.EOF。
func (reader *V2Reader) ReadFrame() (V2Frame, error) {
	if _, err := io.ReadFull(reader.source, reader.header); err != nil {
		if err == io.EOF {
			return V2Frame{}, io.EOF
		}
		return V2Frame{}, protocolError(CategoryPayloadTruncated, StageMessage, "帧头在流结束前被截断")
	}
	frameType := binary.BigEndian.Uint16(reader.header[0:2])
	if !isDefinedV2FrameType(frameType) {
		return V2Frame{}, protocolError(CategoryFrameTypeInvalid, StageMessage, "帧类型未定义")
	}
	flags := binary.BigEndian.Uint16(reader.header[2:4])
	if flags != 0 {
		return V2Frame{}, protocolError(CategoryFlagsUnsupported, StageMessage, "收到非零 flags")
	}

	// 先校验长度，再取缓冲：超限声明不得触发分配。
	declared := int(binary.BigEndian.Uint32(reader.header[4:V2HeaderSize]))
	if err := ensurePayloadWithinLimit(declared, reader.limit, StageMessage); err != nil {
		return V2Frame{}, err
	}

	payload, release, err := reader.acquire(declared)
	if err != nil {
		return V2Frame{}, err
	}
	if _, err := io.ReadFull(reader.source, payload); err != nil {
		release()
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return V2Frame{}, protocolError(CategoryPayloadTruncated, StageMessage, "载荷未读满声明长度")
		}
		return V2Frame{}, protocolError(CategoryTransportFailure, StageMessage, "读取载荷失败")
	}

	frame := V2Frame{FrameType: frameType, Flags: flags}
	if frameType != V2FrameTypeMessage {
		frame.Message.Payload = payload
		frame.release = release
		return frame, nil
	}
	message, err := decodeMessagePayload(payload)
	if err != nil {
		release()
		return V2Frame{}, err
	}
	// 消息帧正文在类型 ID 之后：切掉前缀即得到正文视图，无需二次拷贝。
	message.Payload = message.Payload[:len(message.Payload):len(message.Payload)]
	frame.Message = message
	frame.release = release
	return frame, nil
}

// acquire 从有界池或按需取得载荷缓冲，并返回归还函数。
func (reader *V2Reader) acquire(size int) ([]byte, func(), error) {
	if reader.pool == nil {
		return make([]byte, size), func() {}, nil
	}
	payload, err := reader.pool.Get(size)
	if err != nil {
		return nil, nil, err
	}
	return payload, func() { reader.pool.Put(payload) }, nil
}

// Release 归还该帧持有的池化载荷。未使用缓冲池时是空操作。
func (frame V2Frame) Release() {
	if frame.release != nil {
		frame.release()
	}
}
