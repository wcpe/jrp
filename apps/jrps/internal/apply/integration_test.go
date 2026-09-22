package apply

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/wcpe/jrp/apps/jrps/internal/store"
	"github.com/wcpe/jrp/core"
	"github.com/wcpe/jrp/core/client"
	"github.com/wcpe/jrp/core/server"
)

// 集成测试专用凭证 Provider：真实引擎需要与客户端配置一致的明文凭证。
type fixedCredentials struct {
	clientID string
	token    string
}

func (provider *fixedCredentials) DataPlaneCredentials(_ context.Context) ([]core.ClientCredential, error) {
	return []core.ClientCredential{{ClientID: provider.clientID, Token: provider.token}}, nil
}

// startLocalEchoServer 启动本地回显服务，模拟被代理目标。
func startLocalEchoServer(t *testing.T) (netip.AddrPort, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动回显服务失败：%v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(connection net.Conn) {
				defer connection.Close()
				_, _ = io.Copy(connection, connection)
			}(conn)
		}
	}()
	address := listener.Addr().(*net.TCPAddr)
	return netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(address.Port)), func() {
		_ = listener.Close()
		<-done
	}
}

// integrationEnv 组装真实引擎与编排服务的完整环境。
type integrationEnv struct {
	service     *Service
	database    *store.Store
	engine      *server.Engine
	clientID    string
	token       string
	controlPort int
}

// newIntegrationEnv 启动真实服务端引擎 + 编排服务 + store，并完成启动恢复。
//
// 引擎按 Core 的约定由宿主注入控制监听器；恢复以 desired 为输入走完整四阶段。
func newIntegrationEnv(t *testing.T) *integrationEnv {
	t.Helper()

	database, err := store.Open(store.Config{
		Path:   filepath.Join(t.TempDir(), "jrps.db"),
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("打开数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	clientID := "int-client"
	token := "int-token"
	credentials := &fixedCredentials{clientID: clientID, token: token}

	// 控制监听端口与访客端口从固定测试区间取号：申请后立即释放存在撞车窗口。
	controlListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听控制端口失败：%v", err)
	}
	controlPort := controlListener.Addr().(*net.TCPAddr).Port

	// 无代理的初始配置只用于构造引擎（Start 需要合法配置）；真实配置经
	// 恢复流程以 desired 内容进入。
	initialConfig, err := core.NewServerConfig(
		core.WithListen(core.BindEndpoint{
			Address:   netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(controlPort)),
			Transport: core.TransportTCP,
		}),
		core.WithWire(core.WireV1),
		core.WithClientCredential(core.ClientCredential{ClientID: clientID, Token: token}),
	)
	if err != nil {
		t.Fatalf("构造初始配置失败：%v", err)
	}
	engine := server.New(initialConfig, server.WithListener(controlListener))
	if err := engine.Start(context.Background()); err != nil {
		t.Fatalf("引擎启动失败：%v", err)
	}
	t.Cleanup(func() {
		_ = engine.Shutdown(context.Background())
	})

	service := New(database, credentials, engine, slog.New(slog.DiscardHandler))
	return &integrationEnv{
		service:     service,
		database:    database,
		engine:      engine,
		clientID:    clientID,
		token:       token,
		controlPort: controlPort,
	}
}

// writeProxy 写入一个代理定义并追加 desired 版本。
func (env *integrationEnv) writeProxy(t *testing.T, proxy store.Proxy) uint64 {
	t.Helper()
	var revision uint64
	err := env.database.Transaction(context.Background(), func(tx *store.Tx) error {
		var err error
		revision, err = tx.SaveProxy(proxy, store.ActorAdmin("admin"), store.OriginProxyCreate)
		return err
	})
	if err != nil {
		t.Fatalf("写入代理失败：%v", err)
	}
	return revision
}

// buildClientConfig 按当前引擎实际控制地址构造客户端配置。
func (env *integrationEnv) buildClientConfig(t *testing.T, target netip.AddrPort, guestPort int) core.ClientConfig {
	t.Helper()
	config, err := core.NewClientConfig(
		core.WithClientID(env.clientID),
		core.WithServerEndpoint(core.ServerEndpoint{
			Address:   netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(env.controlPort)),
			Transport: core.TransportTCP,
			Wire:      core.WireV1,
		}),
		core.WithClientAuth(core.TokenAuth{Token: env.token}),
		core.WithTCPProxy(core.TCPProxy{Name: "svc-echo", LocalAddr: target, RemotePort: guestPort}),
		core.WithHeartbeat(200*time.Millisecond),
		core.WithTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("构造客户端配置失败：%v", err)
	}
	return config
}

// 集成：真实引擎上应用 desired 建立代理，客户端登录注册后访客流量可达；
// 再次应用新 revision，已建立的访客长连接不中断、数据继续往返（规格 §5）。
func TestIntegrationApplyKeepsLongConnectionAlive(t *testing.T) {
	target, stopEcho := startLocalEchoServer(t)
	defer stopEcho()

	env := newIntegrationEnv(t)

	// 写入代理并应用：这是 desired → 快照 → 四阶段的完整外壳路径。
	guestPort := reserveTestPort(t)
	revision := env.writeProxy(t, store.Proxy{
		ID: "p-echo", ClientID: env.clientID, Name: "svc-echo", Type: "tcp",
		RemotePort: guestPort, Target: target.String(),
	})
	if err := env.service.ApplyDesiredAllowCurrent(context.Background(), revision, store.ActorAdmin("server"), "boot"); err != nil {
		t.Fatalf("首次应用失败：%v", err)
	}

	// 客户端登录并注册代理，等待代理就绪事件。
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	startIntegrationClient(t, ctx, env, target, guestPort)

	// 访客建立长连接并完成一次往返。
	guestAddress := env.engine.GuestAddr("svc-echo")
	guest, err := net.Dial("tcp", guestAddress.String())
	if err != nil {
		t.Fatalf("访客连接失败：%v", err)
	}
	defer guest.Close()
	if err := echoOnce(guest, []byte("切换前数据")); err != nil {
		t.Fatalf("切换前往返失败：%v", err)
	}

	// 应用新 revision（内容等价、仅版本推进）：入口未变化时 Core 复用监听器，
	// 已建立的长连接必须存活。
	//
	// Apply 同步调用会停在 drain 等待旧流结束，因此放后台执行：本用例要在
	// drain 窗口内验证旧流连续（与 FR-26 场景测试同一模式）。若在 Apply 返回
	// 之后才回显，旧流会被 drain 上限的强制释放关掉——那是排水语义，不是断流。
	newRevision := env.writeProxy(t, store.Proxy{
		ID: "p-echo", ClientID: env.clientID, Name: "svc-echo", Type: "tcp",
		RemotePort: guestPort, Target: target.String(),
	})
	applyDone := make(chan error, 1)
	go func() {
		applyDone <- env.service.ApplyDesiredAllowCurrent(context.Background(), newRevision, store.ActorAdmin("server"), "switch")
	}()

	// drain 窗口内旧流持续可用（规格 §5：drain 期间已有长连接不中断）。
	if err := echoOnce(guest, []byte("切换中数据")); err != nil {
		t.Fatalf("切换中长连接被切断：%v", err)
	}
	if err := echoOnce(guest, []byte("切换后数据")); err != nil {
		t.Fatalf("切换后长连接被切断：%v", err)
	}

	// 关闭旧流放行 drain，等待应用收敛。
	_ = guest.Close()
	select {
	case err := <-applyDone:
		if err != nil {
			t.Fatalf("二次应用失败：%v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("二次应用未收敛（drain 挂死）")
	}

	// 入口交接后新访客仍可连同一个地址。
	newGuest, err := net.Dial("tcp", guestAddress.String())
	if err != nil {
		t.Fatalf("切换后新访客连接失败：%v", err)
	}
	defer newGuest.Close()
	if err := echoOnce(newGuest, []byte("换代后新流")); err != nil {
		t.Fatalf("切换后新访客回显失败：%v", err)
	}

	// active 推进到新 revision。
	var state store.RevisionState
	if err := env.database.View(context.Background(), func(tx *store.Tx) error {
		var err error
		state, err = tx.RevisionState(store.ScopeServer)
		return err
	}); err != nil {
		t.Fatalf("读取状态失败：%v", err)
	}
	if state.ActiveRevision != newRevision {
		t.Fatalf("active 应为 %d，实际 %d", newRevision, state.ActiveRevision)
	}
}

// 集成：prepare 失败（代理入口端口被占）时 active 与 last-good 保持不变，
// 已建立的连接不受影响（规格 §5）。
func TestIntegrationPrepareFailureKeepsState(t *testing.T) {
	target, stopEcho := startLocalEchoServer(t)
	defer stopEcho()

	env := newIntegrationEnv(t)

	guestPort := reserveTestPort(t)
	firstRevision := env.writeProxy(t, store.Proxy{
		ID: "p-ok", ClientID: env.clientID, Name: "svc-echo", Type: "tcp",
		RemotePort: guestPort, Target: target.String(),
	})
	if err := env.service.ApplyDesiredAllowCurrent(context.Background(), firstRevision, store.ActorAdmin("server"), "boot"); err != nil {
		t.Fatalf("首次应用失败：%v", err)
	}

	// 客户端上线并注册同一代理，保证旧代有活动会话。
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	startIntegrationClient(t, ctx, env, target, guestPort)

	// 追加一个新代理：用与既有代理相同的入口端口但不同的代理名构造冲突。
	// 复用按「名字 + 绑定地址」判定，不同名必然重绑已被旧代占用的端口，
	// 触发 prepare 失败——比独立 listener 占端口更确定（Windows 下通配
	// 地址绑定可与既有监听共存，独立 listener 不可靠）。
	conflictRevision := env.writeProxy(t, store.Proxy{
		ID: "p-bad", ClientID: env.clientID, Name: "svc-conflict", Type: "tcp",
		RemotePort: guestPort, Target: target.String(),
	})
	err := env.service.ApplyDesiredAllowCurrent(context.Background(), conflictRevision, store.ActorAdmin("server"), "conflict")
	if err == nil {
		t.Fatalf("端口冲突应用应失败")
	}

	// active 与 last-good 落库记录保持在首次成功应用的版本。
	var state store.RevisionState
	if err := env.database.View(context.Background(), func(tx *store.Tx) error {
		var err error
		state, err = tx.RevisionState(store.ScopeServer)
		return err
	}); err != nil {
		t.Fatalf("读取状态失败：%v", err)
	}
	if state.ActiveRevision != firstRevision || state.LastGoodRevision != firstRevision {
		t.Fatalf("失败后 active/last-good 应保持 %d，实际 active=%d lastGood=%d",
			firstRevision, state.ActiveRevision, state.LastGoodRevision)
	}

	// 原有代理入口仍然可用：冲突应用的失败不影响既有服务。
	guest, dialErr := net.Dial("tcp", env.engine.GuestAddr("svc-echo").String())
	if dialErr != nil {
		t.Fatalf("失败后原有入口应可连接：%v", dialErr)
	}
	defer guest.Close()
	if err := echoOnce(guest, []byte("失败后数据")); err != nil {
		t.Fatalf("失败后原有服务应正常：%v", err)
	}
}

// reserveTestPort 从内核申请一个可用端口并立即释放（集成测试端口少、窗口小）。
func reserveTestPort(t *testing.T) int {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("申请端口失败：%v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	return port
}

// startIntegrationClient 启动真实客户端引擎并等待它登录注册。
//
// 必须先建立服务端事件订阅再启动客户端：客户端 Start 返回时登录已完成，
// 登录事件在订阅建立之前发布的话会永久丢失。
func startIntegrationClient(t *testing.T, ctx context.Context, env *integrationEnv, target netip.AddrPort, guestPort int) {
	t.Helper()
	subscription := env.engine.Subscribe(core.Options{Capacity: 16})
	defer subscription.Close()

	config := env.buildClientConfig(t, target, guestPort)
	engine := client.New(config)
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("客户端启动失败：%v", err)
	}
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })

	waitClientConnected(t, subscription, env.clientID)
}

// waitClientConnected 从既有订阅等待指定客户端的登录事件。
func waitClientConnected(t *testing.T, subscription *core.Subscription, clientID string) {
	deadline := time.After(10 * time.Second)
	for {
		select {
		case event := <-subscription.Events():
			connected, ok := event.(core.ClientConnected)
			if ok && connected.ClientID == clientID {
				return
			}
		case <-deadline:
			t.Fatalf("等待客户端 %s 登录超时", clientID)
		}
	}
}

// echoOnce 在连接上发送载荷并读回，断言逐字节一致。
//
// 读侧设 2 秒截止时间（与 Core 测试一致）；写侧不设，Windows 上对已断开连接
// 的写会直接报错，足以暴露断流。
func echoOnce(connection net.Conn, payload []byte) error {
	if _, err := connection.Write(payload); err != nil {
		return err
	}
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer func() { _ = connection.SetReadDeadline(time.Time{}) }()
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(connection, received); err != nil {
		return err
	}
	for index := range payload {
		if received[index] != payload[index] {
			return io.ErrUnexpectedEOF
		}
	}
	return nil
}
