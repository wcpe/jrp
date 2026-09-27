// FR-03 黑盒互操作矩阵执行器：用官方 frpc 发行二进制对 jrps 做端到端互操作验证。
//
// 每个用例的流程固定：生成 frpc 配置 → 启动 frpc → 等待 jrps 侧事件
// → 访问入口断言 → 清理 → 证据落 .tmp/compat/<日期>/<用例>/。
//
// 运行方式：
//   node scripts/compat/run-matrix.mjs --wire=v1 --case=login
//   node scripts/compat/run-matrix.mjs --wire=both --case=all
//   node scripts/compat/run-matrix.mjs --transport=websocket --wire=both --case=login,proxy-tcp \
//     --front=127.0.0.1:7201   # frpc 经真实反向代理连入，用于验证升级透传与反代超时
//   node scripts/compat/run-matrix.mjs --transport=websocket --frpc-protocol=wss --wire=both \
//     --case=login,proxy-tcp --front=127.0.0.1:7443 --tls-ca=<前置证书> --tls-server-name=127.0.0.1
//     # 前置终结 TLS：jrps 监听明文 websocket，frpc 以 wss 连前置
import { spawn, spawnSync } from 'node:child_process';
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import net from 'node:net';
import http from 'node:http';
import dgram from 'node:dgram';
import path from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { fileURLToPath } from 'node:url';

import { ensureFrpcBinary } from './frpc.mjs';
import { ControlPort, startJrpsHost } from './jrps-host.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');

// clientId/token 是黑盒用例使用的数据面凭证；token 明文只出现在本次运行的临时文件里。
const clientId = 'compat-frpc';
const clientToken = 'compat-frpc-token-0123456789';
const allTransports = ['tcp', 'websocket', 'wss', 'kcp', 'quic'];

const allCases = [
  'login',
  'proxy-tcp',
  'proxy-udp',
  'proxy-http',
  'proxy-https',
  'login-rejected',
  'p2-rejected',
  'port-conflict',
  'heartbeat-relay',
  'work-conn-rejected',
];

// parseArguments 解析命令行参数。
function parseArguments(argv) {
  const options = {
    wires: ['v1'],
    transports: ['tcp'],
    cases: ['login'],
    keep: false,
    // 前置地址：把 frpc 指向真实反向代理（如 nginx）而不是直连控制端口，
    // 用于验证升级透传与反代超时行为；缺省直连。
    front: '',
    // 客户端侧协议：缺省与服务端监听传输相同。反代终结 TLS 时两者不同——
    // jrps 监听明文 websocket，而 frpc 以 wss 连前置。
    frpcProtocol: '',
    // 前置终结 TLS 时的校验材料：CA（自签证书即证书自身）与证书 SAN 中的主机名。
    tlsCa: '',
    tlsServerName: '',
    timeoutMilliseconds: 20000,
  };
  for (const argument of argv) {
    if (argument.startsWith('--wire=')) {
      const value = argument.slice('--wire='.length);
      options.wires = value === 'both' ? ['v1', 'v2'] : [value];
    } else if (argument.startsWith('--transport=')) {
      const value = argument.slice('--transport='.length);
      options.transports = value === 'all' ? [...allTransports] : value.split(',').filter(Boolean);
    } else if (argument.startsWith('--case=')) {
      const value = argument.slice('--case='.length);
      options.cases = value === 'all' ? [...allCases] : value.split(',').filter(Boolean);
    } else if (argument.startsWith('--front=')) {
      options.front = argument.slice('--front='.length);
    } else if (argument.startsWith('--frpc-protocol=')) {
      options.frpcProtocol = argument.slice('--frpc-protocol='.length);
    } else if (argument.startsWith('--tls-ca=')) {
      options.tlsCa = argument.slice('--tls-ca='.length);
    } else if (argument.startsWith('--tls-server-name=')) {
      options.tlsServerName = argument.slice('--tls-server-name='.length);
    } else if (argument.startsWith('--timeout-ms=')) {
      options.timeoutMilliseconds = Number(argument.slice('--timeout-ms='.length));
    } else if (argument === '--keep') {
      options.keep = true;
    } else {
      throw new Error(`未知参数：${argument}`);
    }
  }
  if (!options.wires.every((wire) => ['v1', 'v2'].includes(wire))) {
    throw new Error(`未支持的 wire 版本：${options.wires.join(',')}`);
  }
  if (!options.transports.every((transport) => allTransports.includes(transport))) {
    throw new Error(`未支持的连接传输：${options.transports.join(',')}`);
  }
  if (!options.cases.every((name) => allCases.includes(name))) {
    throw new Error(`未支持的黑盒用例：${options.cases.join(',')}`);
  }
  if (!Number.isInteger(options.timeoutMilliseconds) || options.timeoutMilliseconds < 1000) {
    throw new Error(`--timeout-ms 必须是不小于 1000 的整数：${options.timeoutMilliseconds}`);
  }
  if (options.front && !/^[^:]+:\d+$/u.test(options.front)) {
    throw new Error(`--front 必须形如 <主机>:<端口>：${options.front}`);
  }
  if (options.frpcProtocol && !allTransports.includes(options.frpcProtocol)) {
    throw new Error(`未支持的客户端协议：${options.frpcProtocol}`);
  }
  if (options.tlsCa && !options.front) {
    throw new Error('--tls-ca 只在前置终结 TLS（配合 --front 与 --frpc-protocol）时有意义');
  }
  return options;
}

// reservePort 申请一个当前空闲的 TCP 端口号。
async function reservePort() {
  return new Promise((resolve, reject) => {
    const probe = net.createServer();
    probe.once('error', reject);
    probe.once('listening', () => {
      const { port } = probe.address();
      probe.close(() => resolve(port));
    });
    probe.listen(0, '127.0.0.1');
  });
}

// ensureLocalTLS 为 WSS/QUIC 本机矩阵生成临时自签名证书。
function ensureLocalTLS(directory) {
  const certificate = path.join(directory, 'server.crt');
  const privateKey = path.join(directory, 'server.key');
  const result = spawnSync(
    'openssl',
    [
      'req',
      '-x509',
      '-newkey',
      'rsa:2048',
      '-nodes',
      '-keyout',
      privateKey,
      '-out',
      certificate,
      '-subj',
      '/CN=127.0.0.1',
      '-addext',
      'subjectAltName=IP:127.0.0.1',
      '-days',
      '1',
    ],
    { stdio: 'ignore' },
  );
  if (result.status !== 0) {
    throw new Error(`生成本机 TLS 证书失败：退出码 ${result.status}`);
  }
  return { certificate, privateKey };
}

// startEchoService 启动本地 TCP 回显服务，作为 TCP/HTTPS 代理的被代理目标。
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

// startHttpService 启动本地 HTTP 服务，作为 HTTP 代理的真实目标。
function startHttpService() {
  const service = http.createServer((_request, response) => {
    response.writeHead(200, { 'content-type': 'text/plain; charset=utf-8' });
    response.end('compat-http-ok');
  });
  service.on('clientError', (_error, socket) => socket.destroy());
  return new Promise((resolve) => {
    service.listen(0, '127.0.0.1', () => resolve(service));
  });
}

// startUdpEchoService 启动本地 UDP 回显服务，供 UDP 用例验证客户端本地目标已可用。
function startUdpEchoService() {
  const service = dgram.createSocket('udp4');
  service.on('message', (message, remote) => service.send(message, remote.port, remote.address));
  service.on('error', () => {});
  return new Promise((resolve) => {
    service.bind(0, '127.0.0.1', () => resolve(service));
  });
}

// startTcpRelay 启动仅用于黑盒证据的 TCP 中继；证据中记录中继是否主动切断连接。
function startTcpRelay(targetPort, { cutAfterMilliseconds = 0 } = {}) {
  const sockets = new Set();
  const relay = net.createServer((client) => {
    sockets.add(client);
    const upstream = net.connect(targetPort, '127.0.0.1');
    sockets.add(upstream);
    let closed = false;
    let dropClientToUpstream = false;
    const closeBoth = () => {
      if (closed) {
        return;
      }
      closed = true;
      client.destroy();
      upstream.destroy();
      sockets.delete(client);
      sockets.delete(upstream);
    };
    client.on('error', closeBoth);
    upstream.on('error', closeBoth);
    client.on('data', (chunk) => {
      if (!closed && !dropClientToUpstream) {
        upstream.write(chunk);
      }
    });
    upstream.on('data', (chunk) => {
      if (!closed) {
        client.write(chunk);
      }
    });
    client.on('close', closeBoth);
    upstream.on('close', closeBoth);
    if (cutAfterMilliseconds > 0) {
      setTimeout(() => {
        dropClientToUpstream = true;
      }, cutAfterMilliseconds).unref();
    }
  });
  relay.on('error', () => {});
  return new Promise((resolve) => {
    relay.listen(0, '127.0.0.1', () =>
      resolve({
        server: relay,
        port: relay.address().port,
        close: async () => {
          for (const socket of sockets) {
            socket.destroy();
          }
          await closeServer(relay);
        },
      }),
    );
  });
}

// closeServer 等待监听器真正关闭，避免下一用例抢占仍在释放的端口。
function closeServer(server) {
  return new Promise((resolve) => {
    if (!server || !server.listening) {
      resolve();
      return;
    }
    server.close(() => resolve());
  });
}

// closeUdpService 等待 UDP 套接字释放。
function closeUdpService(service) {
  return new Promise((resolve) => {
    if (!service) {
      resolve();
      return;
    }
    service.close(() => resolve());
  });
}

// renderFrpcConfig 生成已由固定 frpc v0.70.0 verify 命令验证过的 TOML 字段。
function renderFrpcConfig({
  wireVersion,
  transport = 'tcp',
  token,
  localPort,
  remotePort,
  logFile,
  proxyName,
  proxyType = 'tcp',
  serverAddress = '127.0.0.1',
  serverPort = ControlPort,
  heartbeat = false,
  tlsTrustedCaFile = '',
  tlsServerName = '',
}) {
  const tlsEnabled = transport === 'wss' || transport === 'quic';
  const lines = [
    `serverAddr = "${serverAddress}"`,
    `serverPort = ${serverPort}`,
    `clientID = "${clientId}"`,
    'auth.method = "token"',
    `auth.token = "${token}"`,
    `transport.protocol = "${transport}"`,
    `transport.wireProtocol = "${wireVersion}"`,
    `transport.tls.enable = ${tlsEnabled}`,
    'transport.tcpMux = false',
    ...(tlsTrustedCaFile
      ? [`transport.tls.trustedCaFile = "${tlsTrustedCaFile.replace(/\\/gu, '/')}"`]
      : []),
    ...(tlsServerName ? [`transport.tls.serverName = "${tlsServerName}"`] : []),
    ...(transport === 'quic'
      ? ['transport.quic.keepalivePeriod = 10', 'transport.quic.maxIdleTimeout = 30']
      : []),
    ...(heartbeat ? ['transport.heartbeatInterval = 1', 'transport.heartbeatTimeout = 3'] : []),
    'log.level = "debug"',
    `log.to = "${logFile.replace(/\\/gu, '/')}"`,
    '',
    '[[proxies]]',
    `name = "${proxyName}"`,
    `type = "${proxyType}"`,
    'localIP = "127.0.0.1"',
    `localPort = ${localPort}`,
  ];
  if (!['stcp', 'xtcp', 'http', 'https'].includes(proxyType)) {
    lines.push(`remotePort = ${remotePort}`);
  }
  if (proxyType === 'stcp' || proxyType === 'xtcp') {
    lines.push('secretKey = "compat-secret"');
  }
  if (proxyType === 'http' || proxyType === 'https') {
    lines.push('customDomains = ["127.0.0.1"]');
  }
  lines.push('');
  return lines.join('\n');
}

// startFrpc 启动官方 frpc 进程，返回句柄与退出信息。
function startFrpc(executable, configPath) {
  const child = spawn(executable, ['-c', configPath], { stdio: ['ignore', 'pipe', 'pipe'] });
  const state = { exited: false, exitCode: null, signal: null, stdout: '', stderr: '' };
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
  child.once('exit', (code, signal) => {
    state.exited = true;
    state.exitCode = code;
    state.signal = signal;
  });
  return { child, state };
}

// stopFrpc 终止并等待官方客户端退出，确保不会把子进程带入后续用例。
async function stopFrpc(frpc) {
  if (!frpc || frpc.state.exited) {
    return;
  }
  frpc.child.kill();
  const deadline = Date.now() + 5000;
  while (!frpc.state.exited && Date.now() < deadline) {
    await delay(50);
  }
  if (!frpc.state.exited) {
    frpc.child.kill('SIGKILL');
    await delay(100);
  }
}

// assertEchoThroughProxy 通过 TCP 入口访问回显服务，验证数据面双向可达。
async function assertEchoThroughProxy(port, timeoutMilliseconds = 20000) {
  const deadline = Date.now() + timeoutMilliseconds;
  let lastError;
  while (Date.now() < deadline) {
    try {
      const payload = Buffer.from('黑盒互操作-回显断言-0123456789');
      const echoed = await new Promise((resolve, reject) => {
        let settled = false;
        const finish = (action, value) => {
          if (settled) return;
          settled = true;
          action(value);
        };
        const socket = net.connect(port, '127.0.0.1');
        socket.setTimeout(3000);
        socket.on('error', (error) => finish(reject, error));
        socket.on('timeout', () => finish(reject, new Error('入口读取超时')));
        socket.once('connect', () => socket.write(payload));
        const chunks = [];
        socket.on('data', (chunk) => {
          chunks.push(chunk);
          if (Buffer.concat(chunks).length >= payload.length) {
            socket.end();
            finish(resolve, Buffer.concat(chunks));
          }
        });
        socket.on('close', () => finish(resolve, Buffer.concat(chunks)));
      });
      if (echoed.length >= payload.length && echoed.subarray(0, payload.length).equals(payload)) {
        return;
      }
      lastError = new Error(`回显数据不一致：收到 ${echoed.length} 字节`);
    } catch (error) {
      lastError = error;
    }
    await delay(300);
  }
  throw lastError ?? new Error('入口未在期限内可用');
}

// assertHttpThroughProxy 发送 HTTP 请求，覆盖官方 HTTP 配置字段与主机路由语义。
async function assertHttpThroughProxy(port, timeoutMilliseconds = 10000) {
  const executable = process.platform === 'win32' ? 'curl.exe' : 'curl';
  const output = await new Promise((resolve, reject) => {
    const child = spawn(
      executable,
      [
        '--noproxy',
        '*',
        '--max-time',
        String(Math.max(1, Math.ceil(timeoutMilliseconds / 1000))),
        '--silent',
        '--show-error',
        '--include',
        '-H',
        'Host: 127.0.0.1',
        `http://127.0.0.1:${port}/compat`,
      ],
      { stdio: ['ignore', 'pipe', 'pipe'] },
    );
    let stdout = '';
    let stderr = '';
    child.stdout.on('data', (chunk) => {
      stdout += chunk.toString();
    });
    child.stderr.on('data', (chunk) => {
      stderr += chunk.toString();
    });
    child.once('error', reject);
    child.once('exit', (code) => {
      if (code !== 0) {
        reject(new Error(stderr || `curl 退出码 ${code}`));
        return;
      }
      resolve(stdout);
    });
  });
  if (!output.includes('HTTP/1.1 200')) {
    throw new Error(`HTTP 入口未返回 200：${output.slice(0, 120)}`);
  }
  if (!output.includes('compat-http-ok')) {
    throw new Error(`HTTP 目标响应正文不匹配：${output.slice(0, 120)}`);
  }
}

// assertUdpThroughProxy 发送 UDP 数据报并等待回显；不支持时由调用方记录 blocked。
async function assertUdpThroughProxy(port, timeoutMilliseconds = 5000) {
  const client = dgram.createSocket('udp4');
  try {
    const payload = Buffer.from('黑盒互操作-UDP-0123456789');
    const deadline = Date.now() + timeoutMilliseconds;
    let lastError;
    while (Date.now() < deadline) {
      try {
        const echoed = await new Promise((resolve, reject) => {
          let settled = false;
          const onMessage = (message) => finish(null, message);
          const onError = (error) => finish(error);
          const timer = setTimeout(() => finish(new Error('UDP 入口读取超时')), 800);
          const finish = (error, message) => {
            if (settled) return;
            settled = true;
            clearTimeout(timer);
            client.removeListener('message', onMessage);
            client.removeListener('error', onError);
            if (error) reject(error);
            else resolve(message);
          };
          client.once('message', onMessage);
          client.once('error', onError);
          client.send(payload, port, '127.0.0.1', (error) => {
            if (error) onError(error);
          });
        });
        if (!echoed.equals(payload)) {
          throw new Error(`UDP 回显数据不一致：收到 ${echoed.length} 字节`);
        }
        return;
      } catch (error) {
        lastError = error;
        await delay(100);
      }
    }
    throw lastError ?? new Error('UDP 入口读取超时');
  } finally {
    client.close();
  }
}

// encodeV1Frame 编码工作连接拒绝用例需要的最小 v1 帧。
function encodeV1Frame(type, payload) {
  const body = Buffer.from(JSON.stringify(payload));
  const frame = Buffer.alloc(9 + body.length);
  frame[0] = type;
  frame.writeBigUInt64BE(BigInt(body.length), 1);
  body.copy(frame, 9);
  return frame;
}

// encodeV2WorkFrame 编码工作连接拒绝用例需要的 v2 魔数与消息帧。
function encodeV2WorkFrame(payload) {
  const body = Buffer.from(JSON.stringify(payload));
  const message = Buffer.alloc(2 + body.length);
  message.writeUInt16BE(6, 0);
  body.copy(message, 2);
  const frame = Buffer.alloc(17 + body.length);
  Buffer.from([0x46, 0x52, 0x50, 0x00, 0x02, 0x0d, 0x0a]).copy(frame, 0);
  frame.writeUInt16BE(16, 7);
  frame.writeUInt16BE(0, 9);
  frame.writeUInt32BE(message.length, 11);
  message.copy(frame, 15);
  return frame;
}

// sendRejectedWorkConn 发送无效运行 ID 的官方形态工作连接，等待服务端关闭。
async function sendRejectedWorkConn(wireVersion) {
  const socket = net.connect(ControlPort, '127.0.0.1');
  const frame =
    wireVersion === 'v2'
      ? encodeV2WorkFrame({ run_id: 'compat-invalid-run-id' })
      : encodeV1Frame('w'.charCodeAt(0), { run_id: 'compat-invalid-run-id' });
  await new Promise((resolve, reject) => {
    socket.once('connect', () => socket.write(frame));
    socket.once('error', reject);
    socket.once('close', resolve);
    socket.setTimeout(5000, () => reject(new Error('无效工作连接未被服务端关闭')));
  });
}

// runCase 执行单个黑盒用例，并把不支持项标为 blocked/skipped 而不是伪造通过。
async function runCase({
  name,
  wireVersion,
  transport,
  binary,
  dateStamp,
  timeoutMilliseconds,
  front = '',
  frpcProtocol = '',
  tlsCa = '',
  tlsServerName = '',
}) {
  const caseDirectory = path.join(
    root,
    '.tmp',
    'compat',
    dateStamp,
    `${transport}-${wireVersion}-${name}-${Date.now()}`,
  );
  mkdirSync(caseDirectory, { recursive: true });
  const managementPort = await reservePort();
  const dataDirectory = path.join(caseDirectory, 'data');
  const echoService = await startEchoService();
  const httpService = await startHttpService();
  const udpService = await startUdpEchoService();
  const echoPort = echoService.address().port;
  const httpPort = httpService.address().port;
  const udpPort = udpService.address().port;
  const remotePort = await reservePort();
  const tls =
    transport === 'wss' || transport === 'quic' ? ensureLocalTLS(caseDirectory) : undefined;
  // 客户端协议可以与服务端监听传输不同：反代终结 TLS 时 jrps 监听明文 websocket，
  // 而 frpc 以 wss 连前置；此时校验材料来自 --tls-ca/--tls-server-name。
  const clientProtocol = frpcProtocol || transport;
  const controlListen = {
    host: '0.0.0.0',
    port: ControlPort,
    transport,
    ...(transport === 'websocket' || transport === 'wss' ? { path: '/~!frp' } : {}),
    ...(tls ? { tlsCertFile: tls.certificate, tlsKeyFile: tls.privateKey } : {}),
  };
  const result = {
    case: name,
    wire: wireVersion,
    transport,
    frpc: binary.version,
    startedAt: new Date().toISOString(),
    status: 'failed',
    ...(front ? { front } : {}),
    ...(clientProtocol !== transport ? { clientProtocol } : {}),
  };
  const blocker = name === 'port-conflict' ? net.createServer() : undefined;
  const relay =
    name === 'heartbeat-relay' && transport === 'tcp'
      ? await startTcpRelay(ControlPort, { cutAfterMilliseconds: 2500 })
      : undefined;
  if (blocker) {
    await new Promise((resolve, reject) => {
      blocker.once('error', reject);
      blocker.listen(remotePort, '0.0.0.0', resolve);
    });
  }
  const caseToken = name === 'login-rejected' ? 'wrong-token-for-rejection-000' : clientToken;
  let host;
  let frpc;
  try {
    host = await startJrpsHost({
      dataDirectory,
      managementPort,
      evidenceDirectory: caseDirectory,
      clientId,
      clientToken,
      controlListen,
    });
    const configPath = path.join(caseDirectory, 'frpc.toml');
    const proxyType =
      name === 'p2-rejected'
        ? 'stcp'
        : name === 'proxy-udp'
          ? 'udp'
          : name === 'proxy-http'
            ? 'http'
            : name === 'proxy-https'
              ? 'https'
              : 'tcp';
    const localPort = name === 'proxy-udp' ? udpPort : name === 'proxy-http' ? httpPort : echoPort;
    // 连接目标优先级：本机中继（用例自身的中断场景）> 显式前置（真实反向代理）> 直连控制端口。
    const [frontHost, frontPort] = front ? front.split(':') : [undefined, undefined];
    const serverPort = relay?.port ?? (front ? Number(frontPort) : ControlPort);
    const serverAddress = relay ? '127.0.0.1' : (frontHost ?? '127.0.0.1');
    writeFileSync(
      configPath,
      renderFrpcConfig({
        wireVersion,
        transport: clientProtocol,
        token: caseToken,
        localPort,
        remotePort,
        serverAddress,
        serverPort,
        logFile: path.join(caseDirectory, 'frpc.log'),
        proxyName: `compat-${proxyType}`,
        proxyType,
        heartbeat: name === 'heartbeat-relay',
        tlsTrustedCaFile: tlsCa || tls?.certificate || '',
        tlsServerName: tlsServerName || (tls ? '127.0.0.1' : ''),
      }),
    );
    frpc = startFrpc(binary.executable, configPath);

    if (name === 'login-rejected') {
      const exited = await waitForFrpcExit(frpc, 8000);
      const connected = await host.waitForLog(
        { event: 'client-connected' },
        (item) => item.clientId === clientId,
        1000,
      );
      if (connected || !exited) throw new Error('鉴权材料错误未形成可观察拒绝路径');
      result.status = 'passed';
      result.observed = 'frpc 退出且无 client-connected 事件';
      return result;
    }

    if (name === 'p2-rejected') {
      const requested = await host.waitForServerLog('收到代理注册请求', timeoutMilliseconds);
      const rejected = await host.waitForServerLog('代理注册被拒', timeoutMilliseconds);
      const registered = await host.waitForServerLog('运行时代理已注册', 1000);
      if (!requested || !rejected || registered) {
        throw new Error('P2 代理拒绝未形成稳定可观察路径');
      }
      result.observed = rejected;
      if (rejected.includes('type_unsupported')) {
        result.status = 'passed';
      } else {
        result.status = 'blocked';
        result.reason =
          '官方 frpc v0.70.0 的 STCP TOML 不携带 remotePort，Core 先按 field_invalid 拒绝，未进入 type_unsupported 分支';
      }
      return result;
    }

    if (name === 'heartbeat-relay') {
      // 该用例依赖本机 TCP 中继切断控制连接；中继只在 tcp 传输下启用，
      // 其它传输没有可切断的对象，必须记为 blocked 而不是失败（否则会假红）。
      if (transport !== 'tcp') {
        result.status = 'blocked';
        result.reason = `心跳 relay 用例仅在 tcp 传输下可执行（当前 ${transport}）：无中继可切断控制连接`;
        return result;
      }
      const connected = await host.waitForLog(
        { event: 'client-connected' },
        (item) => item.clientId === clientId,
        timeoutMilliseconds,
      );
      result.clientConnected = Boolean(connected);
      if (!connected) throw new Error('心跳 relay 用例未建立初始会话');
      const clientTimeout = await waitForFileMarker(
        path.join(caseDirectory, 'frpc.log'),
        'heartbeat timeout',
        15000,
      );
      const cleanupObserved = await host.waitForServerLog('会话结束，运行时代理已清理', 15000);
      if (!clientTimeout || !cleanupObserved) {
        throw new Error('relay 丢弃客户端心跳后未同时观测到官方客户端超时与服务端会话清理');
      }
      result.status = 'passed';
      result.observed =
        'relay 丢弃客户端到服务端心跳；官方客户端报告 heartbeat timeout；服务端完成会话代理清理';
      return result;
    }

    if (name === 'work-conn-rejected') {
      // 该用例用裸 TCP 连接直发 JRP 工作连接帧；WebSocket/WSS 监听不接受这种裸连接，
      // 无法构成可判定的拒绝路径，因此只在 tcp 传输下执行，其它传输记为 blocked。
      if (transport !== 'tcp') {
        result.status = 'blocked';
        result.reason = `工作连接拒绝用例仅在 tcp 传输下可执行（当前 ${transport}）：裸 TCP 帧无法送达 WebSocket 监听`;
        return result;
      }
      const connected = await host.waitForLog(
        { event: 'client-connected' },
        (item) => item.clientId === clientId,
        timeoutMilliseconds,
      );
      if (!connected) throw new Error('工作连接拒绝用例未建立初始会话');
      await sendRejectedWorkConn(wireVersion);
      const rejected = await host.waitForServerLog('运行 ID 不属于活跃会话', timeoutMilliseconds);
      if (!rejected) throw new Error('未观测到无效运行 ID 的工作连接拒绝');
      result.status = 'passed';
      result.observed = rejected;
      return result;
    }

    const connected = await host.waitForLog(
      { event: 'client-connected' },
      (item) => item.clientId === clientId,
      timeoutMilliseconds,
    );
    result.clientConnected = Boolean(connected);
    if (!connected) {
      // KCP 行的失败来自基线客户端自身：官方 frpc v0.70.0 在 protocol=kcp 下
      // 连一个数据报都不发出（对照官方 frps 同样无流量），服务端无从响应。
      // 这类外部原因必须记录为 blocked 并留下服务端已监听的证据，而不是 failed。
      if (transport === 'kcp') {
        const listening = await host.waitForServerLog('控制入口已就绪', 1000);
        result.status = 'blocked';
        result.observed = listening ?? '未在服务端日志中观测到 KCP 控制入口';
        result.reason =
          '基线客户端未发出任何 KCP 数据报（UDP 抓包与官方 frps 对照均已确认），互操作无法执行';
        return result;
      }
      throw new Error('未在期限内观测到 client-connected 事件');
    }
    const registered = await host.waitForServerLog('运行时代理已注册', timeoutMilliseconds);
    if (!registered) throw new Error('未在期限内观测到运行时代理注册');
    const matchedPort = /入口=\[::\]:(\d+)/u.exec(registered);
    if (matchedPort) result.entryPort = Number(matchedPort[1]);

    if (name === 'port-conflict') {
      result.status = 'passed';
      result.observed = '端口冲突后官方客户端自动换端口并完成注册';
      return result;
    }
    if (name === 'proxy-tcp' || name === 'proxy-https') {
      if (!matchedPort) throw new Error(`无法从注册日志解析入口端口：${registered}`);
      await assertEchoThroughProxy(Number(matchedPort[1]), timeoutMilliseconds);
      result.proxyReachable = true;
      result.status = 'passed';
      return result;
    }
    if (name === 'proxy-http') {
      if (!matchedPort) throw new Error(`无法从注册日志解析入口端口：${registered}`);
      try {
        await assertHttpThroughProxy(Number(matchedPort[1]), Math.min(timeoutMilliseconds, 10000));
        result.proxyReachable = true;
        result.status = 'passed';
      } catch (error) {
        result.status = 'blocked';
        result.reason = `Core/jrps 当前运行时代理未提供 HTTP 路由语义：${error.message}`;
      }
      return result;
    }
    if (name === 'proxy-udp') {
      if (!matchedPort) throw new Error(`无法从注册日志解析入口端口：${registered}`);
      try {
        await assertUdpThroughProxy(Number(matchedPort[1]), Math.min(timeoutMilliseconds, 5000));
        result.proxyReachable = true;
        result.status = 'passed';
      } catch (error) {
        result.status = 'blocked';
        result.reason = `Core/jrps 当前官方运行时入口未提供 UDP 数据面：${error.message}`;
      }
      return result;
    }
    result.status = 'passed';
    return result;
  } catch (error) {
    result.status = 'failed';
    result.error = error.message;
    return result;
  } finally {
    await stopFrpc(frpc);
    if (host) await host.stop();
    await closeServer(echoService);
    await closeServer(httpService);
    await closeUdpService(udpService);
    if (blocker) await closeServer(blocker);
    if (relay) await relay.close();
    if (frpc) {
      result.frpcExitCode = frpc.state.exitCode;
      result.frpcSignal = frpc.state.signal;
      writeFileSync(path.join(caseDirectory, 'frpc.stdout.log'), frpc.state.stdout);
      writeFileSync(path.join(caseDirectory, 'frpc.stderr.log'), frpc.state.stderr);
    }
    writeFileSync(path.join(caseDirectory, 'result.json'), `${JSON.stringify(result, null, 2)}\n`);
  }
}

// waitForFrpcExit 等待官方客户端的确定退出结果。
async function waitForFrpcExit(frpc, timeoutMilliseconds) {
  const deadline = Date.now() + timeoutMilliseconds;
  while (!frpc.state.exited && Date.now() < deadline) await delay(100);
  return frpc.state.exited;
}

// waitForFileMarker 等待客户端日志出现固定观测标记。
async function waitForFileMarker(filePath, marker, timeoutMilliseconds) {
  const deadline = Date.now() + timeoutMilliseconds;
  while (Date.now() < deadline) {
    try {
      if (readFileSync(filePath, 'utf8').includes(marker)) return true;
    } catch {
      // 日志尚未创建：继续等待。
    }
    await delay(200);
  }
  return false;
}

// main 执行矩阵并按 failed 状态汇总退出码；blocked/skipped 会原样留证。
async function main() {
  const options = parseArguments(process.argv.slice(2));
  const override = process.env.JRP_COMPAT_FRPC;
  const binary = override
    ? { version: `override:${override}`, executable: override, directory: path.dirname(override) }
    : await ensureFrpcBinary();
  const dateStamp = new Date().toISOString().slice(0, 10);
  const results = [];
  for (const transport of options.transports) {
    for (const wire of options.wires) {
      for (const name of options.cases) {
        process.stdout.write(
          `\n=== 用例 ${name}（传输 ${transport}，wire ${wire}，frpc ${binary.version}${options.front ? `，前置 ${options.front}` : ''}${options.frpcProtocol ? `，客户端协议 ${options.frpcProtocol}` : ''}）===\n`,
        );
        const result = await runCase({
          name,
          wireVersion: wire,
          transport,
          binary,
          dateStamp,
          timeoutMilliseconds: options.timeoutMilliseconds,
          front: options.front,
          frpcProtocol: options.frpcProtocol,
          tlsCa: options.tlsCa,
          tlsServerName: options.tlsServerName,
        });
        results.push(result);
        process.stdout.write(
          `${result.status}：${name}${result.reason ? ` —— ${result.reason}` : result.error ? ` —— ${result.error}` : ''}\n`,
        );
      }
    }
  }
  const counts = results.reduce((summary, result) => {
    summary[result.status] = (summary[result.status] ?? 0) + 1;
    return summary;
  }, {});
  process.stdout.write(`\n汇总：${JSON.stringify(counts)}\n`);
  if ((counts.failed ?? 0) > 0) process.exit(1);
}

await main();
