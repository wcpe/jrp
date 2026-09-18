// 外部消费验证脚本：在临时目录建立独立 Go module，仅依赖 Core 公共包，
// 完成 ClientEngine ↔ ServerEngine 的 TCP + wire v1 + TCP 代理数据闭环。
//
// 运行方式：node scripts/verify-external-consume.mjs
// 要求：Go 工具链可用；Core 已有本地实现（replace 指向仓库内 core）。
import { mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { captureCommand, runCommand } from './command.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const coreDirectory = path.join(root, 'core');

const goMod = `module externalconsume

go 1.25.0

require github.com/wcpe/jrp/core v0.0.0

replace github.com/wcpe/jrp/core => %CORE%
`.replace('%CORE%', coreDirectory.replace(/\\/gu, '/'));

// 验证程序只导入 Core 的三个公共包，不触碰 internal。
const mainGo = `package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"time"

	"github.com/wcpe/jrp/core"
	"github.com/wcpe/jrp/core/client"
	"github.com/wcpe/jrp/core/server"
)

func mustAddrPort(value string) netip.AddrPort {
	address, err := netip.ParseAddrPort(value)
	if err != nil {
		fmt.Println("解析地址失败：", err)
		os.Exit(1)
	}
	return address
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	echoListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("回显服务启动失败：", err)
		os.Exit(1)
	}
	defer echoListener.Close()
	go func() {
		for {
			conn, err := echoListener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	target := mustAddrPort(echoListener.Addr().String())

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("申请访客端口失败：", err)
		os.Exit(1)
	}
	guestPort := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	controlListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("控制监听失败：", err)
		os.Exit(1)
	}
	control := mustAddrPort(controlListener.Addr().String())

	serverConfig, err := core.NewServerConfig(
		core.WithListen(core.BindEndpoint{Address: control, Transport: core.TransportTCP}),
		core.WithWire(core.WireV1),
		core.WithClientCredential(core.ClientCredential{ClientID: "ext-client", Token: "ext-token"}),
		core.WithTCPProxyBinding(core.TCPProxyBinding{Name: "ext-ssh", ClientID: "ext-client", RemotePort: guestPort}),
	)
	if err != nil {
		fmt.Println("服务端配置失败：", err)
		os.Exit(1)
	}
	serverEngine := server.New(serverConfig, server.WithListener(controlListener))
	if err := serverEngine.Start(ctx); err != nil {
		fmt.Println("服务端启动失败：", err)
		os.Exit(1)
	}
	defer serverEngine.Shutdown(context.Background())

	clientConfig, err := core.NewClientConfig(
		core.WithClientID("ext-client"),
		core.WithServerEndpoint(core.ServerEndpoint{Address: control, Transport: core.TransportTCP, Wire: core.WireV1}),
		core.WithClientAuth(core.TokenAuth{Token: "ext-token"}),
		core.WithTCPProxy(core.TCPProxy{Name: "ext-ssh", LocalAddr: target, RemotePort: guestPort}),
	)
	if err != nil {
		fmt.Println("客户端配置失败：", err)
		os.Exit(1)
	}
	clientEngine := client.New(clientConfig)
	if err := clientEngine.Start(ctx); err != nil {
		fmt.Println("客户端启动失败：", err)
		os.Exit(1)
	}
	defer clientEngine.Shutdown(context.Background())

	guestAddr := serverEngine.GuestAddr("ext-ssh")
	guest, err := net.Dial("tcp", guestAddr.String())
	if err != nil {
		fmt.Println("访客连接失败：", err)
		os.Exit(1)
	}
	defer guest.Close()
	payload := []byte("外部消费验证数据-0123456789")
	if _, err := guest.Write(payload); err != nil {
		fmt.Println("访客写入失败：", err)
		os.Exit(1)
	}
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(guest, received); err != nil {
		fmt.Println("访客读回失败：", err)
		os.Exit(1)
	}
	if string(payload) != string(received) {
		fmt.Println("数据不一致")
		os.Exit(1)
	}
	fmt.Println("外部消费验证通过：端到端数据逐字节一致")
}
`;

const workdir = mkdtempSync(path.join(tmpdir(), 'jrp-external-consume-'));
try {
  mkdirSync(workdir, { recursive: true });
  writeFileSync(path.join(workdir, 'go.mod'), goMod);
  writeFileSync(path.join(workdir, 'main.go'), mainGo);
  const env = { ...process.env, GOWORK: 'off', GOFLAGS: '-mod=mod' };
  runCommand('go', ['mod', 'tidy'], { cwd: workdir, env });
  // 校验只导入公共包：internal 路径不得出现在导入表中。
  const imports = captureCommand('go', ['list', '-f', '{{join .Imports "\\n"}}'], {
    cwd: workdir,
    env,
  });
  const bad = imports
    .split(/\r?\n/u)
    .map((line) => line.trim())
    .filter((line) => line.includes('github.com/wcpe/jrp/core/internal'));
  if (bad.length > 0) {
    process.stderr.write(`外部消费验证失败：引用了 Core 内部包：\n- ${bad.join('\n- ')}\n`);
    process.exit(1);
  }
  runCommand('go', ['run', '.'], { cwd: workdir, env, timeout: 90000 });
  process.stdout.write('外部消费验证通过。\n');
} finally {
  rmSync(workdir, { recursive: true, force: true });
}
