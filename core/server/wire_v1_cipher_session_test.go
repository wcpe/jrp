package server

import (
	"encoding/json"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/wcpe/jrp/core"
	"github.com/wcpe/jrp/core/internal/wire"
)

// officialLoginPayload 构造官方形态的登录载荷（md5 鉴权材料 + 官方字段名）。
func officialLoginPayload(t *testing.T, clientID, token string) []byte {
	t.Helper()
	timestamp := time.Now().Unix()
	body, err := json.Marshal(map[string]any{
		"client_id":     clientID,
		"privilege_key": officialPrivilegeKey(token, timestamp),
		"timestamp":     timestamp,
		"run_id":        "",
		"version":       "compat-test",
	})
	if err != nil {
		t.Fatalf("编码官方登录载荷失败：%v", err)
	}
	frame, err := wire.EncodeV1Frame(wire.Frame{Type: wire.MessageTypeLogin, Payload: body})
	if err != nil {
		t.Fatalf("编码登录帧失败：%v", err)
	}
	return frame
}

// 官方形态客户端：明文完成登录握手后，控制通道切换为 AES-128-CFB 加密，
// 其后的消息（代理注册）走密文——服务端必须按同一密钥解密。
//
// 这条用例覆盖官方 frpc 的真实行为顺序：登录响应之后才切换，两个方向的 IV
// 各自独立，切换点两侧必须一致。
func TestOfficialStyleSessionSwitchesToCipher(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听控制端口失败：%v", err)
	}
	config, err := core.NewServerConfig(
		core.WithListen(core.BindEndpoint{
			Address:   netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(listener.Addr().(*net.TCPAddr).Port)),
			Transport: core.TransportTCP,
		}),
		core.WithWire(core.WireV1),
		core.WithServerHeartbeat(300*time.Millisecond),
		core.WithClientCredential(core.ClientCredential{
			ClientID:    "official",
			Token:       DigestToken("plain-token"),
			CompatToken: "plain-token",
		}),
	)
	if err != nil {
		t.Fatalf("构造配置失败：%v", err)
	}
	engine := New(config, WithListener(listener))
	if err := engine.Start(t.Context()); err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	t.Cleanup(func() { _ = engine.Shutdown(t.Context()) })

	raw, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("连接失败：%v", err)
	}
	defer raw.Close()

	// 第一步：明文登录握手。
	if _, err := raw.Write(officialLoginPayload(t, "official", "plain-token")); err != nil {
		t.Fatalf("发送登录失败：%v", err)
	}
	_ = raw.SetReadDeadline(time.Now().Add(3 * time.Second))
	responseReader := wire.NewV1Reader(raw, wire.DefaultV1PayloadLimit)
	frame, err := responseReader.ReadFrame()
	if err != nil {
		t.Fatalf("读取登录响应失败：%v", err)
	}
	var response loginResponsePayload
	if err := json.Unmarshal(frame.Payload, &response); err != nil {
		t.Fatalf("解析登录响应失败：%v", err)
	}
	frame.Release()
	if response.Error != "" {
		t.Fatalf("登录被拒：%s", response.Error)
	}
	if response.RunID == "" {
		t.Fatal("登录响应应携带服务端分配的运行 ID")
	}

	// 第二步：切换加密通道，其后的消息走密文（与官方 frpc 相同）。
	key, err := wire.V1ControlCipherKey("plain-token")
	if err != nil {
		t.Fatalf("派生通道密钥失败：%v", err)
	}
	encryptedWriter, err := wire.NewV1CipherWriter(raw, key)
	if err != nil {
		t.Fatalf("构造加密写入器失败：%v", err)
	}
	encryptedReader := wire.NewV1CipherReader(raw, key)
	readEncryptedFrame := func() (wire.Frame, error) {
		reader := wire.NewV1Reader(encryptedReader, wire.DefaultV1PayloadLimit)
		return reader.ReadFrame()
	}

	remotePort := reserveTestPortForRuntime(t)
	target := netip.MustParseAddrPort("127.0.0.1:9")
	if _, err := encryptedWriter.Write(encodeTestNewProxy(t, "official-proxy", "tcp", remotePort, target)); err != nil {
		t.Fatalf("发送加密注册帧失败：%v", err)
	}

	// 第三步：读取加密的注册响应并断言服务端已完成注册。
	_ = raw.SetReadDeadline(time.Now().Add(5 * time.Second))
	registrationFrame, err := readEncryptedFrame()
	if err != nil {
		t.Fatalf("读取加密注册响应失败（说明服务端未按同一密钥解密）：%v", err)
	}
	var registration proxyOperationResponse
	if err := json.Unmarshal(registrationFrame.Payload, &registration); err != nil {
		t.Fatalf("解析注册响应失败：%v", err)
	}
	registrationFrame.Release()
	if registration.Error != "" {
		t.Fatalf("官方形态注册被拒：%s", registration.Error)
	}
	if engine.GuestAddr("official-proxy") == nil {
		t.Fatal("注册成功后入口应可用")
	}
}
