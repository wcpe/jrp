// NAT 探测工具：把实机验收里「NAT 类型与端口映射方式」这条人工回填项变成可测项。
//
// 为什么需要它：实机批次的报告要求执行者填写客户端侧 NAT 的类型与映射方式，而这条
// 只能靠"看起来像"来猜：普通家用路由器、企业防火墙、云厂商 NAT 网关的行为差别很大，
// 而它直接决定 UDP 载体（QUIC）在空闲后能否继续收到服务端流量。
//
// 依据 RFC 5389（STUN）只做最必要的一步：从同一个本地 UDP 端口向同一主机的**不同 IP**
// 各发一个 binding 请求（同一域名常解析到多个 IP，等价于两台 STUN 服务器）：
//   · 两个端点观测到相同的外部端口 → 端点无关映射（cone，端口映射与目的地无关）
//   · 观测到不同外部端口           → 地址相关映射（对称 NAT，每个目的地一个映射）
// 地址相关映射意味着映射数量随目的地增长，空闲回收更快，弱网验收的结论必须按它评估。
//
// 运行方式：
//   node scripts/compat/nat-probe.mjs --stun=stun.example.com:3478
//   node scripts/compat/nat-probe.mjs --serve=127.0.0.1:34780        # 内置应答器，离线自测用
//   node scripts/compat/nat-probe.mjs --stun=127.0.0.1:34780,127.0.0.2:34780   # 两个目的地用于判定映射行为
//
// 该模式只发 UDP binding 请求，不注册任何代理、不需要凭据；默认不联网——必须显式给出 --stun。
import dgram from 'node:dgram';
import { mkdirSync, writeFileSync } from 'node:fs';
import dns from 'node:dns/promises';
import path from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');

// STUN 常量（RFC 5389 §6、§15）。
const bindingRequest = 0x0001;
const bindingSuccess = 0x0101;
const magicCookie = 0x2112a442;
const attributeMappedAddress = 0x0001;
const attributeXorMappedAddress = 0x0020;

// parseArguments 解析命令行参数。
function parseArguments(argv) {
  const options = { stuns: [], serve: '', timeoutMilliseconds: 4000 };
  for (const argument of argv) {
    const [name, value = ''] = argument.split('=');
    switch (name) {
      case '--stun':
        // 可重复给出，也可用逗号分隔：判定映射行为需要至少两个不同的目的地。
        options.stuns.push(...value.split(',').filter(Boolean));
        break;
      case '--serve':
        options.serve = value || '127.0.0.1:34780';
        break;
      case '--timeout':
        options.timeoutMilliseconds = Number(value);
        break;
      default:
        throw new Error(`未知参数：${argument}`);
    }
  }
  if (options.stuns.length === 0 && !options.serve) {
    throw new Error('必须给出 --stun=<主机:端口>（或用 --serve 启动内置应答器做离线自测）');
  }
  if (!options.stuns.every((item) => item.includes(':'))) {
    throw new Error(`--stun 必须形如 <主机>:<端口>，多个用逗号分隔：${options.stuns.join(',')}`);
  }
  if (options.serve && !options.serve.includes(':')) {
    throw new Error(`--serve 必须形如 <主机>:<端口>：${options.serve}`);
  }
  if (!Number.isInteger(options.timeoutMilliseconds) || options.timeoutMilliseconds < 500) {
    throw new Error('--timeout 必须是不小于 500 的整数（毫秒）');
  }
  return options;
}

// buildBindingRequest 生成一个最小 binding 请求（12 字节头 + 20 字节事务 ID）。
function buildBindingRequest() {
  const buffer = Buffer.alloc(20);
  buffer.writeUInt16BE(bindingRequest, 0);
  buffer.writeUInt16BE(0, 2);
  buffer.writeUInt32BE(magicCookie, 4);
  for (let index = 8; index < 20; index += 1) {
    buffer[index] = Math.floor(Math.random() * 256);
  }
  return buffer;
}

// parseBindingResponse 从响应中取出映射地址（优先 XOR-MAPPED-ADDRESS）。
function parseBindingResponse(message) {
  if (message.length < 20 || message.readUInt16BE(0) !== bindingSuccess) {
    return undefined;
  }
  let offset = 20;
  while (offset + 4 <= message.length) {
    const type = message.readUInt16BE(offset);
    const length = message.readUInt16BE(offset + 2);
    const valueStart = offset + 4;
    if (valueStart + length > message.length) {
      return undefined;
    }
    if (type === attributeXorMappedAddress && length >= 8) {
      const family = message[valueStart + 1];
      const port = message.readUInt16BE(valueStart + 2) ^ (magicCookie >>> 16);
      if (family === 0x01) {
        const address = Buffer.from(message.subarray(valueStart + 4, valueStart + 8));
        // XOR 结果在 JS 里是有符号 32 位整数，写回前必须转回无符号。
        address.writeUInt32BE((address.readUInt32BE(0) ^ magicCookie) >>> 0, 0);
        return { address: address.join('.'), port };
      }
    }
    if (type === attributeMappedAddress && length >= 8) {
      const family = message[valueStart + 1];
      const port = message.readUInt16BE(valueStart + 2);
      if (family === 0x01) {
        return { address: message.subarray(valueStart + 4, valueStart + 8).join('.'), port };
      }
    }
    offset = valueStart + length + ((4 - (length % 4)) % 4);
  }
  return undefined;
}

// queryEndpoint 从指定 socket 向一个端点发 binding 请求并等待响应。
function queryEndpoint(socket, endpoint, timeoutMilliseconds) {
  return new Promise((resolve) => {
    const request = buildBindingRequest();
    const timer = setTimeout(() => {
      socket.removeAllListeners('message');
      resolve({ endpoint, responded: false });
    }, timeoutMilliseconds);
    socket.once('message', (message, remote) => {
      clearTimeout(timer);
      socket.removeAllListeners('message');
      const mapped = parseBindingResponse(message);
      resolve({ endpoint, responded: true, remote: `${remote.address}:${remote.port}`, mapped });
    });
    socket.send(request, endpoint.port, endpoint.address, (error) => {
      if (error) {
        clearTimeout(timer);
        socket.removeAllListeners('message');
        resolve({ endpoint, responded: false, reason: error.message });
      }
    });
  });
}

// resolveEndpoints 把 STUN 端点解析为 IP 列表（映射行为判定需要至少两个不同目的地）。
async function resolveEndpoints(stuns) {
  const endpoints = [];
  for (const stun of stuns) {
    const [host, portText] = stun.split(':');
    const addresses = await dns.lookup(host, { all: true, family: 4 });
    for (const item of addresses) {
      endpoints.push({ address: item.address, port: Number(portText), host });
    }
  }
  const seen = new Set();
  return endpoints.filter((item) => {
    const key = `${item.address}:${item.port}`;
    if (seen.has(key)) {
      return false;
    }
    seen.add(key);
    return true;
  });
}

// serveOnce 启动内置 STUN 应答器：它回送调用方的源地址，让工具可离线自测。
async function serve(address, port) {
  const socket = dgram.createSocket('udp4');
  await new Promise((resolve, reject) => {
    socket.once('error', reject);
    socket.bind(port, address, resolve);
  });
  process.stdout.write(`内置 STUN 应答器监听 ${address}:${port}（Ctrl-C 退出）\n`);
  socket.on('message', (message, remote) => {
    if (message.length < 20 || message.readUInt16BE(0) !== bindingRequest) {
      return;
    }
    const transactionId = message.subarray(8, 20);
    const value = Buffer.alloc(8);
    value[0] = 0;
    value[1] = 0x01;
    value.writeUInt16BE(remote.port ^ (magicCookie >>> 16), 2);
    const addressBytes = Buffer.from(remote.address.split('.').map(Number));
    // 同解析侧：XOR 结果需转回无符号 32 位。
    addressBytes.writeUInt32BE((addressBytes.readUInt32BE(0) ^ magicCookie) >>> 0, 0);
    addressBytes.copy(value, 4);
    const header = Buffer.alloc(20);
    header.writeUInt16BE(bindingSuccess, 0);
    header.writeUInt16BE(8, 2);
    header.writeUInt32BE(magicCookie, 4);
    transactionId.copy(header, 8);
    const attribute = Buffer.alloc(4);
    attribute.writeUInt16BE(attributeXorMappedAddress, 0);
    attribute.writeUInt16BE(8, 2);
    socket.send(Buffer.concat([header, attribute, value]), remote.port, remote.address);
  });
  await new Promise(() => {});
}

// main 执行探测并输出结论。
async function main() {
  const options = parseArguments(process.argv.slice(2));
  if (options.serve) {
    const [address, port] = options.serve.split(':');
    await serve(address, Number(port));
    return;
  }
  const [host, portText] = options.stuns[0].split(':');
  const endpoints = await resolveEndpoints(options.stuns);
  if (endpoints.length === 0) {
    throw new Error(`未解析到 IPv4 地址：${host}`);
  }
  const socket = dgram.createSocket('udp4');
  await new Promise((resolve, reject) => {
    socket.once('error', reject);
    socket.bind(0, '0.0.0.0', resolve);
  });
  const localPort = socket.address().port;
  const results = [];
  for (const endpoint of endpoints.slice(0, 4)) {
    results.push(await queryEndpoint(socket, endpoint, options.timeoutMilliseconds));
    await delay(150);
  }
  socket.close();

  const answered = results.filter((item) => item.responded && item.mapped);
  const mapped = answered.map((item) => item.mapped);
  const externalAddresses = [...new Set(mapped.map((item) => item.address))];
  const externalPorts = [...new Set(mapped.map((item) => item.port))];
  let mappingBehaviour = '无法判定';
  if (answered.length >= 2) {
    mappingBehaviour =
      externalPorts.length === 1
        ? '端点无关映射（cone）：不同目的地复用同一外部端口'
        : '地址相关映射（对称 NAT）：每个目的地各自映射，空闲回收更快';
  }

  const summary = {
    stun: options.stuns.join(','),
    localPort,
    answeredEndpoints: answered.length,
    queriedEndpoints: results.length,
    externalAddresses,
    externalPorts,
    mappingBehaviour,
    details: results,
  };
  const directory = path.join(root, '.tmp', 'compat', new Date().toISOString().slice(0, 10));
  mkdirSync(directory, { recursive: true });
  const evidencePath = path.join(directory, 'nat-probe.json');
  writeFileSync(evidencePath, `${JSON.stringify(summary, null, 2)}\n`);

  process.stdout.write(`本地端口：${localPort}；应答端点：${answered.length}/${results.length}\n`);
  process.stdout.write(`外部地址：${externalAddresses.join(', ') || '（无应答）'}\n`);
  process.stdout.write(`外部端口：${externalPorts.join(', ') || '（无应答）'}\n`);
  process.stdout.write(`映射行为：${mappingBehaviour}\n`);
  if (answered.length < 2) {
    process.stdout.write(
      '提示：只在同一主机的多个 IP 上探测才能判定映射行为；请换用解析到多个 IP 的 STUN 主机。\n',
    );
  }
  process.stdout.write(`结论已写入 ${evidencePath}\n`);
}

await main();
