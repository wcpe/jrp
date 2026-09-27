// 链路夹具：在本地模拟「中间反向代理」与「弱网链路」两类路径。
//
// 用途是把实机批次里最容易失败的环节提前暴露：真实网络下的升级穿透与丢包重连，
// 在本地先过一遍，可区分「JRP 侧问题」与「目标环境问题」。它不能替代实机结论
// ——真实反代软件、公网 NAT 与跨运营商行为仍须用户在目标环境确认。
//
// 运行方式：
//   node scripts/compat/link-fixtures.mjs --mode=http --listen=8080 --target=127.0.0.1:7200
//   node scripts/compat/link-fixtures.mjs --mode=tcp  --listen=8081 --target=127.0.0.1:7200 \
//     --delay-ms=30 --drop-connections-pct=5
//   node scripts/compat/link-fixtures.mjs --mode=udp  --listen=8082 --target=127.0.0.1:7200 \
//     --delay-ms=30 --jitter-ms=20 --drop-datagrams-pct=5 --idle-timeout-ms=5000
// 方法论限制（重要）：本夹具在 TCP 之上转发，因此不能用"丢字节"模拟丢包——那会
// 不可恢复地破坏字节流（真实丢包由 TCP 重传吸收，而这里丢掉的就是已交付的字节），
// 会把夹具自身的破坏误报成被测实现的失败。因此弱网只提供两类忠实手段：
//   --delay-ms              给每个分片加固定延迟（真实存在，且不破坏流）；
//   --drop-connections-pct  按概率丢弃**新建连接**（模拟建链阶段的丢包与失败）。
// UDP 载体（QUIC/KCP）走 --mode=udp：UDP 本身没有重传，因此在那里按概率丢弃
// **数据报**是丢包的忠实模型（--drop-datagrams-pct），由 QUIC/KCP 自己承担恢复。
// 真实丢包率、抖动与 NAT 空闲回收必须由用户在目标环境确认。
import dgram from 'node:dgram';
import http from 'node:http';
import net from 'node:net';
import { setTimeout as delay } from 'node:timers/promises';

// parseArguments 解析命令行参数。
function parseArguments(argv) {
  const options = {
    mode: 'http',
    listen: 0,
    target: '',
    delayMilliseconds: 0,
    jitterMilliseconds: 0,
    // 空闲回收建模：UDP 模式下到期即丢弃该对端的上游映射（下一次数据报会用新的
    // 源端口重建），TCP 模式下到期即断开已建立连接——两者都对应中间设备/NAT 的
    // 空闲表项回收，用来观察对端能否自行恢复。
    idleTimeoutMilliseconds: 0,
    dropConnectionsPercent: 0,
    dropDatagramsPercent: 0,
  };
  for (const argument of argv) {
    const [name, value = ''] = argument.split('=');
    switch (name) {
      case '--mode':
        options.mode = value;
        break;
      case '--listen':
        options.listen = Number(value);
        break;
      case '--target':
        options.target = value;
        break;
      case '--delay-ms':
        options.delayMilliseconds = Number(value);
        break;
      case '--jitter-ms':
        options.jitterMilliseconds = Number(value);
        break;
      case '--idle-timeout-ms':
        options.idleTimeoutMilliseconds = Number(value);
        break;
      case '--drop-connections-pct':
        options.dropConnectionsPercent = Number(value);
        break;
      case '--drop-datagrams-pct':
        options.dropDatagramsPercent = Number(value);
        break;
      default:
        throw new Error(`未知参数：${argument}`);
    }
  }
  if (!['http', 'tcp', 'udp'].includes(options.mode)) {
    throw new Error(`未支持的夹具模式：${options.mode}`);
  }
  if (!Number.isInteger(options.listen) || options.listen <= 0) {
    throw new Error('必须提供 --listen=<本机监听端口>');
  }
  if (!/^[^:]+:\d+$/u.test(options.target)) {
    throw new Error('必须提供 --target=<主机:端口>');
  }
  if (options.dropConnectionsPercent < 0 || options.dropConnectionsPercent > 100) {
    throw new Error('--drop-connections-pct 必须在 0 到 100 之间');
  }
  if (options.dropDatagramsPercent < 0 || options.dropDatagramsPercent > 100) {
    throw new Error('--drop-datagrams-pct 必须在 0 到 100 之间');
  }
  return options;
}

// targetAddress 拆分目标地址。
function targetAddress(options) {
  const [host, port] = options.target.split(':');
  return { host, port: Number(port) };
}

// transferDelay 返回一个分片/数据报的转发延迟：固定延迟加上抖动。
function transferDelay(options) {
  const jitter = options.jitterMilliseconds > 0 ? Math.random() * options.jitterMilliseconds : 0;
  return options.delayMilliseconds + jitter;
}

// shouldDropConnection 按概率决定是否直接丢弃一条新建连接。
function shouldDropConnection(dropConnectionsPercent) {
  return dropConnectionsPercent > 0 && Math.random() * 100 < dropConnectionsPercent;
}

// forward 建立上游连接并按延迟/丢包策略双向转发。
function forward(client, options, stats) {
  const { host, port } = targetAddress(options);
  const upstream = net.connect({ host, port });
  const sockets = [client, upstream];
  let closed = false;
  const closeBoth = () => {
    if (closed) {
      return;
    }
    closed = true;
    for (const socket of sockets) {
      socket.destroy();
    }
  };
  client.on('error', closeBoth);
  upstream.on('error', closeBoth);
  client.on('close', closeBoth);
  upstream.on('close', closeBoth);

  // 空闲回收建模：到期即断开这条已建立连接（对应 NAT/中间设备的空闲表项回收）。
  if (options.idleTimeoutMilliseconds > 0) {
    let timer = setTimeout(() => {
      stats.reclaimedConnections += 1;
      closeBoth();
    }, options.idleTimeoutMilliseconds);
    timer.unref?.();
    const touch = () => {
      clearTimeout(timer);
      timer = setTimeout(() => {
        stats.reclaimedConnections += 1;
        closeBoth();
      }, options.idleTimeoutMilliseconds);
      timer.unref?.();
    };
    client.on('data', touch);
    upstream.on('data', touch);
  }

  const pipeWithPolicy = (from, to) => {
    from.on('data', async (chunk) => {
      if (closed) {
        return;
      }
      const wait = transferDelay(options);
      if (wait > 0) {
        await delay(wait);
      }
      stats.forwardedBytes += chunk.length;
      if (!closed && to.writable) {
        to.write(chunk);
      }
    });
  };
  pipeWithPolicy(client, upstream);
  pipeWithPolicy(upstream, client);
}

// startHttpFixture 启动 HTTP 反向代理夹具，转发 WebSocket 升级请求。
function startHttpFixture(options, stats) {
  const server = http.createServer((request, response) => {
    // 控制入口在 websocket/wss 传输下只接受升级请求：普通 HTTP 请求按上游不可用处理。
    response.writeHead(502, { 'content-type': 'text/plain; charset=utf-8' });
    response.end(`该夹具只转发 WebSocket 升级请求（收到 ${request.method} ${request.url}）`);
  });
  server.on('upgrade', (request, socket, head) => {
    const { host, port } = targetAddress(options);
    const upstream = net.connect({ host, port }, () => {
      // 原样重建请求行与首部：升级请求的 Sec-WebSocket-* 与 Upgrade 头缺一不可。
      const lines = [`${request.method} ${request.url} HTTP/1.1`];
      for (let index = 0; index < request.rawHeaders.length; index += 2) {
        lines.push(`${request.rawHeaders[index]}: ${request.rawHeaders[index + 1]}`);
      }
      upstream.write(`${lines.join('\r\n')}\r\n\r\n`);
      if (head.length > 0) {
        upstream.write(head);
      }
      stats.upgrades += 1;
    });
    let closed = false;
    const closeBoth = () => {
      if (closed) {
        return;
      }
      closed = true;
      socket.destroy();
      upstream.destroy();
    };
    socket.on('error', closeBoth);
    upstream.on('error', closeBoth);
    socket.on('close', closeBoth);
    upstream.on('close', closeBoth);
    socket.on('data', (chunk) => {
      stats.forwardedBytes += chunk.length;
      if (!closed) {
        upstream.write(chunk);
      }
    });
    upstream.on('data', (chunk) => {
      stats.forwardedBytes += chunk.length;
      if (!closed) {
        socket.write(chunk);
      }
    });
  });
  return server;
}

// startUdpFixture 启动 UDP 中继夹具，按延迟/丢包策略转发数据报。
//
// 每个客户端地址映射一条上游套接字：UDP 无连接，QUIC/KCP 都按「对端地址」识别
// 会话，中继必须为每个对端保持独立的上游端点，否则对端地址会在上游侧被混成同一个。
function startUdpFixture(options, stats) {
  const { host, port } = targetAddress(options);
  const server = dgram.createSocket('udp4');
  const upstreams = new Map();

  const shouldDrop = () =>
    options.dropDatagramsPercent > 0 && Math.random() * 100 < options.dropDatagramsPercent;

  server.on('message', async (message, remote) => {
    const key = `${remote.address}:${remote.port}`;
    const now = Date.now();
    let entry = upstreams.get(key);
    // 空闲回收建模：映射到期后丢弃，下一条数据报用新的上游端口重建——
    // 这正是 NAT 空闲表项回收后对端看到的现象（源端口变化），用来观察
    // QUIC/KCP 能否自行恢复会话。
    if (
      entry &&
      options.idleTimeoutMilliseconds > 0 &&
      now - entry.lastActiveAt > options.idleTimeoutMilliseconds
    ) {
      entry.upstream.close();
      upstreams.delete(key);
      stats.reclaimedMappings += 1;
      entry = undefined;
    }
    if (!entry) {
      const upstream = dgram.createSocket('udp4');
      upstream.on('error', () => {});
      upstream.on('message', async (reply) => {
        const wait = transferDelay(options);
        if (wait > 0) {
          await delay(wait);
        }
        entry.lastActiveAt = Date.now();
        if (shouldDrop()) {
          stats.droppedDatagrams += 1;
          return;
        }
        stats.forwardedDatagrams += 1;
        server.send(reply, remote.port, remote.address);
      });
      entry = { upstream, lastActiveAt: now };
      upstreams.set(key, entry);
      stats.sessions += 1;
    }
    if (shouldDrop()) {
      stats.droppedDatagrams += 1;
      return;
    }
    const wait = transferDelay(options);
    if (wait > 0) {
      await delay(wait);
    }
    entry.lastActiveAt = Date.now();
    stats.forwardedDatagrams += 1;
    entry.upstream.send(message, port, host);
  });
  server.closeAll = () => {
    for (const entry of upstreams.values()) {
      entry.upstream.close();
    }
    upstreams.clear();
  };
  return server;
}

// startTcpFixture 启动 TCP 转发夹具，按延迟/丢包策略转发字节。
function startTcpFixture(options, stats, fixture) {
  fixture.on('connection', (client) => {
    stats.connections += 1;
    if (shouldDropConnection(options.dropConnectionsPercent)) {
      stats.droppedConnections += 1;
      client.destroy();
      return;
    }
    forward(client, options, stats);
  });
  return fixture;
}

// main 启动夹具并等待退出信号；退出时打印统计。
async function main() {
  const options = parseArguments(process.argv.slice(2));
  const stats = {
    mode: options.mode,
    listen: options.listen,
    target: options.target,
    connections: 0,
    sessions: 0,
    upgrades: 0,
    forwardedBytes: 0,
    forwardedDatagrams: 0,
    droppedConnections: 0,
    droppedDatagrams: 0,
    reclaimedConnections: 0,
    reclaimedMappings: 0,
  };

  let server;
  if (options.mode === 'http') {
    server = startHttpFixture(options, stats);
  } else if (options.mode === 'udp') {
    server = startUdpFixture(options, stats);
  } else {
    server = startTcpFixture(options, stats, net.createServer());
  }
  await new Promise((resolve, reject) => {
    server.once('error', reject);
    if (options.mode === 'udp') {
      server.bind({ port: options.listen, address: '127.0.0.1' }, resolve);
      return;
    }
    server.listen(options.listen, '127.0.0.1', resolve);
  });
  process.stdout.write(
    `夹具已就绪：${options.mode} 模式，127.0.0.1:${options.listen} → ${options.target}` +
      `${options.delayMilliseconds > 0 ? `，延迟 ${options.delayMilliseconds}ms` : ''}` +
      `${options.dropConnectionsPercent > 0 ? `，新建连接丢弃 ${options.dropConnectionsPercent}%` : ''}` +
      `${options.dropDatagramsPercent > 0 ? `，数据报丢弃 ${options.dropDatagramsPercent}%` : ''}` +
      `${options.jitterMilliseconds > 0 ? `，抖动 ${options.jitterMilliseconds}ms` : ''}` +
      `${options.idleTimeoutMilliseconds > 0 ? `，空闲回收 ${options.idleTimeoutMilliseconds}ms` : ''}\n`,
  );

  const stop = () => {
    process.stdout.write(`\n夹具统计：${JSON.stringify(stats)}\n`);
    if (typeof server.closeAll === 'function') {
      server.closeAll();
    }
    server.close(() => process.exit(0));
    setTimeout(() => process.exit(0), 1000);
  };
  process.on('SIGINT', stop);
  process.on('SIGTERM', stop);
  // 供脚本化调用：按 --duration 自动结束（毫秒）。
  const duration = Number(process.env.JRP_FIXTURE_DURATION_MS ?? 0);
  if (duration > 0) {
    setTimeout(stop, duration);
  }
}

await main();
