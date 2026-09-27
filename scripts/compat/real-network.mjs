// 实机验收执行器：在目标网络环境对远端 jrps 跑一条命令的端到端验收。
//
// 与 run-matrix.mjs 的分工：矩阵脚本在本机自建 jrps 并读取其日志，属自动化回归；
// 本脚本面向真实网络——远端 jrps 的日志不可读，因此断言只依赖客户端日志、入口
// 可达性与数据回显，并把必须由人观察的项目（NAT 类型、反向代理、跨运营商、跨平台）
// 以清单形式写进报告，由执行者回填。
//
// 运行方式：
//   node scripts/compat/real-network.mjs --server=203.0.113.10:7200 --transport=wss \
//     --wire=both --client-id=frpc-1 --token=<数据面 token> --entry-port=6000 \
//     --tls-ca=./ca.crt --tls-server-name=proxy.example.com
//
// 全部参数以 `--help` 打印；实机批次的执行清单见 docs/OPERATIONS.md「官方 frpc 接入的实机验收批次」。
import { spawn } from 'node:child_process';
import { mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import net from 'node:net';
import dgram from 'node:dgram';
import path from 'node:path';
import tls from 'node:tls';
import { setTimeout as delay } from 'node:timers/promises';
import { fileURLToPath } from 'node:url';

import { ensureFrpcBinary } from './frpc.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');

// 支持的连接传输与 wire 版本；与 PROTOCOL §9 的矩阵声明一致。
const transports = ['tcp', 'websocket', 'wss', 'kcp', 'quic'];
const wires = ['v1', 'v2'];

// usage 说明全部参数。执行者按此自证命令形状，避免依赖外部清单文档。
const usage = `实机验收执行器用法：

  node scripts/compat/real-network.mjs --server=<主机:端口> [选项]

只读预检（不需要凭据，不启动客户端，不注册代理）：
  node scripts/compat/real-network.mjs --server=<主机:端口> --transport=wss --preflight-only

必填：
  --server=<主机:端口>        远端 jrps 控制入口地址
  --entry-port=<端口>         服务端为代理分配的入口端口（预检模式不需要）
  --token=<数据面 token>      数据面凭据（预检模式不需要）

可选：
  --transport=<值>            连接传输，all 表示全部：tcp|websocket|wss|kcp|quic（默认 tcp）
  --wire=<值>                 wire 版本，both 表示两个都跑：v1|v2（默认 v1）
  --client-id=<标识>          客户端标识（默认脚本生成的随机值）
  --entry-host=<主机>         代理入口所在主机（默认与控制入口同主机）
  --proxy-type=<类型>         代理类型（默认 tcp）
  --local-port=<端口>         本地回显目标端口（默认自动选择空闲端口）
  --tls-ca=<路径>             WSS/HTTPS 校验用的 CA 证书（自签证书必需）
  --tls-server-name=<主机名>  与证书 SAN 匹配的服务名（默认取控制入口主机）
  --duration=<秒>             每个用例的观察时长，不小于 5（默认 20）
  --entry-release-wait=<秒>   用例间等待入口端口释放的上限（默认 40）
  --frpc=<路径>               指定官方 frpc 可执行文件（默认自动下载基线版本）
  -h, --help                  显示本说明
`;

// parseArguments 解析命令行参数。
function parseArguments(argv) {
  const options = {
    help: false,
    server: '',
    transports: [],
    wires: [],
    clientId: '',
    token: '',
    // 代理入口所在主机；缺省与控制入口同主机。真实部署里控制入口常经反向代理或
    // 负载均衡暴露，而代理入口是各代理自己的端口，两者不同主机是常见形态。
    entryHost: '',
    proxyType: 'tcp',
    entryPort: 0,
    localPort: 0,
    tlsCa: '',
    tlsServerName: '',
    durationSeconds: 20,
    // 只读预检：只做可达性与证书探查，不启动客户端、不注册任何代理、不需要凭据。
    preflightOnly: false,
    // 上一用例的入口端口释放等待上限（秒）：UDP 载体（KCP/QUIC）在客户端被强杀后
    // 只能等会话空闲回收，回收慢于 TCP，因此默认给足一个空闲超时窗口。
    entryReleaseSeconds: 40,
    frpc: process.env.JRP_COMPAT_FRPC ?? '',
  };
  for (const argument of argv) {
    const [name, value = ''] = argument.split('=');
    switch (name) {
      case '--server':
        options.server = value;
        break;
      case '--transport':
        options.transports = value === 'all' ? [...transports] : value.split(',').filter(Boolean);
        break;
      case '--wire':
        options.wires = value === 'both' ? [...wires] : value.split(',').filter(Boolean);
        break;
      case '--client-id':
        options.clientId = value;
        break;
      case '--token':
        options.token = value;
        break;
      case '--entry-host':
        options.entryHost = value;
        break;
      case '--proxy-type':
        options.proxyType = value;
        break;
      case '--entry-port':
        options.entryPort = Number(value);
        break;
      case '--local-port':
        options.localPort = Number(value);
        break;
      case '--tls-ca':
        options.tlsCa = value;
        break;
      case '--tls-server-name':
        options.tlsServerName = value;
        break;
      case '--duration':
        options.durationSeconds = Number(value);
        break;
      case '--entry-release-wait':
        options.entryReleaseSeconds = Number(value);
        break;
      case '--preflight-only':
        options.preflightOnly = true;
        break;
      case '--frpc':
        options.frpc = value;
        break;
      case '-h':
      case '--help':
        options.help = true;
        break;
      default:
        throw new Error(`未知参数：${argument}（用 --help 查看用法）`);
    }
  }
  // 帮助优先：不校验其余参数形状，让用户在缺参数时也能看到用法。
  if (options.help) {
    return options;
  }
  if (!options.server.includes(':')) {
    throw new Error('必须提供 --server=<主机:端口>');
  }
  if (options.transports.length === 0) {
    options.transports = ['tcp'];
  }
  if (options.wires.length === 0) {
    options.wires = ['v1'];
  }
  if (!options.transports.every((item) => transports.includes(item))) {
    throw new Error(`未支持的连接传输：${options.transports.join(',')}`);
  }
  if (!options.wires.every((item) => wires.includes(item))) {
    throw new Error(`未支持的 wire 版本：${options.wires.join(',')}`);
  }
  if (
    !options.preflightOnly &&
    (!Number.isInteger(options.entryPort) || options.entryPort <= 0 || options.entryPort > 65535)
  ) {
    throw new Error('必须提供 --entry-port=<服务端代理入口端口>');
  }
  if (!Number.isInteger(options.durationSeconds) || options.durationSeconds < 5) {
    throw new Error('--duration 必须是不小于 5 的整数（秒）');
  }
  return options;
}

// startEchoService 启动本地回显服务，作为被代理目标。
function startEchoService(port) {
  const service = net.createServer((socket) => {
    socket.on('error', () => socket.destroy());
    socket.pipe(socket);
  });
  service.on('error', () => {});
  return new Promise((resolve) => {
    service.listen(port, '127.0.0.1', () => resolve(service));
  });
}

// closeServer 等待监听器真正关闭。
function closeServer(server) {
  return new Promise((resolve) => {
    if (!server || !server.listening) {
      resolve();
      return;
    }
    server.close(() => resolve());
  });
}

// checkTcpReachability 探测控制端口 TCP 可达性。
function checkTcpReachability(host, port, timeoutMilliseconds = 5000) {
  return new Promise((resolve) => {
    const socket = net.connect({ host, port });
    const finish = (result) => {
      socket.destroy();
      resolve(result);
    };
    socket.setTimeout(timeoutMilliseconds);
    socket.once('connect', () => finish({ reachable: true }));
    socket.once('timeout', () => finish({ reachable: false, reason: '连接超时' }));
    socket.once('error', (error) => finish({ reachable: false, reason: error.message }));
  });
}

// checkUdpProbe 发送一个探测数据报；UDP 无连接语义，只能报告本端是否发出。
function checkUdpProbe(host, port) {
  return new Promise((resolve) => {
    const socket = dgram.createSocket('udp4');
    socket.once('error', (error) => {
      socket.close();
      resolve({ sent: false, reason: error.message });
    });
    socket.send(Buffer.from('jrp-real-network-probe'), port, host, (error) => {
      socket.close();
      resolve(
        error
          ? { sent: false, reason: error.message }
          : { sent: true, reason: '已发出探测数据报（UDP 不可达需由登录结果判定）' },
      );
    });
  });
}

// checkTlsChain 校验 TLS 证书链；未提供 CA 时只报告对端证书主体。
function checkTlsChain(host, port, options) {
  return new Promise((resolve) => {
    const socket = tls.connect({
      host,
      port,
      // RFC 6066 不允许 SNI 使用 IP 字面量：仅在实际给的是主机名时携带。
      servername:
        net.isIP(options.tlsServerName) === 0 ? options.tlsServerName || undefined : undefined,
      ca: options.tlsCa ? readFileSync(options.tlsCa) : undefined,
      rejectUnauthorized: Boolean(options.tlsCa),
      timeout: 5000,
    });
    const finish = (result) => {
      socket.destroy();
      resolve(result);
    };
    socket.once('secureConnect', () => {
      const certificate = socket.getPeerCertificate();
      finish({
        established: true,
        authorized: socket.authorized,
        subject: certificate?.subject?.CN ?? '',
        issuer: certificate?.issuer?.CN ?? '',
        reason: socket.authorizationError ? String(socket.authorizationError) : '',
      });
    });
    socket.once('timeout', () => finish({ established: false, reason: 'TLS 握手超时' }));
    socket.once('error', (error) => finish({ established: false, reason: error.message }));
  });
}

// renderFrpcConfig 生成官方 frpc 配置；字段名与官方 v0.70.0 的 schema 一致。
function renderFrpcConfig(options, caseDirectory, localPort) {
  const transport = options.transport;
  const secure = transport === 'wss' || transport === 'quic';
  const lines = [
    `serverAddr = "${options.host}"`,
    `serverPort = ${options.port}`,
    ...(options.clientId ? [`clientID = "${options.clientId}"`] : []),
    'auth.method = "token"',
    `auth.token = "${options.token}"`,
    `transport.protocol = "${transport}"`,
    `transport.wireProtocol = "${options.wire}"`,
    `transport.tls.enable = ${secure}`,
    'transport.tcpMux = false',
    ...(options.tlsCa
      ? [`transport.tls.trustedCaFile = "${options.tlsCa.replace(/\\/gu, '/')}"`]
      : []),
    ...(options.tlsServerName ? [`transport.tls.serverName = "${options.tlsServerName}"`] : []),
    ...(transport === 'quic'
      ? ['transport.quic.keepalivePeriod = 10', 'transport.quic.maxIdleTimeout = 30']
      : []),
    'log.level = "debug"',
    `log.to = "${path.join(caseDirectory, 'frpc.log').replace(/\\/gu, '/')}"`,
    '',
    '[[proxies]]',
    // 代理名逐用例唯一：同一环境连续执行多个组合时，上一个会话的运行时代理
    // 可能尚未被服务端清理，固定名字会撞成 name_conflict。
    `name = "real-network-${transport}-${options.wire}"`,
    `type = "${options.proxyType}"`,
    'localIP = "127.0.0.1"',
    `localPort = ${localPort}`,
    `remotePort = ${options.entryPort}`,
    '',
  ];
  return lines.join('\n');
}

// startFrpc 启动官方客户端并收集输出。
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

// stopFrpc 终止客户端并等待退出。
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

// waitForLogMarker 等待客户端日志出现标记。
//
// offset 用于「重启后再次出现同一标记」的场景：只检查偏移之后新增的内容，
// 否则重启前的旧标记会让新断言立即通过。
async function waitForLogMarker(logPath, marker, timeoutMilliseconds, offset = 0) {
  const deadline = Date.now() + timeoutMilliseconds;
  while (Date.now() < deadline) {
    if (readLog(logPath).slice(offset).includes(marker)) {
      return true;
    }
    await delay(200);
  }
  return false;
}

// readLog 读取客户端日志；文件不存在时返回空串。
function readLog(logPath) {
  try {
    return readFileSync(logPath, 'utf8');
  } catch {
    return '';
  }
}

// assertEntryEcho 通过远端入口访问本地目标，断言数据双向可达。
async function assertEntryEcho(host, port, timeoutMilliseconds) {
  const deadline = Date.now() + timeoutMilliseconds;
  const payload = Buffer.from('实机验收-回显-0123456789');
  let lastError;
  while (Date.now() < deadline) {
    try {
      const echoed = await new Promise((resolve, reject) => {
        let settled = false;
        const finish = (action, value) => {
          if (settled) {
            return;
          }
          settled = true;
          action(value);
        };
        const socket = net.connect({ host, port });
        socket.setTimeout(5000);
        socket.once('connect', () => socket.write(payload));
        socket.once('timeout', () => finish(reject, new Error('入口读取超时')));
        socket.once('error', (error) => finish(reject, error));
        const chunks = [];
        socket.on('data', (chunk) => {
          chunks.push(chunk);
          if (Buffer.concat(chunks).length >= payload.length) {
            socket.end();
            finish(resolve, Buffer.concat(chunks));
          }
        });
        socket.once('close', () => finish(resolve, Buffer.concat(chunks)));
      });
      if (echoed.length >= payload.length && echoed.subarray(0, payload.length).equals(payload)) {
        return;
      }
      lastError = new Error(`回显不一致：收到 ${echoed.length} 字节`);
    } catch (error) {
      lastError = error;
    }
    await delay(500);
  }
  throw lastError ?? new Error('入口在期限内不可用');
}

// waitForEntryRelease 等待入口端口从上一用例释放。
//
// 同一环境连续执行多个组合时，上一个会话的运行时代理可能仍在回收中：此时新会话
// 会收到 port_conflict，而官方客户端不会在本次进程内立刻重试注册（实测 QUIC 用例
// 因此失败）。等端口真正空出来再启动客户端，比放宽断言更接近真实可用性。
async function waitForEntryRelease(host, port, timeoutMilliseconds = 25000) {
  const deadline = Date.now() + timeoutMilliseconds;
  while (Date.now() < deadline) {
    const probe = await checkTcpReachability(host, port, 1000);
    if (!probe.reachable) {
      return true;
    }
    await delay(500);
  }
  return false;
}

// runCase 执行一个「传输 × wire」组合的实机验收。
async function runCase(options, binary, dateStamp, localPort, echoService, waitEntryRelease) {
  const caseDirectory = path.join(
    root,
    '.tmp',
    'compat-real',
    dateStamp,
    `${options.transport}-${options.wire}`,
  );
  mkdirSync(caseDirectory, { recursive: true });
  const logPath = path.join(caseDirectory, 'frpc.log');
  // 每次执行都从空日志开始：沿用上次日志会让"标记已存在"的断言把上一次的
  // 成功误判为本次成功（本机自测已实测到这一误判）。
  rmSync(logPath, { force: true });
  const result = {
    transport: options.transport,
    wire: options.wire,
    server: options.server,
    frpc: binary.version,
    startedAt: new Date().toISOString(),
    status: 'failed',
    preflight: {},
    assertions: {},
  };

  if (options.transport === 'wss') {
    // WSS 在 TCP 之上完成 TLS 握手，可以直接探测证书链。
    result.preflight.tls = await checkTlsChain(options.host, options.port, options);
  } else if (options.transport === 'quic') {
    // QUIC 的 TLS 在建链内部完成，用 TCP 握手探测只会得到误导性的连接拒绝。
    result.preflight.tls = {
      skipped: true,
      reason: 'QUIC 的 TLS 在 QUIC 握手内完成，无法用 TCP 握手探测；以登录结果为准',
    };
  } else {
    result.preflight.tcp = await checkTcpReachability(options.host, options.port);
  }
  if (options.transport === 'kcp' || options.transport === 'quic') {
    result.preflight.udp = await checkUdpProbe(options.host, options.port);
  }
  if (waitEntryRelease) {
    result.preflight.entryPortReleased = await waitForEntryRelease(
      options.entryHost,
      options.entryPort,
      options.entryReleaseSeconds * 1000,
    );
  }

  const configPath = path.join(caseDirectory, 'frpc.toml');
  writeFileSync(configPath, renderFrpcConfig(options, caseDirectory, localPort));

  let frpc;
  try {
    frpc = startFrpc(binary.executable, configPath);
    result.assertions.login = await waitForLogMarker(logPath, 'login to server success', 25000);
    if (!result.assertions.login) {
      throw new Error('客户端未在期限内完成登录');
    }
    result.assertions.proxyRegistered = await waitForLogMarker(
      logPath,
      'start proxy success',
      15000,
    );
    if (!result.assertions.proxyRegistered) {
      throw new Error('客户端未在期限内完成代理注册');
    }
    await assertEntryEcho(options.entryHost, options.entryPort, 25000);
    result.assertions.entryEcho = true;

    // 心跳稳定性：观察期内不得出现客户端侧心跳超时，也不得异常退出。
    await delay(options.durationSeconds * 1000);
    const log = readLog(logPath);
    result.assertions.heartbeatStable = !log.includes('heartbeat timeout') && !frpc.state.exited;
    if (!result.assertions.heartbeatStable) {
      throw new Error('观察期内出现心跳超时或客户端退出');
    }

    // 重连观察：重启客户端后应能再次登录（真实网络下的断线恢复）。
    const logOffset = readLog(logPath).length;
    await stopFrpc(frpc);
    frpc = startFrpc(binary.executable, configPath);
    result.assertions.reconnected = await waitForLogMarker(
      logPath,
      'login to server success',
      25000,
      logOffset,
    );
    if (!result.assertions.reconnected) {
      throw new Error('客户端重启后未能在期限内重新登录');
    }

    result.status = 'passed';
    return result;
  } catch (error) {
    result.error = error.message;
    return result;
  } finally {
    await stopFrpc(frpc);
    if (frpc) {
      result.frpcExitCode = frpc.state.exitCode;
    }
    result.evidenceDirectory = caseDirectory;
    writeFileSync(path.join(caseDirectory, 'result.json'), `${JSON.stringify(result, null, 2)}\n`);
    writeFileSync(path.join(caseDirectory, 'report.md'), renderReport(options, result));
  }
}

// renderReport 生成含人工回填项的报告，作为实机验收的签字材料。
function renderReport(options, result) {
  const assertionRows = [
    ['登录成功', result.assertions.login],
    ['代理注册成功', result.assertions.proxyRegistered],
    ['入口回显逐字节一致', result.assertions.entryEcho],
    ['观察期内心跳稳定', result.assertions.heartbeatStable],
    ['客户端重启后重新登录', result.assertions.reconnected],
  ].map(([name, value]) => `| ${name} | ${value ? '✅' : '❌'} |`);
  return [
    `# 实机验收报告：${options.transport} × ${options.wire}`,
    '',
    `- 服务端：${options.server}`,
    `- 入口端口：${options.entryPort}`,
    `- 客户端：官方 frpc ${result.frpc}`,
    `- 执行时间：${result.startedAt}`,
    `- 结论：${result.status === 'passed' ? '自动化断言全部通过' : `未通过（${result.error ?? '见 result.json'}）`}`,
    '',
    '## 自动化断言',
    '',
    '| 断言 | 结果 |',
    '|---|---|',
    ...assertionRows,
    '',
    '## 预检结果',
    '',
    '```json',
    JSON.stringify(result.preflight, null, 2),
    '```',
    '',
    '## 需执行者回填（自动化无法证明）',
    '',
    '- [ ] 网络拓扑：客户端与服务端是否经过 NAT／防火墙；NAT 类型与端口映射方式',
    '      （客户端侧映射方式可实测：`node scripts/compat/nat-probe.mjs --stun=<两个以上 STUN 端点>`，把外部地址与映射行为填到本行）',
    '- [ ] 若走反向代理：升级/隧道是否成功，反代软件与版本，空闲断链行为',
    '- [ ] 传输质量：观测到的延迟、丢包与抖动；与本地回环基线的差异',
    '- [ ] 跨运营商／跨地域：是否有明显退化或重连',
    '- [ ] 跨平台：执行的客户端与服务端操作系统与架构',
    '- [ ] 其他异常：日志中任何非预期错误（附片段）',
    '',
  ].join('\n');
}

// runPreflight 只读预检：可达性、证书与入口端口现状，不启动客户端、不注册代理。
//
// 预检不需要数据面凭据，因此可以在拿到凭证之前先确认环境是否正确；它产生的结论
// 只是"链路是否具备执行条件"，不构成任何验收结论。
async function runPreflight(options) {
  const dateStamp = new Date().toISOString().slice(0, 10);
  const report = { server: options.server, startedAt: new Date().toISOString(), transports: {} };
  for (const transport of options.transports) {
    const entry = {};
    if (transport === 'wss') {
      entry.tcp = await checkTcpReachability(options.host, options.port);
      entry.tls = await checkTlsChain(options.host, options.port, options);
    } else if (transport === 'quic') {
      entry.udp = await checkUdpProbe(options.host, options.port);
      entry.tls = { skipped: true, reason: 'QUIC 的 TLS 在 QUIC 握手内完成，无法用 TCP 握手探测' };
    } else if (transport === 'kcp') {
      entry.udp = await checkUdpProbe(options.host, options.port);
      entry.note = 'KCP 行的官方客户端不发出数据报（见 FR-03 规格 §5.1），预检只确认端口可达性';
    } else {
      entry.tcp = await checkTcpReachability(options.host, options.port);
    }
    if (options.entryPort > 0) {
      // 入口端口若已被监听，可能是上一轮的运行时代理尚未回收，也可能是别的服务。
      entry.entryPortProbe = await checkTcpReachability(options.entryHost, options.entryPort);
    }
    report.transports[transport] = entry;
    process.stdout.write(`预检 ${transport}：${JSON.stringify(entry)}\n`);
  }
  const directory = path.join(root, '.tmp', 'compat-real', dateStamp, 'preflight');
  mkdirSync(directory, { recursive: true });
  writeFileSync(path.join(directory, 'preflight.json'), `${JSON.stringify(report, null, 2)}\n`);
  process.stdout.write(`\n预检结果已写入 ${path.join(directory, 'preflight.json')}\n`);
  process.stdout.write('预检为只读：未启动客户端、未注册任何代理、未使用数据面凭据。\n');
}

// main 依次执行全部「传输 × wire」组合并汇总。
async function main() {
  const options = parseArguments(process.argv.slice(2));
  if (options.help) {
    process.stdout.write(usage);
    return;
  }
  const [host, portText] = options.server.split(':');
  options.host = host;
  options.port = Number(portText);
  if (!options.entryHost) {
    options.entryHost = host;
  }
  if (!Number.isInteger(options.port) || options.port <= 0) {
    throw new Error(`服务端端口非法：${options.server}`);
  }
  if (options.preflightOnly) {
    // 预检不需要凭据与本地回显服务：只读探查目标环境的执行条件。
    await runPreflight(options);
    return;
  }
  if (!options.token) {
    throw new Error('必须提供 --token=<数据面 token>');
  }
  const binary = options.frpc
    ? { version: `override:${options.frpc}`, executable: options.frpc }
    : await ensureFrpcBinary();

  const echoService = await startEchoService(options.localPort);
  const localPort = options.localPort || echoService.address().port;
  const dateStamp = new Date().toISOString().slice(0, 10);
  const results = [];
  try {
    for (const transport of options.transports) {
      for (const wire of options.wires) {
        const caseOptions = { ...options, transport, wire };
        process.stdout.write(`\n=== 实机验收：${transport} × ${wire}（${options.server}）===\n`);
        const result = await runCase(
          caseOptions,
          binary,
          dateStamp,
          localPort,
          echoService,
          results.length > 0,
        );
        results.push(result);
        process.stdout.write(
          `${result.status}：${transport} × ${wire}${result.error ? ` —— ${result.error}` : ''}\n`,
        );
        // 用例之间留出收尾时间：上一用例结束时服务端正在做会话接管与资源回收，
        // 紧接着启动下一用例会让工作连接与回收过程竞争（实测过一次偶发失败）。
        await delay(2000);
      }
    }
  } finally {
    await closeServer(echoService);
  }

  const counts = results.reduce((summary, result) => {
    summary[result.status] = (summary[result.status] ?? 0) + 1;
    return summary;
  }, {});
  process.stdout.write(`\n汇总：${JSON.stringify(counts)}\n`);
  process.stdout.write(
    '报告与证据位于 .tmp/compat-real/<日期>/<传输>-<wire>/，请回填报告中的执行者项目。\n',
  );
  if ((counts.failed ?? 0) > 0) {
    process.exit(1);
  }
}

await main();
