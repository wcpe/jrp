package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
)

const (
	// V1HeaderSize 是 wire v1 帧头宽度：1 字节类型 + 8 字节载荷长度。
	V1HeaderSize = 9
	// V1LengthFieldSize 是 wire v1 载荷长度字段宽度。
	V1LengthFieldSize = 8
	// DefaultV1PayloadLimit 是 wire v1 载荷上限。
	// 该值属实现上限而非协议常量，可在自有上限范围内取更严格的值。
	DefaultV1PayloadLimit = 10240
)

// EncodeV1Frame 按默认上限编码一条 wire v1 帧。
func EncodeV1Frame(frame Frame) ([]byte, error) {
	return EncodeV1FrameWithLimit(frame, DefaultV1PayloadLimit)
}

// EncodeV1FrameWithLimit 按给定上限编码一条 wire v1 帧。
//
// 载荷长度以 8 字节有符号整数、网络字节序写入。超限载荷直接拒绝，不产出半成品帧。
func EncodeV1FrameWithLimit(frame Frame, limit int) ([]byte, error) {
	if _, ok := MessageTypeByV1Byte(frame.Type.V1Byte); !ok {
		return nil, protocolError(CategoryTypeInvalid, StageMessage, "消息类型未登记")
	}
	if err := ensurePayloadWithinLimit(len(frame.Payload), limit, StageMessage); err != nil {
		return nil, err
	}

	encoded := make([]byte, V1HeaderSize+len(frame.Payload))
	encoded[0] = frame.Type.V1Byte
	binary.BigEndian.PutUint64(encoded[1:V1HeaderSize], uint64(int64(len(frame.Payload))))
	copy(encoded[V1HeaderSize:], frame.Payload)
	return encoded, nil
}

// DecodeV1Frame 解码一段完整的 wire v1 帧字节。
//
// 校验顺序固定为：类型字节有效 → 长度为非负 → 长度不超上限 → 载荷读满。
// 长度校验先于任何按声明长度的分配。
func DecodeV1Frame(raw []byte, limit int) (Frame, error) {
	if len(raw) < V1HeaderSize {
		return Frame{}, protocolError(CategoryPayloadTruncated, StageMessage, "帧头不完整，缺少类型或长度字段")
	}
	messageType, ok := MessageTypeByV1Byte(raw[0])
	if !ok {
		return Frame{}, protocolError(CategoryTypeInvalid, StageMessage, "消息类型字节未登记")
	}

	declared := int64(binary.BigEndian.Uint64(raw[1:V1HeaderSize]))
	if declared < 0 {
		return Frame{}, protocolError(CategoryLengthInvalid, StageMessage, "载荷长度为负值")
	}
	if err := ensurePayloadWithinLimit(int(declared), limit, StageMessage); err != nil {
		return Frame{}, err
	}

	body := raw[V1HeaderSize:]
	if len(body) < int(declared) {
		return Frame{}, protocolError(CategoryPayloadTruncated, StageMessage, "载荷未读满声明长度")
	}
	payload := body[:declared]

	if err := validateJSONObject(payload); err != nil {
		return Frame{}, err
	}
	return Frame{Type: messageType, Payload: payload}, nil
}

// V1Reader 按帧边界从字节流读取 wire v1 消息。
type V1Reader struct {
	source io.Reader
	limit  int
	pool   *BufferPool
	buffer []byte
}

// NewV1Reader 建立 wire v1 读取器。
func NewV1Reader(source io.Reader, limit int) *V1Reader {
	return &V1Reader{source: source, limit: limit, buffer: make([]byte, V1HeaderSize)}
}

// SetPool 设置载荷缓冲来源。未设置时按需分配，设置后从有界池取用。
func (reader *V1Reader) SetPool(pool *BufferPool) {
	reader.pool = pool
}

// ReadFrame 读取一条 v1 帧。
//
// 帧边界处的 EOF 返回 io.EOF；帧内截断返回协议错误，两者必须区分。
// 返回帧携带的载荷在被下一条帧覆盖前有效。
func (reader *V1Reader) ReadFrame() (Frame, error) {
	if _, err := io.ReadFull(reader.source, reader.buffer); err != nil {
		return Frame{}, reader.frameHeaderError(err)
	}
	messageType, ok := MessageTypeByV1Byte(reader.buffer[0])
	if !ok {
		return Frame{}, protocolError(CategoryTypeInvalid, StageMessage, "消息类型字节未登记")
	}

	declared := int64(binary.BigEndian.Uint64(reader.buffer[1:V1HeaderSize]))
	if declared < 0 {
		return Frame{}, protocolError(CategoryLengthInvalid, StageMessage, "载荷长度为负值")
	}
	if err := ensurePayloadWithinLimit(int(declared), reader.limit, StageMessage); err != nil {
		return Frame{}, err
	}
	return reader.readPayload(messageType, int(declared))
}

// frameHeaderError 区分「干净的帧边界结束」与「帧内截断」。
func (reader *V1Reader) frameHeaderError(err error) error {
	if err == io.EOF {
		return io.EOF
	}
	if err == io.ErrUnexpectedEOF {
		return protocolError(CategoryPayloadTruncated, StageMessage, "帧头在流结束前被截断")
	}
	return protocolError(CategoryTransportFailure, StageMessage, "读取帧头失败")
}

// readPayload 按已校验长度读取载荷并校验 JSON。
func (reader *V1Reader) readPayload(messageType MessageType, size int) (Frame, error) {
	payload, release, err := reader.acquire(size)
	if err != nil {
		return Frame{}, err
	}
	if _, err := io.ReadFull(reader.source, payload); err != nil {
		release()
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return Frame{}, protocolError(CategoryPayloadTruncated, StageMessage, "载荷未读满声明长度")
		}
		return Frame{}, protocolError(CategoryTransportFailure, StageMessage, "读取载荷失败")
	}
	if err := validateJSONObject(payload); err != nil {
		release()
		return Frame{}, err
	}
	return Frame{Type: messageType, Payload: payload, release: release}, nil
}

// acquire 从有界池或按需取得载荷缓冲，并返回归还函数。
func (reader *V1Reader) acquire(size int) ([]byte, func(), error) {
	if reader.pool == nil {
		return make([]byte, size), func() {}, nil
	}
	payload, err := reader.pool.Get(size)
	if err != nil {
		return nil, nil, err
	}
	return payload, func() { reader.pool.Put(payload) }, nil
}

// ensurePayloadWithinLimit 在分配之前校验声明长度。
func ensurePayloadWithinLimit(size, limit int, stage Stage) error {
	if size < 0 {
		return protocolError(CategoryLengthInvalid, stage, "载荷长度为负值")
	}
	if size > limit {
		return protocolError(CategoryLengthExceeded, stage, "声明载荷超过上限")
	}
	return nil
}

// validateJSONObject 校验载荷是合法 JSON 对象。
//
// 未知字段按兼容语义忽略：只校验结构合法性，不改写、不回显、不提升为管理配置。
func validateJSONObject(payload []byte) error {
	if len(payload) == 0 {
		return protocolError(CategoryJSONInvalid, StageMessage, "载荷为空")
	}
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return protocolError(CategoryJSONInvalid, StageMessage, "载荷不是 JSON 对象")
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		return protocolError(CategoryJSONInvalid, StageMessage, "载荷不是合法 JSON")
	}
	return nil
}
