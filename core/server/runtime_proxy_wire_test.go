package server

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/wcpe/jrp/core/internal/wire"
)

// encodeTestLogin 构造登录帧（测试辅助）。
func encodeTestLogin(t *testing.T, clientID, token string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"client_id":     clientID,
		"privilege_key": token,
		"timestamp":     time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("编码登录失败：%v", err)
	}
	frame, err := wire.EncodeV1Frame(wire.Frame{Type: wire.MessageTypeLogin, Payload: body})
	if err != nil {
		t.Fatalf("编码登录帧失败：%v", err)
	}
	return frame
}

// readLoginResponse 读取并断言登录成功（测试辅助）。
func readLoginResponse(t *testing.T, raw net.Conn) {
	t.Helper()
	_ = raw.SetReadDeadline(time.Now().Add(3 * time.Second))
	defer func() { _ = raw.SetReadDeadline(time.Time{}) }()
	reader := wire.NewV1Reader(raw, wire.DefaultV1PayloadLimit)
	frame, err := reader.ReadFrame()
	if err != nil {
		t.Fatalf("读取登录响应失败：%v", err)
	}
	defer frame.Release()
	var response loginResponsePayload
	if err := json.Unmarshal(frame.Payload, &response); err != nil {
		t.Fatalf("解析登录响应失败：%v", err)
	}
	if response.Error != "" {
		t.Fatalf("登录被拒：%s", response.Error)
	}
}

// encodeTestNewProxy 构造 new-proxy 帧（测试辅助）。
func encodeTestNewProxy(t *testing.T, name, proxyType string, remotePort int, target interface{ String() string }) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{
		"proxy_name":  name,
		"proxy_type":  proxyType,
		"remote_port": remotePort,
		"target":      target.String(),
	})
	if err != nil {
		t.Fatalf("编码注册失败：%v", err)
	}
	frame, err := wire.EncodeV1Frame(wire.Frame{Type: wire.MessageTypeNewProxy, Payload: body})
	if err != nil {
		t.Fatalf("编码注册帧失败：%v", err)
	}
	return frame
}

// encodeTestNewProxyWithFields 构造带官方可选字段的 new-proxy 帧。
func encodeTestNewProxyWithFields(t *testing.T, name, proxyType string, remotePort int, fields map[string]any) []byte {
	t.Helper()
	bodyFields := map[string]any{
		"proxy_name":  name,
		"proxy_type":  proxyType,
		"remote_port": remotePort,
	}
	for key, value := range fields {
		bodyFields[key] = value
	}
	body, err := json.Marshal(bodyFields)
	if err != nil {
		t.Fatalf("编码注册失败：%v", err)
	}
	frame, err := wire.EncodeV1Frame(wire.Frame{Type: wire.MessageTypeNewProxy, Payload: body})
	if err != nil {
		t.Fatalf("编码注册帧失败：%v", err)
	}
	return frame
}

// encodeTestCloseProxy 构造 close-proxy 帧（测试辅助）。
func encodeTestCloseProxy(t *testing.T, name string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]string{"proxy_name": name})
	if err != nil {
		t.Fatalf("编码关闭失败：%v", err)
	}
	frame, err := wire.EncodeV1Frame(wire.Frame{Type: wire.MessageTypeCloseProxy, Payload: body})
	if err != nil {
		t.Fatalf("编码关闭帧失败：%v", err)
	}
	return frame
}

// readTestProxyResponse 读取注册响应并按 ok 断言（测试辅助）。
func readTestProxyResponse(t *testing.T, raw net.Conn, timeout time.Duration) error {
	t.Helper()
	_ = raw.SetReadDeadline(time.Now().Add(timeout))
	defer func() { _ = raw.SetReadDeadline(time.Time{}) }()
	reader := wire.NewV1Reader(raw, wire.DefaultV1PayloadLimit)
	frame, err := reader.ReadFrame()
	if err != nil {
		t.Fatalf("读取注册响应失败：%v", err)
	}
	defer frame.Release()
	var response proxyOperationResponse
	if err := json.Unmarshal(frame.Payload, &response); err != nil {
		t.Fatalf("解析注册响应失败：%v", err)
	}
	if response.Error != "" {
		return errProxyRejected{message: response.Error}
	}
	return nil
}

// errProxyRejected 表示注册/关闭被服务端拒绝。
type errProxyRejected struct{ message string }

func (e errProxyRejected) Error() string { return e.message }
