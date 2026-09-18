package wire

// MessageType 描述一种已支持的兼容消息类型。
//
// wire v1 用单字节 ASCII 字符标识消息种类，wire v2 用 2 字节网络字节序数字 ID。
// 两者是同一消息族的两种线上表达，因此登记在同一张表中，保证跨版本语义一致。
//
// 本表只登记已有证据支持的类型；其余消息类型的语义属于会话层规格，未登记的类型
// 一律按「不支持的消息类型」拒绝，不做猜测性补全。
type MessageType struct {
	// Name 是 JRP 自有的类型名称。
	Name string
	// V1Byte 是 wire v1 的类型字节。
	V1Byte byte
	// V2ID 是 wire v2 的类型 ID。
	V2ID uint16
}

var (
	// MessageTypeLogin 是登录消息。
	MessageTypeLogin = MessageType{Name: "login", V1Byte: 'o', V2ID: 1}
	// MessageTypeLoginResponse 是登录响应消息。
	MessageTypeLoginResponse = MessageType{Name: "login-response", V1Byte: '1', V2ID: 2}
	// MessageTypeNewProxy 是新建代理消息。
	MessageTypeNewProxy = MessageType{Name: "new-proxy", V1Byte: 'p', V2ID: 3}
	// MessageTypeNewWorkConn 是新工作连接消息。
	MessageTypeNewWorkConn = MessageType{Name: "new-work-conn", V1Byte: 'w', V2ID: 6}
	// MessageTypeStartWorkConn 是启动工作连接消息。
	MessageTypeStartWorkConn = MessageType{Name: "start-work-conn", V1Byte: 's', V2ID: 8}
	// MessageTypePing 是心跳消息。
	MessageTypePing = MessageType{Name: "ping", V1Byte: 'h', V2ID: 11}
	// MessageTypePong 是心跳响应消息。
	MessageTypePong = MessageType{Name: "pong", V1Byte: '4', V2ID: 12}
	// MessageTypeUDPPacket 是 UDP 数据包消息。
	MessageTypeUDPPacket = MessageType{Name: "udp-packet", V1Byte: 'u', V2ID: 13}
)

// supportedMessageTypes 是已登记消息类型的稳定顺序表。
var supportedMessageTypes = []MessageType{
	MessageTypeLogin,
	MessageTypeLoginResponse,
	MessageTypeNewProxy,
	MessageTypeNewWorkConn,
	MessageTypeStartWorkConn,
	MessageTypePing,
	MessageTypePong,
	MessageTypeUDPPacket,
}

// MessageTypeByName 按名称查找消息类型。
func MessageTypeByName(name string) (MessageType, bool) {
	for _, candidate := range supportedMessageTypes {
		if candidate.Name == name {
			return candidate, true
		}
	}
	return MessageType{}, false
}

// MessageTypeByV1Byte 按 wire v1 类型字节查找消息类型。
func MessageTypeByV1Byte(value byte) (MessageType, bool) {
	for _, candidate := range supportedMessageTypes {
		if candidate.V1Byte == value {
			return candidate, true
		}
	}
	return MessageType{}, false
}

// MessageTypeByV2ID 按 wire v2 类型 ID 查找消息类型。
func MessageTypeByV2ID(value uint16) (MessageType, bool) {
	for _, candidate := range supportedMessageTypes {
		if candidate.V2ID == value {
			return candidate, true
		}
	}
	return MessageType{}, false
}

// Frame 是一条编解码后的 wire v1 消息帧。
//
// 从有界池取得的载荷必须在处理完后通过 Release 显式归还。
type Frame struct {
	Type    MessageType
	Payload []byte

	release func()
}

// Release 归还该帧持有的池化载荷。未使用缓冲池时是空操作，可安全重复调用。
func (frame Frame) Release() {
	if frame.release != nil {
		frame.release()
	}
}
