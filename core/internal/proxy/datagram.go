package proxy

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"time"

	"github.com/wcpe/jrp/core/internal/wire"
)

// readDeadlineStep 是数据报入口的单次读截止时间。
//
// 读取循环必须可被停止流程中断，因此按短步长时间片轮询上下文而不是永久阻塞。
const readDeadlineStep = 200 * time.Millisecond

// errDatagramInvalid 表示工作连接回传的数据报帧不合法。
var errDatagramInvalid = errors.New("工作连接回传的数据报帧不合法")

// encodeDatagram 把数据报原文编码为可写入工作连接的 wire 帧。
//
// 原文经 base64 编码后承载：数据报可能是任意二进制，而 wire v1 帧载荷要求
// 合法 JSON，直接放入会破坏帧校验。
func encodeDatagram(datagram []byte) ([]byte, error) {
	body, err := json.Marshal(udpDatagram{Data: base64.StdEncoding.EncodeToString(datagram)})
	if err != nil {
		return nil, err
	}
	return wire.EncodeV1FrameWithLimit(
		wire.Frame{Type: wire.MessageTypeUDPPacket, Payload: body}, udpDatagramFrameLimit)
}

// decodeDatagram 从 wire 帧还原数据报原文；帧类型或载荷不符时返回假。
func decodeDatagram(frame wire.Frame) ([]byte, bool) {
	if frame.Type != wire.MessageTypeUDPPacket {
		return nil, false
	}
	var body udpDatagram
	if err := json.Unmarshal(frame.Payload, &body); err != nil {
		return nil, false
	}
	datagram, err := base64.StdEncoding.DecodeString(body.Data)
	if err != nil {
		return nil, false
	}
	return datagram, true
}

// writeDatagramFrame 整帧写入工作连接，短写即视为连接失效。
func writeDatagramFrame(target net.Conn, frame []byte) error {
	written, err := target.Write(frame)
	if err != nil {
		return err
	}
	if written != len(frame) {
		return errors.New("数据报帧写入不完整")
	}
	return nil
}
