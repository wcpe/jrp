// jrps 黑盒宿主：在临时数据目录里初始化并启动一个 jrps 实例，供官方 frpc 接入。
//
// 这里刻意绕开管理面写路径（客户端与代理的 HTTP 端点分属 FR-11/FR-15，尚未交付），
// 在 jrps 未运行时直接准备 SQLite：
//   - clients 行：数据面凭证只认 enrollment_state='active'（pending 无法登录）；
//   - config_revisions 行：启动恢复要求"至少一个 active 凭证 + 至少一个配置版本"，
//     否则应用阶段直接失败。
// 官方 frpc 注册的代理是会话级运行时代理，不需要预置 desired 代理条目。
//
// 控制面监听端口固定 7200（apps/jrps 的 dataPlaneListenPort 常量，没有 CLI 开关），
// 因此同一时刻只允许一个黑盒实例运行。
import { spawn } from 'node:child_process';
import { createHash } from 'node:crypto';
import { existsSync, mkdirSync, openSync } from 'node:fs';
import net from 'node:net';
import path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { setTimeout as delay } from 'node:timers/promises';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');

// ControlPort 与 apps/jrps 的 dataPlaneListenPort 保持一致（store.DefaultControlListenPort）。
export const ControlPort = 7200;

// AdminUsername 是 jrps 唯一管理员用户名（store.AdminUsername）。
const adminUsername = 'admin';

// jrpsExecutable 返回已构建的 jrps 产物路径。
function jrpsExecutable() {
  const executable = path.join(root, 'bin', process.platform === 'win32' ? 'jrps.exe' : 'jrps');
  if (!existsSync(executable)) {
    throw new Error(`未找到 jrps 二进制：${executable}；请先运行 node scripts/build.mjs jrps-fallback`);
  }
  return executable;
}

// digestToken 与 core/server.DigestToken 一致：明文 token 的 SHA-256 十六进制摘要。
function digestToken(token) {
  return createHash('sha256').update(token, 'utf8').digest('hex');
}

// portIsFree 检测端口是否空闲；控制面端口被占用时必须明确失败而不是连到别的实例。
async function portIsFree(port) {
  return new Promise((resolve) => {
    const probe = net.createServer();
    probe.once('error', () => resolve(false));
    probe.once('listening', () => probe.close(() => resolve(true)));
    probe.listen(port, '0.0.0.0');
  });
}

// prepareDatabase 写入黑盒用例所需的客户端与配置版本。
function prepareDatabase(databasePath, clientId, clientToken) {
  const database = new DatabaseSync(databasePath);
  try {
    database
      .prepare(
        `INSERT INTO clients
           (id, name, token_digest, token_compat, enrollment_state, connection_state, desired_revision, active_revision, created_at, updated_at)
         VALUES (?, ?, ?, ?, 'active', 'offline', 0, 0, datetime('now'), datetime('now'))`,
      )
      // token_compat 是官方 frpc 鉴权复算所需的明文（FR-03 §3.4 兼容例外）。
      .run(clientId, `黑盒互操作 ${clientId}`, digestToken(clientToken), clientToken);
    const desired = JSON.stringify({
      schemaVersion: 1,
      controlListen: { host: '0.0.0.0', port: ControlPort },
      proxies: [],
    });
    database
      .prepare(
        `INSERT INTO config_revisions (revision, content, creator, origin, change_summary, created_at)
         VALUES (1, ?, 'compat-script', 'compat-verify', '黑盒互操作矩阵引导版本', datetime('now'))`,
      )
      .run(desired);
  } finally {
    database.close();
  }
}

// runJrpsInit 执行 jrps init（迁移 + 建立管理员凭据），输出落到证据目录。
function runJrpsInit(dataDirectory, adminPassword, logFile) {
  return new Promise((resolve, reject) => {
    const descriptor = openSync(logFile, 'a');
    const child = spawn(jrpsExecutable(), ['init', '--data-dir', dataDirectory], {
      env: { ...process.env, JRP_ADMIN_PASSWORD: adminPassword },
      stdio: ['ignore', descriptor, descriptor],
    });
    child.once('error', reject);
    child.once('exit', (code) => {
      if (code === 0) {
        resolve();
        return;
      }
      reject(new Error(`jrps init 失败，退出码 ${code}，详见 ${logFile}`));
    });
  });
}

// loginSession 建立管理员会话，返回后续请求所需的 Cookie。
async function loginSession(managementUrl, adminPassword) {
  const response = await fetch(`${managementUrl}/api/v1/session`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ username: adminUsername, password: adminPassword }),
  });
  if (!response.ok) {
    throw new Error(`管理员会话建立失败：HTTP ${response.status}`);
  }
  const cookies = response.headers.getSetCookie?.() ?? [];
  if (cookies.length === 0) {
    throw new Error('管理员会话未返回 Cookie');
  }
  return cookies.map((cookie) => cookie.split(';')[0]).join('; ');
}

// startJrpsHost 启动黑盒宿主并返回观测句柄。
export async function startJrpsHost(options) {
  const {
    dataDirectory,
    managementPort,
    evidenceDirectory,
    clientId,
    clientToken,
    adminPassword = 'compat-verify-password',
  } = options;
  mkdirSync(dataDirectory, { recursive: true });
  mkdirSync(evidenceDirectory, { recursive: true });
  if (!(await portIsFree(ControlPort))) {
    throw new Error(`控制面端口 ${ControlPort} 已被占用，无法启动黑盒实例`);
  }

  const initLog = path.join(evidenceDirectory, 'jrps-init.log');
  await runJrpsInit(dataDirectory, adminPassword, initLog);
  prepareDatabase(path.join(dataDirectory, 'jrps.db'), clientId, clientToken);

  // 启动主进程：日志同时落证据目录，失败时可直接排障。
  const processLog = path.join(evidenceDirectory, 'jrps.log');
  const descriptor = openSync(processLog, 'a');
  const child = spawn(
    jrpsExecutable(),
    ['--data-dir', dataDirectory, '--listen', `127.0.0.1:${managementPort}`],
    { env: process.env, stdio: ['ignore', descriptor, descriptor] },
  );
  child.once('error', (error) => {
    throw error;
  });

  const managementUrl = `http://127.0.0.1:${managementPort}`;
  let exited = false;
  child.once('exit', () => {
    exited = true;
  });

  // 就绪等待：/readyz 只在引擎与存储都可用后返回 200。
  const deadline = Date.now() + 20000;
  let ready = false;
  while (Date.now() < deadline && !exited) {
    try {
      const response = await fetch(`${managementUrl}/readyz`);
      if (response.ok) {
        ready = true;
        break;
      }
    } catch {
      // 尚未监听：继续等待。
    }
    await delay(200);
  }
  if (!ready) {
    child.kill();
    throw new Error(`jrps 未在期限内就绪，详见 ${processLog}`);
  }

  const cookie = await loginSession(managementUrl, adminPassword);

  // queryLogs 按过滤条件读取运行日志（权威观测通道）。
  async function queryLogs(params) {
    const search = new URLSearchParams(params);
    const response = await fetch(`${managementUrl}/api/v1/logs?${search.toString()}`, {
      headers: { cookie },
    });
    if (!response.ok) {
      throw new Error(`日志查询失败：HTTP ${response.status}`);
    }
    return response.json();
  }

  // waitForLog 轮询等待满足条件的日志条目；日志异步批量落库，必须给足超时。
  async function waitForLog(params, predicate, timeoutMilliseconds = 15000) {
    const logDeadline = Date.now() + timeoutMilliseconds;
    while (Date.now() < logDeadline) {
      const page = await queryLogs({ ...params, limit: '50' });
      const hit = (page.items ?? []).find(predicate);
      if (hit) {
        return hit;
      }
      await delay(300);
    }
    return undefined;
  }

  async function stop() {
    if (exited) {
      return;
    }
    child.kill();
    const stopDeadline = Date.now() + 5000;
    while (Date.now() < stopDeadline && !exited) {
      await delay(100);
    }
    if (!exited) {
      child.kill('SIGKILL');
    }
  }

  return {
    managementUrl,
    controlPort: ControlPort,
    clientId,
    queryLogs,
    waitForLog,
    stop,
    evidenceDirectory,
  };
}
