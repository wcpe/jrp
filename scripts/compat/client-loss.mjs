// 客户端离线观测：测量官方客户端被强杀后，服务端代理入口停止接收新流量所需的时长。
//
// 对应验收条款（FR-03 §5 正常路径第 3 条、FR-06a §5 错误路径「客户端离线时代理入口
// 停止接收新流量」）：客户端离线后入口必须停止接收，而"多快停止"依传输而异——
// 流传输能立刻感知连接断开，UDP 载体（QUIC）只能等会话空闲回收。本脚本给出可复核
// 的数字，避免用"看起来很快"替代实测。
//
// 运行方式（需先有一个按目标传输运行的 jrps）：
//   node scripts/compat/client-loss.mjs --server=127.0.0.1:7200 --transport=tcp \
//     --client-id=compat-frpc --token=<token> --entry-port=6150
import { spawn } from 'node:child_process';
import { mkdirSync, rmSync, writeFileSync } from 'node:fs';
import net from 'node:net';
import path from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { fileURLToPath } from 'node:url';

import { ensureFrpcBinary } from './frpc.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');

// parseArguments 解析命令行参数。
function parseArguments(argv) {
  const options = {
    server: '',
    transport: 'tcp',
    wire: 'v1',
    clientId: '',
    token: '',
    entryPort: 0,
    timeoutSeconds: 90,
    frpc: process.env.JRP_COMPAT_FRPC ?? '',
  };
  for (const argument of argv) {
    const [name, value = ''] = argument.split('=');
    switch (name) {
      case '--server':
        options.server = value;
        break;
      case '--transport':
        options.transport = value;
        break;
      case '--wire':
        options.wire = value;
        break;
      case '--client-id':
        options.clientId = value;
        break;
      case '--token':
        options.token = value;
        break;
      case '--entry-port':
        options.entryPort = Number(value);
        break;
      case '--timeout':
        options.timeoutSeconds = Number(value);
        break;
      case '--frpc':
        options.frpc = value;
        break;
      default:
        throw new Error(`未知参数：${argument}`);
    }
  }
  if (!options.server.includes(':')) {
    throw new Error('必须提供 --server=<主机:端口>');
  }
  if (!options.token) {
    throw new Error('必须提供 --token=<数据面 token>');
  }
  if (!Number.isInteger(options.entryPort) || options.entryPort <= 0) {
    throw new Error('必须提供 --entry-port=<服务端代理入口端口>');
  }
  const [host, portText] = options.server.split(':');
  options.host = host;
  options.port = Number(portText);
  return options;
}

// renderFrpcConfig 生成官方 frpc 配置。
function renderFrpcConfig(options, caseDirectory) {
  const transport = options.transport;
  const secure = transport === 'wss' || transport === 'quic';
  return [
    `serverAddr = "${options.host}"`,
    `serverPort = ${options.port}`,
    ...(options.clientId ? [`clientID = "${options.clientId}"`] : []),
    'auth.method = "token"',
    `auth.token = "${options.token}"`,
    `transport.protocol = "${transport}"`,
    `transport.wireProtocol = "${options.wire}"`,
    `transport.tls.enable = ${secure}`,
    'transport.tcpMux = false',
    ...(transport === 'quic'
      ? ['transport.quic.keepalivePeriod = 10', 'transport.quic.maxIdleTimeout = 30']
      : []),
    'log.level = "debug"',
    `log.to = "${path.join(caseDirectory, 'frpc.log').replace(/\\/gu, '/')}"`,
    '',
    '[[proxies]]',
    'name = "client-loss-probe"',
    `type = "tcp"`,
    'localIP = "127.0.0.1"',
    `localPort = ${options.localPort}`,
    `remotePort = ${options.entryPort}`,
    '',
  ].join('\n');
}

// startEchoService 启动本地回显服务作为被代理目标。
function startEchoService() {
  const service = net.createServer((socket) => {
    socket.on('error', () => socket.destroy());
    socket.pipe(socket);
  });
  service.on('error', () => {});
  return new Promise((resolve) => {
    service.listen(0, '127.0.0.1', () => resolve(service));
  });
}

// closeServer 等待监听器关闭。
function closeServer(server) {
  return new Promise((resolve) => {
    if (!server || !server.listening) {
      resolve();
      return;
    }
    server.close(() => resolve());
  });
}

// entryAccepts 探测入口端口是否仍在接受连接。
function entryAccepts(host, port, timeoutMilliseconds = 1000) {
  return new Promise((resolve) => {
    const socket = net.connect({ host, port });
    const finish = (value) => {
      socket.destroy();
      resolve(value);
    };
    socket.setTimeout(timeoutMilliseconds);
    socket.once('connect', () => finish(true));
    socket.once('timeout', () => finish(false));
    socket.once('error', () => finish(false));
  });
}

// startFrpc 启动官方客户端。
function startFrpc(executable, configPath) {
  const child = spawn(executable, ['-c', configPath], { stdio: ['ignore', 'pipe', 'pipe'] });
  const state = { exited: false, exitCode: null, stdout: '', stderr: '' };
  child.stdout.on('data', (chunk) => {
    state.stdout += chunk.toString();
  });
  child.stderr.on('data', (chunk) => {
    state.stderr += chunk.toString();
  });
  child.once('error', (error) => {
    state.exited = true;
    state.stderr += `\n进程启动失败：${error.message}\n`;
  });
  child.once('exit', (code) => {
    state.exited = true;
    state.exitCode = code;
  });
  return { child, state };
}

// waitForLogMarker 等待客户端日志出现标记。
async function waitForLogMarker(logPath, marker, timeoutMilliseconds) {
  const { readFileSync } = await import('node:fs');
  const deadline = Date.now() + timeoutMilliseconds;
  while (Date.now() < deadline) {
    try {
      if (readFileSync(logPath, 'utf8').includes(marker)) {
        return true;
      }
    } catch {
      // 日志尚未创建：继续等待。
    }
    await delay(200);
  }
  return false;
}

// main 执行一次测量并输出结论。
async function main() {
  const options = parseArguments(process.argv.slice(2));
  const dateStamp = new Date().toISOString().slice(0, 10);
  const caseDirectory = path.join(
    root,
    '.tmp',
    'compat-real',
    dateStamp,
    `client-loss-${options.transport}`,
  );
  mkdirSync(caseDirectory, { recursive: true });
  const logPath = path.join(caseDirectory, 'frpc.log');
  rmSync(logPath, { force: true });

  const binary = options.frpc
    ? { version: `override:${options.frpc}`, executable: options.frpc }
    : await ensureFrpcBinary();
  const echoService = await startEchoService();
  options.localPort = echoService.address().port;
  writeFileSync(path.join(caseDirectory, 'frpc.toml'), renderFrpcConfig(options, caseDirectory));

  const result = {
    transport: options.transport,
    wire: options.wire,
    server: options.server,
    entryPort: options.entryPort,
    startedAt: new Date().toISOString(),
    status: 'failed',
  };
  let frpc;
  try {
    frpc = startFrpc(binary.executable, path.join(caseDirectory, 'frpc.toml'));
    if (!(await waitForLogMarker(logPath, 'start proxy success', 25000))) {
      throw new Error('客户端未在期限内完成代理注册');
    }
    result.entryAcceptsBefore = await entryAccepts(options.host, options.entryPort);
    if (!result.entryAcceptsBefore) {
      throw new Error('注册成功后入口仍不可接受连接');
    }

    // 强杀客户端：不给它发送关闭帧的机会，模拟断电/进程崩溃。
    const killedAt = Date.now();
    frpc.child.kill('SIGKILL');
    const deadline = killedAt + options.timeoutSeconds * 1000;
    let stoppedAt = 0;
    while (Date.now() < deadline) {
      if (!(await entryAccepts(options.host, options.entryPort))) {
        stoppedAt = Date.now();
        break;
      }
      await delay(500);
    }
    result.entryStoppedAfterMilliseconds = stoppedAt === 0 ? null : stoppedAt - killedAt;
    result.status = stoppedAt === 0 ? 'timeout' : 'passed';
    if (stoppedAt === 0) {
      result.error = `入口在 ${options.timeoutSeconds} 秒内仍在接受连接`;
    }
    return result;
  } catch (error) {
    result.error = error.message;
    return result;
  } finally {
    if (frpc && !frpc.state.exited) {
      frpc.child.kill('SIGKILL');
    }
    await closeServer(echoService);
    writeFileSync(path.join(caseDirectory, 'result.json'), `${JSON.stringify(result, null, 2)}\n`);
    process.stdout.write(
      `\n传输 ${result.transport}：入口在客户端强杀后 ` +
        `${result.entryStoppedAfterMilliseconds === null ? '未停止' : `${result.entryStoppedAfterMilliseconds} ms 停止接受连接`}\n`,
    );
    process.stdout.write(`证据：${caseDirectory}\n`);
  }
}

const outcome = await main();
if (outcome.status !== 'passed') {
  process.exit(1);
}
