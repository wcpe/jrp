package proxy_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"

	"github.com/wcpe/jrp/core/internal/wire"
)

// UDP 数据报经工作连接承载时使用 wire v1 的 `udp-packet` 帧：载荷是形如
// {"data":"<base64>"} 的对象，因为数据报原文可能是任意二进制而 wire 帧载荷
// 要求合法 JSON。测试侧按同一形状编解码，不另发明一套分帧。

// datagramFrame 是数据报帧的承载形态，与实现侧保持一致。
type datagramFrame struct {
	Data string `json:"data"`
}

// encodeDatagram 把一个数据报编码为 udp-packet 帧。
func encodeDatagram(payload []byte, limit int) ([]byte, error) {
	body, err := json.Marshal(datagramFrame{Data: base64.StdEncoding.EncodeToString(payload)})
	if err != nil {
		return nil, err
	}
	return wire.EncodeV1FrameWithLimit(
		wire.Frame{Type: wire.MessageTypeUDPPacket, Payload: body}, limit)
}

// readDatagram 从工作连接读出一个 udp-packet 帧并还原数据报原文。
func readDatagram(source net.Conn, limit int) ([]byte, error) {
	reader := wire.NewV1Reader(source, limit)
	frame, err := reader.ReadFrame()
	if err != nil {
		return nil, err
	}
	defer frame.Release()
	if frame.Type != wire.MessageTypeUDPPacket {
		return nil, errors.New("帧类型不是 udp-packet")
	}
	var body datagramFrame
	if err := json.Unmarshal(frame.Payload, &body); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(body.Data)
}
