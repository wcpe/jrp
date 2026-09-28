package core_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/netip"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jrp/core"
	"github.com/wcpe/jrp/core/client"
	"github.com/wcpe/jrp/core/internal/wire"
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
	return fr06aServerConfigWithHTTPTarget(t, control, tcpTarget, udpTarget, ports, tcpTarget)
}

// fr06aServerConfigWithHTTPTarget 构造带四种代理绑定的服务端配置，并允许
// HTTP 绑定指向独立的目标服务。
//
// HTTP 用例需要一个能区分"请求抵达目标"与"服务端把请求回吐给访客"的目标：
// 纯回显服务两者产出的字节相同，无法证明请求真的走到了目标。
func fr06aServerConfigWithHTTPTarget(
	t *testing.T, control netip.AddrPort, tcpTarget, udpTarget netip.AddrPort, ports [4]int,
	httpTarget netip.AddrPort,
) core.ServerConfig {
	t.Helper()
	config, err := core.NewServerConfig(
		core.WithListen(core.BindEndpoint{Address: control, Transport: core.TransportTCP}),
		core.WithWire(core.WireV1),
		core.WithClientCredential(core.ClientCredential{ClientID: fr06aClientID, Token: server.DigestToken(fr06aClientToken)}),
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
			AllowedTargets: []netip.AddrPort{httpTarget},
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
	return fr06aClientConfigWithHTTPTarget(t, control, tcpTarget, udpTarget, ports, tcpTarget)
}

// fr06aClientConfigWithHTTPTarget 构造客户端配置，并允许 HTTP 代理指向独立目标。
//
// HTTP 代理声明的本地目标必须与服务端该绑定的 AllowedTargets 一致：两者不一致
// 时服务端会按越权拒绝该工作连接，表现为该代理永远没有待命连接、访客只能暂存。
func fr06aClientConfigWithHTTPTarget(
	t *testing.T, control netip.AddrPort, tcpTarget, udpTarget netip.AddrPort, ports [4]int,
	httpTarget netip.AddrPort,
) core.ClientConfig {
	t.Helper()
	config, err := core.NewClientConfig(
		core.WithClientID(fr06aClientID),
		core.WithServerEndpoint(core.ServerEndpoint{
			Address: control, Transport: core.TransportTCP, Wire: core.WireV1,
		}),
		core.WithClientAuth(core.TokenAuth{Token: fr06aClientToken}),
		core.WithTCPProxy(core.TCPProxy{Name: "tcp-echo", LocalAddr: tcpTarget, RemotePort: ports[0]}),
		core.WithUDPProxy(core.UDPProxy{Name: "udp-echo", LocalAddr: udpTarget, RemotePort: ports[1]}),
		core.WithHTTPProxy(core.HTTPProxy{Name: "http-echo", LocalAddr: httpTarget, RemotePort: ports[2]}),
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
//
// httpTarget 可省略；给出时服务端 HTTP 绑定与客户端 HTTP 代理都改用它，两侧必须
// 同时改：只改一侧会让客户端声明的目标落在服务端允许集合之外，工作连接被按越权
// 拒绝，表现为该代理永远没有待命连接。
func startFourProxyPair(
	t *testing.T, ctx context.Context, tcpTarget, udpTarget netip.AddrPort,
	ports [4]int, controlListener net.Listener, httpTarget ...netip.AddrPort,
) *server.Engine {
	t.Helper()
	control, err := netip.ParseAddrPort(controlListener.Addr().String())
	if err != nil {
		t.Fatalf("解析控制地址失败：%v", err)
	}

	httpSide := tcpTarget
	if len(httpTarget) > 0 {
		httpSide = httpTarget[0]
	}
	serverEngine := server.New(
		fr06aServerConfigWithHTTPTarget(t, control, tcpTarget, udpTarget, ports, httpSide),
		server.WithListener(controlListener),
	)
	if err := serverEngine.Start(ctx); err != nil {
		t.Fatalf("服务端启动失败：%v", err)
	}
	t.Cleanup(func() { _ = serverEngine.Shutdown(context.Background()) })

	clientEngine := client.New(fr06aClientConfigWithHTTPTarget(t, control, tcpTarget, udpTarget, ports, httpSide))
	if err := clientEngine.Start(ctx); err != nil {
		t.Fatalf("客户端启动失败：%v", err)
	}
	t.Cleanup(func() { _ = clientEngine.Shutdown(context.Background()) })
	return serverEngine
}

// httpProbeMarker 是 HTTP 探针服务在回显请求后追加的标记。
//
// 它由目标侧产生，代理无从伪造：只有真正抵达目标并读到其响应的请求才会带回它。
// 纯回显服务无法区分"请求抵达了目标"与"服务端把请求回吐给访客"——两者字节相同。
const httpProbeMarker = "\n-- target-marker --\n"

// startHTTPProbe 启动一个 HTTP 探针服务。
//
// 行为：读请求首部（直到空行），再按 Content-Length 读出正文，把首部与正文
// 一并回显，最后追加目标侧标记。按声明长度读正文是刻意的：若上游把正文丢了，
// 这里会一直等不到那部分字节，用例即以超时暴露问题。
func startHTTPProbe(t *testing.T) (netip.AddrPort, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动 HTTP 探针服务失败：%v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				reader := bufio.NewReader(c)
				var head bytes.Buffer
				contentLength := 0
				for {
					line, readErr := reader.ReadString('\n')
					head.WriteString(line)
					if name, value, ok := splitProbeHeader(line); ok &&
						strings.EqualFold(name, "Content-Length") {
						if parsed, convErr := strconv.Atoi(value); convErr == nil {
							contentLength = parsed
						}
					}
					if readErr != nil || line == "\r\n" || line == "\n" {
						break
					}
				}
				if _, err := c.Write(head.Bytes()); err != nil {
					return
				}
				if contentLength > 0 {
					body := make([]byte, contentLength)
					if _, err := io.ReadFull(reader, body); err != nil {
						return
					}
					if _, err := c.Write(body); err != nil {
						return
					}
				}
				_, _ = c.Write([]byte(httpProbeMarker))
			}(conn)
		}
	}()
	address := listener.Addr().(*net.TCPAddr)
	return mustAddrPort(t, "127.0.0.1:"+itoa(address.Port)), func() {
		_ = listener.Close()
		<-done
	}
}

// splitProbeHeader 拆分探针读到的首部行；空行与非法行返回假。
func splitProbeHeader(line string) (name, value string, ok bool) {
	trimmed := strings.TrimRight(line, "\r\n")
	if trimmed == "" {
		return "", "", false
	}
	separator := strings.Index(trimmed, ":")
	if separator < 0 {
		return "", "", false
	}
	return strings.TrimSpace(trimmed[:separator]), strings.TrimSpace(trimmed[separator+1:]), true
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
	// HTTP 绑定指向带标记的探针服务：回显服务与"把请求写回访客"产出的字节
	// 相同，无法证明请求真的抵达了目标。
	httpTarget, stopProbe := startHTTPProbe(t)
	defer stopProbe()

	controlListener, ports := controlAndFourPorts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	serverEngine := startFourProxyPair(t, ctx, tcpTarget, udpTarget, ports, controlListener, httpTarget)

	guest, err := net.Dial("tcp", serverEngine.GuestAddr("http-echo").String())
	if err != nil {
		t.Fatalf("访客连接 HTTP 入口失败：%v", err)
	}
	defer guest.Close()

	request := "GET /api/v1/users HTTP/1.1\r\nHost: app.example.com\r\n\r\n"
	if _, err := guest.Write([]byte(request)); err != nil {
		t.Fatalf("写入 HTTP 请求失败：%v", err)
	}
	// 断言必须能区分"请求抵达了目标"与"服务端把请求原样回吐给访客"：
	// 两者读到的字节完全相同，因此仅比对请求原文不足以证明前者。
	// 改用目标侧写入的标记作为证据——只有真正抵达目标的请求才会带回它。
	echoed := make([]byte, len(request))
	if _, err := readFullWithTimeout(guest, echoed); err != nil {
		t.Fatalf("HTTP 请求未抵达目标服务（路由未命中）：%v", err)
	}
	if string(echoed) != request {
		t.Fatalf("目标回显内容与请求不一致：%q", string(echoed))
	}
	// 目标服务在回显后追加的标记：它由目标侧产生，服务端无从伪造。
	marker := make([]byte, len(httpProbeMarker))
	if _, err := readFullWithTimeout(guest, marker); err != nil {
		t.Fatalf("未收到目标服务追加的标记，请求可能未真正抵达目标：%v", err)
	}
	if string(marker) != httpProbeMarker {
		t.Fatalf("目标标记不匹配：%q", string(marker))
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
		core.WithClientCredential(core.ClientCredential{ClientID: fr06aClientID, Token: server.DigestToken(fr06aClientToken)}),
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

// 未携带有效凭据的工作连接声明必须被拒绝，且不得影响该代理的暂存访客。
//
// 回归用例：工作连接是与控制连接平行的独立连接，服务端此前不做任何归属校验，
// 于是任何能连上控制端口的对端都能声明任意代理名。除了能与真实访客配对（读取其
// 数据），它还能借"越权目标"这一分支触发配对中心的副作用——释放该代理的全部
// 暂存访客，形成无需认证的拒绝服务。
//
// 本用例验证：伪造声明被拒绝、真实访客不受影响、合法客户端仍能正常配对。
func TestFR06aWorkConnRequiresCredentials(t *testing.T) {
	tcpTarget, stopEcho := startLocalEcho(t)
	defer stopEcho()
	udpTarget, stopUDP := startLocalUDPEcho(t)
	defer stopUDP()

	controlListener, ports := controlAndFourPorts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	control, err := netip.ParseAddrPort(controlListener.Addr().String())
	if err != nil {
		t.Fatalf("解析控制地址失败：%v", err)
	}

	// 只启动服务端：客户端稍后手动启动，以便在两者之间插入伪造声明。
	serverEngine := server.New(
		fr06aServerConfig(t, control, tcpTarget, udpTarget, ports),
		server.WithListener(controlListener),
	)
	if err := serverEngine.Start(ctx); err != nil {
		t.Fatalf("服务端启动失败：%v", err)
	}
	t.Cleanup(func() { _ = serverEngine.Shutdown(context.Background()) })

	// 先建立一条真实访客并让它暂存（此时还没有工作连接）。
	// 此刻不写入数据：暂存期间写入的字节会在配对后先于后续数据被转发回来，
	// 让"哪一段回显对应哪次写入"变得难以分辨。本用例只需访客连接存在。
	guest, err := net.Dial("tcp", serverEngine.GuestAddr("tcp-echo").String())
	if err != nil {
		t.Fatalf("访客连接失败：%v", err)
	}
	defer guest.Close()

	// 伪造一条工作连接声明：代理名正确，但没有凭据。
	forged, err := net.Dial("tcp", control.String())
	if err != nil {
		t.Fatalf("伪造对端连接失败：%v", err)
	}
	payload, err := json.Marshal(map[string]string{
		"proxy_name":  "tcp-echo",
		"target_addr": tcpTarget.String(),
	})
	if err != nil {
		t.Fatalf("序列化伪造声明失败：%v", err)
	}
	frame, err := wire.EncodeV1Frame(wire.Frame{Type: wire.MessageTypeNewWorkConn, Payload: payload})
	if err != nil {
		t.Fatalf("编码伪造帧失败：%v", err)
	}
	if _, err := forged.Write(frame); err != nil {
		t.Fatalf("发送伪造声明失败：%v", err)
	}
	// 服务端应关闭该连接。
	_ = forged.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := forged.Read(make([]byte, 1)); err == nil {
		t.Fatal("缺少凭据的工作连接声明应被拒绝并关闭")
	}
	_ = forged.Close()

	// 再伪造一条带**错误凭据**的声明：这一条专门验证凭据校验本身。
	// 若只测"无凭据"，代理归属校验会先拦住它，凭据校验被整体移除也测不出来。
	wrongCred, err := net.Dial("tcp", control.String())
	if err != nil {
		t.Fatalf("错误凭据对端连接失败：%v", err)
	}
	wrongPayload, err := json.Marshal(map[string]string{
		"client_id":   fr06aClientID,
		"token":       "伪造的令牌",
		"proxy_name":  "tcp-echo",
		"target_addr": tcpTarget.String(),
	})
	if err != nil {
		t.Fatalf("序列化错误凭据声明失败：%v", err)
	}
	wrongFrame, err := wire.EncodeV1Frame(wire.Frame{Type: wire.MessageTypeNewWorkConn, Payload: wrongPayload})
	if err != nil {
		t.Fatalf("编码错误凭据帧失败：%v", err)
	}
	if _, err := wrongCred.Write(wrongFrame); err != nil {
		t.Fatalf("发送错误凭据声明失败：%v", err)
	}
	_ = wrongCred.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := wrongCred.Read(make([]byte, 1)); err == nil {
		t.Fatal("令牌不匹配的工作连接声明应被拒绝并关闭")
	}
	_ = wrongCred.Close()

	// 第三条：凭据合法但代理名不属于该客户端（此处用未注册的代理名）。
	// 它验证归属校验，且同样不得触碰配对中心。
	wrongProxy, err := net.Dial("tcp", control.String())
	if err != nil {
		t.Fatalf("越权代理对端连接失败：%v", err)
	}
	wrongProxyPayload, err := json.Marshal(map[string]string{
		"client_id":   fr06aClientID,
		"token":       fr06aClientToken,
		"proxy_name":  "不属于该客户端的代理",
		"target_addr": tcpTarget.String(),
	})
	if err != nil {
		t.Fatalf("序列化越权代理声明失败：%v", err)
	}
	wrongProxyFrame, err := wire.EncodeV1Frame(wire.Frame{Type: wire.MessageTypeNewWorkConn, Payload: wrongProxyPayload})
	if err != nil {
		t.Fatalf("编码越权代理帧失败：%v", err)
	}
	if _, err := wrongProxy.Write(wrongProxyFrame); err != nil {
		t.Fatalf("发送越权代理声明失败：%v", err)
	}
	_ = wrongProxy.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := wrongProxy.Read(make([]byte, 1)); err == nil {
		t.Fatal("代理归属不匹配的工作连接声明应被拒绝并关闭")
	}
	_ = wrongProxy.Close()

	// 暂存访客不得被伪造声明影响：它的连接仍应是打开的。
	_ = guest.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := guest.Read(make([]byte, 1)); err == nil {
		t.Fatal("暂存访客不应收到数据")
	} else if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		t.Fatalf("暂存访客被伪造声明影响（连接被关闭）：%v", err)
	}

	// 合法客户端仍应正常配对：访客写入的数据最终抵达回显目标并原路返回。
	clientEngine := client.New(fr06aClientConfig(t, control, tcpTarget, udpTarget, ports))
	if err := clientEngine.Start(ctx); err != nil {
		t.Fatalf("客户端启动失败：%v", err)
	}
	t.Cleanup(func() { _ = clientEngine.Shutdown(context.Background()) })

	payload2 := []byte("合法配对验证")
	if _, err := guest.Write(payload2); err != nil {
		t.Fatalf("访客二次写入失败：%v", err)
	}
	echoed := make([]byte, len(payload2))
	if _, err := readFullWithTimeout(guest, echoed); err != nil {
		t.Fatalf("合法客户端接入后访客数据未往返：%v", err)
	}
	if string(echoed) != string(payload2) {
		t.Fatalf("往返内容不一致：%q", string(echoed))
	}
}

// 打开入口失败时必须释放已成功打开的入口，不留半注册监听器。
//
// 回归用例：失败分支写的是 `return nil, nil, nil, err`，把命名返回值置空，
// 于是 defer 里的释放函数拿到空 map，已打开的监听器不释放。反复 Start 失败
// 会持续累积监听器，其间这些端口不可被其他进程使用，且释放时机取决于 GC。
//
// 判定方式：让第二个入口端口被外部占用以触发失败，随后验证第一个入口端口
// 已可重新绑定——能绑上即说明它被真正释放。
func TestFR06aStartFailureReleasesOpenedEntries(t *testing.T) {
	tcpTarget, stopEcho := startLocalEcho(t)
	defer stopEcho()
	udpTarget, stopUDP := startLocalUDPEcho(t)
	defer stopUDP()

	controlListener, ports := controlAndFourPorts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	control, err := netip.ParseAddrPort(controlListener.Addr().String())
	if err != nil {
		t.Fatalf("解析控制地址失败：%v", err)
	}

	// 外部占住第三个入口端口（HTTP 入口，TCP 监听），使服务端打开它时失败。
	// 刻意不占第二个（UDP）：UDP 入口用 UDP 绑定，占用 TCP 端口不会让它失败。
	blocker, err := net.Listen("tcp", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(ports[2])).String())
	if err != nil {
		t.Fatalf("占用第三个入口端口失败：%v", err)
	}
	defer blocker.Close()

	serverEngine := server.New(
		fr06aServerConfig(t, control, tcpTarget, udpTarget, ports),
		server.WithListener(controlListener),
	)
	if startErr := serverEngine.Start(ctx); startErr == nil {
		_ = serverEngine.Shutdown(context.Background())
		t.Fatal("入口端口被占用时 Start 应失败")
	}

	// 第一个入口端口在失败前已成功打开，失败后必须已被释放。
	firstAddr := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(ports[0])).String()
	reclaim, err := net.Listen("tcp", firstAddr)
	if err != nil {
		t.Fatalf("已打开的入口未在失败路径释放，端口 %s 仍被占用：%v", firstAddr, err)
	}
	_ = reclaim.Close()
}

// Host 首部携带端口时路由仍必须命中。
//
// 回归用例：HTTP/1.1 客户端在非默认端口上会把 Host 写成 `host:port`（标准行为），
// 而路由表里配置的是裸主机名。此前直接取 Host 原值做等值比较，于是入口端口不是
// 80 时**几乎所有真实客户端都被判为未匹配**，HTTP 代理在真实场景下不可用。
func TestFR06aHTTPRoutesWithPortInHostHeader(t *testing.T) {
	tcpTarget, stopEcho := startLocalEcho(t)
	defer stopEcho()
	udpTarget, stopUDP := startLocalUDPEcho(t)
	defer stopUDP()
	httpTarget, stopProbe := startHTTPProbe(t)
	defer stopProbe()

	controlListener, ports := controlAndFourPorts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	serverEngine := startFourProxyPair(t, ctx, tcpTarget, udpTarget, ports, controlListener, httpTarget)

	guest, err := net.Dial("tcp", serverEngine.GuestAddr("http-echo").String())
	if err != nil {
		t.Fatalf("访客连接 HTTP 入口失败：%v", err)
	}
	defer guest.Close()

	// Host 带端口：这是真实客户端在非 80 端口上的标准写法。
	entryAddr := mustAddrPort(t, serverEngine.GuestAddr("http-echo").String())
	request := "GET /api/v1/users HTTP/1.1\r\nHost: " + fr06aHTTPHost +
		":" + itoa(int(entryAddr.Port())) + "\r\n\r\n"
	if _, err := guest.Write([]byte(request)); err != nil {
		t.Fatalf("写入 HTTP 请求失败：%v", err)
	}
	echoed := make([]byte, len(request))
	if _, err := readFullWithTimeout(guest, echoed); err != nil {
		t.Fatalf("Host 带端口时路由未命中：%v", err)
	}
	marker := make([]byte, len(httpProbeMarker))
	if _, err := readFullWithTimeout(guest, marker); err != nil {
		t.Fatalf("未收到目标标记，请求未抵达目标：%v", err)
	}
	if string(marker) != httpProbeMarker {
		t.Fatalf("目标标记不匹配：%q", string(marker))
	}
}

// 带正文的请求必须完整送达目标，正文不得被丢弃。
//
// 回归用例：首部解析用 bufio 预读，被预读进缓冲的正文既不在回放字节里、也无法
// 再被后续读取，于是目标收到 Content-Length 却拿不到数据（挂起或 400）。
func TestFR06aHTTPPreservesRequestBody(t *testing.T) {
	tcpTarget, stopEcho := startLocalEcho(t)
	defer stopEcho()
	udpTarget, stopUDP := startLocalUDPEcho(t)
	defer stopUDP()
	httpTarget, stopProbe := startHTTPProbe(t)
	defer stopProbe()

	controlListener, ports := controlAndFourPorts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	serverEngine := startFourProxyPair(t, ctx, tcpTarget, udpTarget, ports, controlListener, httpTarget)

	guest, err := net.Dial("tcp", serverEngine.GuestAddr("http-echo").String())
	if err != nil {
		t.Fatalf("访客连接 HTTP 入口失败：%v", err)
	}
	defer guest.Close()

	body := "HELLO-BODY-PAYLOAD"
	request := "POST /api HTTP/1.1\r\nHost: " + fr06aHTTPHost +
		"\r\nContent-Length: " + itoa(len(body)) + "\r\n\r\n" + body
	if _, err := guest.Write([]byte(request)); err != nil {
		t.Fatalf("写入 HTTP 请求失败：%v", err)
	}
	// 探针按 Content-Length 读回正文并与首部一起回显，因此读满整个请求长度
	// 即证明正文完整抵达了目标；若正文被丢弃，探针会一直等不到那部分字节。
	echoed := make([]byte, len(request))
	if _, err := readFullWithTimeout(guest, echoed); err != nil {
		t.Fatalf("带正文的请求未完整抵达目标：%v", err)
	}
	if string(echoed) != request {
		t.Fatalf("目标回显与请求不一致：%q", string(echoed))
	}
}

// 默认配置下同一 UDP 代理必须能服务多个对端。
//
// 回归用例：UDP 按对端地址会话化，每条会话独占一条工作连接并在整个会话生命周期
// 内持有。工作连接池的默认上限此前是 1，而 UDP 会话上限是 8——于是第 2 个对端起
// 永远取不到工作连接，表现为"多用户共享一个 UDP 代理时随机只有一个可用"，
// 与规格 §3.4 的多对端识别能力直接冲突。
func TestFR06aUDPProxyServesMultiplePeersByDefault(t *testing.T) {
	tcpTarget, stopEcho := startLocalEcho(t)
	defer stopEcho()
	udpTarget, stopUDP := startLocalUDPEcho(t)
	defer stopUDP()

	controlListener, ports := controlAndFourPorts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	serverEngine := startFourProxyPair(t, ctx, tcpTarget, udpTarget, ports, controlListener)
	guestAddr := serverEngine.GuestAddr("udp-echo").String()

	// 三个不同的源端口 = 三个不同对端，各自应能建立会话并完成往返。
	for index := 0; index < 3; index += 1 {
		socket, err := net.Dial("udp", guestAddr)
		if err != nil {
			t.Fatalf("第 %d 个对端拨号失败：%v", index+1, err)
		}
		defer socket.Close()

		payload := []byte("对端 " + itoa(index+1) + " 的数据报")
		// 首个数据报用于建立会话并等待工作连接就绪，因此允许有限次重发：
		// 重发是 UDP 的正常语义，重试次数有界，不是等待无限期。
		// 用既有辅助而非手写读写：它只对读设截止时间，不会把写也置于超时之下。
		var echoed []byte
		var lastErr error
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			echoed, lastErr = roundTripUDP(socket, payload, 2*time.Second)
			if lastErr == nil {
				break
			}
		}
		if lastErr != nil {
			t.Fatalf("第 %d 个对端往返失败（默认配置应支持多对端）：%v", index+1, lastErr)
		}
		if string(echoed) != string(payload) {
			t.Fatalf("第 %d 个对端往返内容不一致：%q", index+1, string(echoed))
		}
	}
}
