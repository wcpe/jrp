// FR-03 黑盒互操作矩阵执行器：用官方 frpc 发行二进制对 jrps 做端到端互操作验证。
//
// 每个用例的流程固定（规格 §3.9）：生成 frpc 配置 → 启动 frpc → 等待 jrps 侧事件
// → 访问入口断言 → 清理 → 证据落 .tmp/compat/<日期>/<用例>/。
//
// 运行方式：
//   node scripts/compat/run-matrix.mjs --wire=v1 --case=login
//   node scripts/compat/run-matrix.mjs --wire=v2 --case=all
import { spawn } from 'node:child_process';
import { mkdirSync, writeFileSync } from 'node:fs';
import net from 'node:net';
import path from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { fileURLToPath } from 'node:url';

import { ensureFrpcBinary } from './frpc.mjs';
import { ControlPort, startJrpsHost } from './jrps-host.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');

// clientId/token 是黑盒用例使用的数据面凭证；token 明文只出现在本次运行的临时文件里。
const clientId = 'compat-frpc';
const clientToken = 'compat-frpc-token-0123456789';

// parseArguments 解析命令行参数。
function parseArguments(argv) {
  const options = { wire: 'v1', cases: ['login'], keep: false };
  for (const argument of argv) {
    if (argument.startsWith('--wire=')) {
      options.wire = argument.slice('--wire='.length);
    } else if (argument.startsWith('--case=')) {
      const value = argument.slice('--case='.length);
      options.cases = value === 'all' ? ['login', 'proxy-tcp'] : value.split(',');
    } else if (argument === '--keep') {
      options.keep = true;
    } else {
      throw new Error(`未知参数：${argument}`);
    }
  }
  if (!['v1', 'v2'].includes(options.wire)) {
    throw new Error(`未支持的 wire 版本：${options.wire}`);
  }
  return options;
}

// reservePort 申请一个当前空闲的端口号。
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

// startEchoService 启动本地回显服务，作为被代理的目标。
function startEchoService() {
  const service = net.createServer((socket) => {
    socket.pipe(socket);
  });
  return new Promise((resolve) => {
    service.listen(0, '127.0.0.1', () => resolve(service));
  });
}

// renderFrpcConfig 生成 frpc 配置（官方 TOML 结构）。
function renderFrpcConfig({ wireVersion, token, echoPort, remotePort, logFile, proxyName }) {
  const lines = [
    'serverAddr = "127.0.0.1"',
    `serverPort = ${ControlPort}`,
    // clientID 决定登录消息里的 client_id：服务端按它匹配数据面凭证。
    `clientID = "${clientId}"`,
    'auth.method = "token"',
    `auth.token = "${token}"`,
    `transport.wireProtocol = "${wireVersion}"`,
    // 官方 frpc 默认对控制连接启用 TLS 与 TCPMux 两层传输包装；两者都属于
    // 连接传输范围（FR-05a/FR-05c），本批矩阵只验证裸 TCP 承载下的 wire
    // v1/v2 互操作，故显式关闭两层包装。
    'transport.tls.enable = false',
    'transport.tcpMux = false',
    'log.level = "info"',
    `log.to = "${logFile.replace(/\\/gu, '/')}"`,
    '',
    '[[proxies]]',
    `name = "${proxyName}"`,
    'type = "tcp"',
    'localIP = "127.0.0.1"',
    `localPort = ${echoPort}`,
    `remotePort = ${remotePort}`,
    '',
  ];
  return lines.join('\n');
}

// startFrpc 启动官方 frpc 进程，返回句柄与退出信息。
function startFrpc(executable, configPath) {
  const child = spawn(executable, ['-c', configPath], { stdio: ['ignore', 'pipe', 'pipe'] });
  const state = { exited: false, exitCode: null, stdout: '', stderr: '' };
  child.stdout.on('data', (chunk) => {
    state.stdout += chunk.toString();
  });
  child.stderr.on('data', (chunk) => {
    state.stderr += chunk.toString();
  });
  child.once('exit', (code) => {
    state.exited = true;
    state.exitCode = code;
  });
  return { child, state };
}

// assertEchoThroughProxy 通过 jrps 的代理入口访问回显服务，验证数据面双向可达。
async function assertEchoThroughProxy(port, timeoutMilliseconds = 20000) {
  const deadline = Date.now() + timeoutMilliseconds;
  let lastError;
  while (Date.now() < deadline) {
    try {
      const payload = Buffer.from('黑盒互操作-回显断言-0123456789');
      const echoed = await new Promise((resolve, reject) => {
        const socket = net.connect(port, '127.0.0.1');
        socket.setTimeout(3000);
        socket.once('error', reject);
        socket.once('timeout', () => reject(new Error('入口读取超时')));
        socket.once('connect', () => socket.write(payload));
        const chunks = [];
        socket.on('data', (chunk) => {
          chunks.push(chunk);
          if (Buffer.concat(chunks).length >= payload.length) {
            socket.end();
            resolve(Buffer.concat(chunks));
          }
        });
        socket.once('close', () => resolve(Buffer.concat(chunks)));
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

// runCase 执行单个黑盒用例。
async function runCase({ name, wireVersion, binary, dateStamp }) {
  const caseDirectory = path.join(root, '.tmp', 'compat', dateStamp, `${wireVersion}-${name}`);
  mkdirSync(caseDirectory, { recursive: true });
  const managementPort = await reservePort();
  const dataDirectory = path.join(caseDirectory, 'data');
  const echoService = await startEchoService();
  const echoPort = echoService.address().port;
  const remotePort = await reservePort();

  const result = { case: name, wire: wireVersion, frpc: binary.version, startedAt: new Date().toISOString() };
  let host;
  let frpc;
  try {
    host = await startJrpsHost({
      dataDirectory,
      managementPort,
      evidenceDirectory: caseDirectory,
      clientId,
      clientToken,
    });
    const configPath = path.join(caseDirectory, 'frpc.toml');
    writeFileSync(
      configPath,
      renderFrpcConfig({
        wireVersion,
        token: clientToken,
        echoPort,
        remotePort,
        logFile: path.join(caseDirectory, 'frpc.log'),
        proxyName: 'compat-tcp',
      }),
    );
    frpc = startFrpc(binary.executable, configPath);

    // 断言 1：jrps 侧观测到客户端接入（权威通道是运行日志事件）。
    const connected = await host.waitForLog({ event: 'client-connected' }, (item) => item.clientId === clientId, 20000);
    result.clientConnected = Boolean(connected);
    if (!connected) {
      throw new Error('未在期限内观测到 client-connected 事件');
    }

    if (name === 'proxy-tcp') {
      // 断言 2：代理入口可访问且数据面双向可达。
      await assertEchoThroughProxy(remotePort);
      result.proxyReachable = true;
    }
    result.passed = true;
  } catch (error) {
    result.passed = false;
    result.error = error.message;
  } finally {
    if (frpc) {
      frpc.child.kill();
    }
    if (host) {
      await host.stop();
    }
    echoService.close();
    // frpc 的进程输出与日志一并归档，失败时可直接排障。
    if (frpc) {
      result.frpcExitCode = frpc.state.exitCode;
      writeFileSync(path.join(caseDirectory, 'frpc.stdout.log'), frpc.state.stdout);
      writeFileSync(path.join(caseDirectory, 'frpc.stderr.log'), frpc.state.stderr);
    }
    writeFileSync(path.join(caseDirectory, 'result.json'), `${JSON.stringify(result, null, 2)}\n`);
  }
  return result;
}

// main 执行矩阵并汇总退出码。
async function main() {
  const options = parseArguments(process.argv.slice(2));
  const binary = await ensureFrpcBinary();
  const dateStamp = new Date().toISOString().slice(0, 10);
  const results = [];
  for (const name of options.cases) {
    process.stdout.write(`\n=== 用例 ${name}（wire ${options.wire}，frpc ${binary.version}）===\n`);
    const result = await runCase({ name, wireVersion: options.wire, binary, dateStamp });
    results.push(result);
    process.stdout.write(result.passed ? `通过：${name}\n` : `失败：${name} —— ${result.error}\n`);
  }
  const failed = results.filter((result) => !result.passed);
  process.stdout.write(`\n汇总：${results.length - failed.length}/${results.length} 通过\n`);
  if (failed.length > 0) {
    process.exit(1);
  }
}

await main();
