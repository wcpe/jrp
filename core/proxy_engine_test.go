package core_test

import (
	"context"
	"net"
	"net/netip"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jrp/core"
	"github.com/wcpe/jrp/core/client"
	"github.com/wcpe/jrp/core/server"
)

// 本文件覆盖 FR-06a §5 的正常路径与 HTTPS 透传边界：
// 四种代理在 TCP 传输（FR-05a）上的端到端闭环，以及 HTTPS 不生成正文的断言。
// S2 只验收 TCP 传输组合：UDP/HTTP/HTTPS × WS/WSS、× KCP/QUIC 待依赖批准。

const (
	fr06aClientID    = "proxy-client"
	fr06aClientToken = "proxy-token"
	// fr06aHTTPHost 与 fr06aHTTPPath 是 FR-06a HTTP 路由用例的已配置主机与路径。
	// 定义成常量让"配置路由"与"断言响应不回显路由表"引用同一份取值，
	// 避免两处各写字面量而在改动时漂移。
	fr06aHTTPHost = "app.example.com"
	fr06aHTTPPath = "/api"
)

// controlAndFourPorts 一次性申请控制监听器与四个入口端口，全部取自
// nextFreePort 的同一区间：控制监听器保持打开供服务端复用，四个入口端口
// 释放后交给服务端绑定。同源分配保证五者互不相同。
func controlAndFourPorts(t *testing.T) (net.Listener, [4]int) {
	t.Helper()
	controlPort := nextFreePort()
	control, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(controlPort)))
	if err != nil {
		t.Fatalf("监听控制端口失败：%v", err)
	}
	ports := [4]int{nextFreePort(), nextFreePort(), nextFreePort(), nextFreePort()}
	return control, ports
}

// startLocalUDPEcho 启动一个本地 UDP 回显服务，返回其地址与关闭函数。
func startLocalUDPEcho(t *testing.T) (netip.AddrPort, func()) {
	t.Helper()
	socket, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("启动本地 UDP 回显服务失败：%v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 2048)
		for {
			read, peer, readErr := socket.ReadFrom(buffer)
			if readErr != nil {
				return
			}
			if _, writeErr := socket.WriteTo(buffer[:read], peer); writeErr != nil {
				return
			}
		}
	}()
	address, err := netip.ParseAddrPort(socket.LocalAddr().String())
	if err != nil {
		t.Fatalf("解析回显地址失败：%v", err)
	}
	return address, func() {
		_ = socket.Close()
		<-done
	}
}

// fr06aServerConfig 构造带四种代理绑定的服务端配置。
func fr06aServerConfig(t *testing.T, control netip.AddrPort, tcpTarget, udpTarget netip.AddrPort, ports [4]int) core.ServerConfig {
	t.Helper()
	config, err := core.NewServerConfig(
		core.WithListen(core.BindEndpoint{Address: control, Transport: core.TransportTCP}),
		core.WithWire(core.WireV1),
		core.WithClientCredential(core.ClientCredential{ClientID: fr06aClientID, Token: fr06aClientToken}),
		core.WithTCPProxyBinding(core.TCPProxyBinding{
			Name: "tcp-echo", ClientID: fr06aClientID, RemotePort: ports[0],
			AllowedTargets: []netip.AddrPort{tcpTarget},
		}),
		core.WithUDPProxyBinding(core.UDPProxyBinding{
			Name: "udp-echo", ClientID: fr06aClientID, RemotePort: ports[1],
			AllowedTargets: []netip.AddrPort{udpTarget},
		}),
		core.WithHTTPProxyBinding(core.HTTPProxyBinding{
			Name: "http-echo", ClientID: fr06aClientID, RemotePort: ports[2],
			Hosts:          []string{fr06aHTTPHost},
			Path:           fr06aHTTPPath,
			AllowedTargets: []netip.AddrPort{tcpTarget},
		}),
		core.WithHTTPSProxyBinding(core.HTTPSProxyBinding{
			Name: "https-echo", ClientID: fr06aClientID, RemotePort: ports[3],
			AllowedTargets: []netip.AddrPort{tcpTarget},
		}),
		core.WithServerHeartbeat(200*time.Millisecond),
		core.WithServerTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("构造服务端配置失败：%v", err)
	}
	return config
}

// fr06aClientConfig 构造与上述服务端匹配的客户端配置。
func fr06aClientConfig(t *testing.T, control netip.AddrPort, tcpTarget, udpTarget netip.AddrPort, ports [4]int) core.ClientConfig {
	t.Helper()
	config, err := core.NewClientConfig(
		core.WithClientID(fr06aClientID),
		core.WithServerEndpoint(core.ServerEndpoint{
			Address: control, Transport: core.TransportTCP, Wire: core.WireV1,
		}),
		core.WithClientAuth(core.TokenAuth{Token: fr06aClientToken}),
		core.WithTCPProxy(core.TCPProxy{Name: "tcp-echo", LocalAddr: tcpTarget, RemotePort: ports[0]}),
		core.WithUDPProxy(core.UDPProxy{Name: "udp-echo", LocalAddr: udpTarget, RemotePort: ports[1]}),
		core.WithHTTPProxy(core.HTTPProxy{Name: "http-echo", LocalAddr: tcpTarget, RemotePort: ports[2]}),
		core.WithHTTPSProxy(core.HTTPSProxy{Name: "https-echo", LocalAddr: tcpTarget, RemotePort: ports[3]}),
		core.WithHeartbeat(200*time.Millisecond),
		core.WithTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("构造客户端配置失败：%v", err)
	}
	return config
}

// startFourProxyPair 启动带四种代理的一对 Engine。
// controlListener 由调用方在同批端口中申请，避免与入口端口重复。
func startFourProxyPair(t *testing.T, ctx context.Context, tcpTarget, udpTarget netip.AddrPort, ports [4]int, controlListener net.Listener) *server.Engine {
	t.Helper()
	control, err := netip.ParseAddrPort(controlListener.Addr().String())
	if err != nil {
		t.Fatalf("解析控制地址失败：%v", err)
	}

	serverEngine := server.New(
		fr06aServerConfig(t, control, tcpTarget, udpTarget, ports),
		server.WithListener(controlListener),
	)
	if err := serverEngine.Start(ctx); err != nil {
		t.Fatalf("服务端启动失败：%v", err)
	}
	t.Cleanup(func() { _ = serverEngine.Shutdown(context.Background()) })

	clientEngine := client.New(fr06aClientConfig(t, control, tcpTarget, udpTarget, ports))
	if err := clientEngine.Start(ctx); err != nil {
		t.Fatalf("客户端启动失败：%v", err)
	}
	t.Cleanup(func() { _ = clientEngine.Shutdown(context.Background()) })
	return serverEngine
}

// TestFR06aTCPProxyRoundTrip 覆盖 TCP 代理：用户连接服务端端口后数据双向可达。
func TestFR06aTCPProxyRoundTrip(t *testing.T) {
	tcpTarget, stopEcho := startLocalEcho(t)
	defer stopEcho()
	udpTarget, stopUDP := startLocalUDPEcho(t)
	defer stopUDP()

	controlListener, ports := controlAndFourPorts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	serverEngine := startFourProxyPair(t, ctx, tcpTarget, udpTarget, ports, controlListener)

	guest, err := net.Dial("tcp", serverEngine.GuestAddr("tcp-echo").String())
	if err != nil {
		t.Fatalf("访客连接 TCP 入口失败：%v", err)
	}
	defer guest.Close()
	if err := echoOnceOn(guest, []byte("TCP 代理往返数据")); err != nil {
		t.Fatalf("TCP 代理往返失败：%v", err)
	}
}

// TestFR06aUDPProxyRoundTrip 覆盖 UDP 代理：同一对端地址的往返数据报正确送达，
// 响应回到原对端。
func TestFR06aUDPProxyRoundTrip(t *testing.T) {
	tcpTarget, stopEcho := startLocalEcho(t)
	defer stopEcho()
	udpTarget, stopUDP := startLocalUDPEcho(t)
	defer stopUDP()

	controlListener, ports := controlAndFourPorts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	serverEngine := startFourProxyPair(t, ctx, tcpTarget, udpTarget, ports, controlListener)

	clientSocket, err := net.Dial("udp", serverEngine.GuestAddr("udp-echo").String())
	if err != nil {
		t.Fatalf("拨号 UDP 入口失败：%v", err)
	}
	defer clientSocket.Close()

	payload := []byte("UDP 代理往返数据报")
	// 首个数据报用于建立会话并等待客户端的工作连接就绪，因此允许有限次重试：
	// 重发是 UDP 的正常语义，重试次数有界，不是等待无限期。
	var datagram []byte
	var lastErr error
	deadline := time.Now().Add(10 * time.Second)
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		datagram, lastErr = roundTripUDP(clientSocket, payload, 2*time.Second)
		if lastErr == nil {
			break
		}
	}
	if lastErr != nil {
		t.Fatalf("UDP 代理往返失败：%v", lastErr)
	}
	if string(datagram) != string(payload) {
		t.Fatalf("UDP 往返内容不一致：发出 %q，收到 %q", payload, datagram)
	}
}

// TestFR06aHTTPSProxyPassthrough 覆盖 HTTPS 透传：字节流双向可达，Core 不解析内容。
//
// HTTPS 透传不终止 TLS，因此这里直接走原始字节流；TLS 握手发生在最终用户与
// 被代理服务之间，Core 只搬运字节（规格 §3.6）。
func TestFR06aHTTPSProxyPassthrough(t *testing.T) {
	tcpTarget, stopEcho := startLocalEcho(t)
	defer stopEcho()
	udpTarget, stopUDP := startLocalUDPEcho(t)
	defer stopUDP()

	controlListener, ports := controlAndFourPorts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	serverEngine := startFourProxyPair(t, ctx, tcpTarget, udpTarget, ports, controlListener)

	guest, err := net.Dial("tcp", serverEngine.GuestAddr("https-echo").String())
	if err != nil {
		t.Fatalf("访客连接 HTTPS 入口失败：%v", err)
	}
	defer guest.Close()

	// 透传不解析内部字节：此处刻意写入非 HTTP 的任意字节，验证 Core 不解析。
	payload := []byte{0x16, 0x03, 0x01, 0x00, 0x05, 0x01, 0x02, 0x03, 0x04, 0x05}
	if _, err := guest.Write(payload); err != nil {
		t.Fatalf("HTTPS 入口写入失败：%v", err)
	}
	received := make([]byte, len(payload))
	if _, err := readFullWithTimeout(guest, received); err != nil {
		t.Fatalf("HTTPS 入口读回失败：%v", err)
	}
	for index := range payload {
		if payload[index] != received[index] {
			t.Fatalf("HTTPS 透传字节被修改：第 %d 字节", index)
		}
	}
}

// roundTripUDP 向 UDP 入口写出一个数据报并等待响应，返回响应内容与错误。
//
// 响应可能晚于写入到达，因此按截止时间读取而不是只等一次，避免偶发丢包让
// 测试变成不稳定用例；超时即失败，不无限等待。
func roundTripUDP(socket net.Conn, payload []byte, timeout time.Duration) ([]byte, error) {
	if _, err := socket.Write(payload); err != nil {
		return nil, err
	}
	_ = socket.SetReadDeadline(time.Now().Add(timeout))
	buffer := make([]byte, 2048)
	read, err := socket.Read(buffer)
	if err != nil {
		return nil, err
	}
	return buffer[:read], nil
}

// readFullWithTimeout 读满给定缓冲，带截止时间避免无限等待。
func readFullWithTimeout(conn net.Conn, buffer []byte) (int, error) {
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	total := 0
	for total < len(buffer) {
		read, err := conn.Read(buffer[total:])
		total += read
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// TestFR06aHTTPProxyRoutesByHostAndPath 覆盖 HTTP 代理：按主机名与路径正确路由。
func TestFR06aHTTPProxyRoutesByHostAndPath(t *testing.T) {
	tcpTarget, stopEcho := startLocalEcho(t)
	defer stopEcho()
	udpTarget, stopUDP := startLocalUDPEcho(t)
	defer stopUDP()

	controlListener, ports := controlAndFourPorts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	serverEngine := startFourProxyPair(t, ctx, tcpTarget, udpTarget, ports, controlListener)

	// 命中 app.example.com + /api 的路由：目标服务是回显服务，
	// 因此写出的请求行会被原样返回，可据此确认请求抵达了目标。
	guest, err := net.Dial("tcp", serverEngine.GuestAddr("http-echo").String())
	if err != nil {
		t.Fatalf("访客连接 HTTP 入口失败：%v", err)
	}
	defer guest.Close()

	request := "GET /api/v1/users HTTP/1.1\r\nHost: app.example.com\r\n\r\n"
	if _, err := guest.Write([]byte(request)); err != nil {
		t.Fatalf("写入 HTTP 请求失败：%v", err)
	}
	_, err = readFullWithTimeout(guest, make([]byte, len(request)))
	if err != nil {
		t.Fatalf("HTTP 请求未抵达目标服务（路由未命中）：%v", err)
	}
}

// TestFR06aHTTPUnmatchedRouteReturnsPlainResponse 覆盖未匹配路由：
// 返回明确响应且不回显内部路由表。
func TestFR06aHTTPUnmatchedRouteReturnsPlainResponse(t *testing.T) {
	tcpTarget, stopEcho := startLocalEcho(t)
	defer stopEcho()
	udpTarget, stopUDP := startLocalUDPEcho(t)
	defer stopUDP()

	controlListener, ports := controlAndFourPorts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	serverEngine := startFourProxyPair(t, ctx, tcpTarget, udpTarget, ports, controlListener)

	guest, err := net.Dial("tcp", serverEngine.GuestAddr("http-echo").String())
	if err != nil {
		t.Fatalf("访客连接 HTTP 入口失败：%v", err)
	}
	defer guest.Close()

	// 主机名不匹配任何已注册路由。
	request := "GET /api HTTP/1.1\r\nHost: unknown.example.com\r\n\r\n"
	if _, err := guest.Write([]byte(request)); err != nil {
		t.Fatalf("写入 HTTP 请求失败：%v", err)
	}
	response := make([]byte, 512)
	read, err := readAnyWithTimeout(guest, response)
	if err != nil {
		t.Fatalf("未匹配请求应当返回明确响应：%v", err)
	}
	body := string(response[:read])
	if !strings.Contains(body, "404") {
		t.Fatalf("未匹配路由应当返回 404，实际：%q", body)
	}
	// 不得回显内部路由表：已配置的主机名与路径都不出现在响应中。
	if strings.Contains(body, fr06aHTTPHost) {
		t.Fatalf("未匹配响应回显了内部路由表：%q", body)
	}
	if strings.Contains(body, fr06aHTTPPath) {
		t.Fatalf("未匹配响应回显了已配置路径：%q", body)
	}
	assertResponseContentLengthMatches(t, body)
}

// assertResponseContentLengthMatches 校验响应的 Content-Length 与正文实际字节数一致。
//
// 长度必须按**字节**比较：正文含中文，字符数与字节数不同（一个汉字 3 字节）。
// 声明值与实际不符时，合规客户端会按声明值截断正文（并可能在多字节字符中间切断），
// 而裸 socket 因为没有解析长度，读到的永远是完整字节流——这正是该缺陷此前
// 逃过测试的原因：断言只查响应里有没有 "404" 子串。
func assertResponseContentLengthMatches(t *testing.T, raw string) {
	t.Helper()
	head, tail, found := strings.Cut(raw, "\r\n\r\n")
	if !found {
		t.Fatalf("响应缺少首部与正文的分隔：%q", raw)
	}
	declared := -1
	for _, line := range strings.Split(head, "\r\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			continue
		}
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			t.Fatalf("Content-Length 不是合法整数：%q", value)
		}
		declared = parsed
	}
	if declared < 0 {
		t.Fatal("未匹配响应必须显式声明 Content-Length")
	}
	if declared != len(tail) {
		t.Fatalf("Content-Length 声明 %d 字节，正文实际 %d 字节：合规客户端会截断正文",
			declared, len(tail))
	}
	// 正文完整可读：防止"长度对了但内容被截"这类改写。
	if !strings.HasSuffix(tail, "路由") {
		t.Fatalf("未匹配响应正文不完整：%q", tail)
	}
}

// TestFR06aTargetOutsideAllowedSetIsRejected 覆盖目标地址越权：
// 客户端声明的目标不在允许集合内时，工作连接被拒绝且访客不被悬挂。
func TestFR06aTargetOutsideAllowedSetIsRejected(t *testing.T) {
	tcpTarget, stopEcho := startLocalEcho(t)
	defer stopEcho()

	// 控制端口与入口端口同批申请：分两次申请的端口可能相同，拨号会撞在一起。
	controlListener, entryPorts := controlAndFourPorts(t)
	guestPort := entryPorts[0]
	control, err := netip.ParseAddrPort(controlListener.Addr().String())
	if err != nil {
		t.Fatalf("解析控制地址失败：%v", err)
	}

	// 服务端只允许一个与客户端实际目标不同的地址：客户端声明必然越权。
	serverConfig, err := core.NewServerConfig(
		core.WithListen(core.BindEndpoint{Address: control, Transport: core.TransportTCP}),
		core.WithWire(core.WireV1),
		core.WithClientCredential(core.ClientCredential{ClientID: fr06aClientID, Token: fr06aClientToken}),
		core.WithTCPProxyBinding(core.TCPProxyBinding{
			Name:           "tcp-echo",
			ClientID:       fr06aClientID,
			RemotePort:     guestPort,
			AllowedTargets: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:1")},
		}),
		core.WithServerHeartbeat(200*time.Millisecond),
		core.WithServerTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("构造服务端配置失败：%v", err)
	}
	clientConfig, err := core.NewClientConfig(
		core.WithClientID(fr06aClientID),
		core.WithServerEndpoint(core.ServerEndpoint{
			Address: control, Transport: core.TransportTCP, Wire: core.WireV1,
		}),
		core.WithClientAuth(core.TokenAuth{Token: fr06aClientToken}),
		core.WithTCPProxy(core.TCPProxy{Name: "tcp-echo", LocalAddr: tcpTarget, RemotePort: guestPort}),
		core.WithHeartbeat(200*time.Millisecond),
		core.WithTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("构造客户端配置失败：%v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverEngine := server.New(serverConfig, server.WithListener(controlListener))
	if err := serverEngine.Start(ctx); err != nil {
		t.Fatalf("服务端启动失败：%v", err)
	}
	t.Cleanup(func() { _ = serverEngine.Shutdown(context.Background()) })
	clientEngine := client.New(clientConfig)
	if err := clientEngine.Start(ctx); err != nil {
		t.Fatalf("客户端启动失败：%v", err)
	}
	t.Cleanup(func() { _ = clientEngine.Shutdown(context.Background()) })

	// 访客能连上入口（入口已注册），但因工作连接被拒绝而收不到数据：
	// 关键断言是访客不被悬挂——服务端必须关闭它，而不是让它永久等待。
	guest, err := net.Dial("tcp", serverEngine.GuestAddr("tcp-echo").String())
	if err != nil {
		t.Fatalf("访客连接失败：%v", err)
	}
	defer guest.Close()
	// 截止时间的存在是为了让"永久悬挂"表现为失败而不是挂死整个测试；
	// 但仅断言 err != nil 不足以区分两种结局——读超时同样返回非 nil 错误，
	// 于是"悬挂"会被误判为通过。必须显式排除超时，要求读到的是关闭。
	_ = guest.SetReadDeadline(time.Now().Add(5 * time.Second))
	buffer := make([]byte, 1)
	if _, err := guest.Read(buffer); err == nil {
		t.Fatalf("目标越权时代理不应当转发任何数据")
	} else if netError, ok := err.(net.Error); ok && netError.Timeout() {
		t.Fatalf("目标越权时访客被悬挂：服务端应在拒绝工作连接后关闭访客连接，实际等到读超时")
	}
}

// readAnyWithTimeout 读取任意长度数据，带截止时间避免无限等待。
func readAnyWithTimeout(conn net.Conn, buffer []byte) (int, error) {
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	return conn.Read(buffer)
}

// TestFR06aShutdownLeavesNoResidue 覆盖关闭后资源归零：
// 带四种代理的 Engine 关闭后 goroutine 回到基线，且入口端口不再可连接。
func TestFR06aShutdownLeavesNoResidue(t *testing.T) {
	tcpTarget, stopEcho := startLocalEcho(t)
	defer stopEcho()
	udpTarget, stopUDP := startLocalUDPEcho(t)
	defer stopUDP()

	controlListener, ports := controlAndFourPorts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// 基线取启动前：关闭后必须回到同一水平（规格 §5：goroutine、监听器与连接计数归零）。
	baseline := runtime.NumGoroutine()

	control, err := netip.ParseAddrPort(controlListener.Addr().String())
	if err != nil {
		t.Fatalf("解析控制地址失败：%v", err)
	}
	serverEngine := server.New(
		fr06aServerConfig(t, control, tcpTarget, udpTarget, ports),
		server.WithListener(controlListener),
	)
	if err := serverEngine.Start(ctx); err != nil {
		t.Fatalf("服务端启动失败：%v", err)
	}
	clientEngine := client.New(fr06aClientConfig(t, control, tcpTarget, udpTarget, ports))
	if err := clientEngine.Start(ctx); err != nil {
		t.Fatalf("客户端启动失败：%v", err)
	}

	entries := []string{"tcp-echo", "udp-echo", "http-echo", "https-echo"}
	addresses := make(map[string]string, len(entries))
	for _, name := range entries {
		addresses[name] = serverEngine.GuestAddr(name).String()
	}

	// 先关闭服务端：待命与在途的工作连接由服务端持有，服务端先释放后客户端
	// 的转发循环才会结束；顺序颠倒会让客户端一直等它无法关闭的连接。
	if err := serverEngine.Shutdown(ctx); err != nil {
		t.Fatalf("服务端关闭失败：%v", err)
	}
	if err := clientEngine.Shutdown(ctx); err != nil {
		t.Fatalf("客户端关闭失败：%v", err)
	}

	// 四种入口都不再可连接。
	for _, name := range entries {
		address := addresses[name]
		if _, err := net.DialTimeout("tcp", address, 300*time.Millisecond); err == nil {
			t.Fatalf("关闭后入口 %s（%s）仍可连接", name, address)
		}
	}

	// goroutine 回到基线：等待上限内收敛，超时即判定为残留。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("关闭后 goroutine 未回到基线：基线 %d，当前 %d", baseline, runtime.NumGoroutine())
}
