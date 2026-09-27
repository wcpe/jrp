// 官方 frpc 二进制供应：下载固定版本的官方发行包、校验摘要并解压到忽略的临时目录。
//
// 版本选择依据（可复核）：兼容基线提交 e1f1d6ab1b591ec9b596f4b1b6a81cd2960c6314 的
// 依赖为 github.com/fatedier/golib v0.7.0；该依赖在官方仓库的 v0.69.0..v0.70.0 区间
// 成立，故取区间内最新发行版 v0.70.0 作为黑盒互操作被测对象。
//
// 只信任下面显式登记的资产摘要：未登记的平台直接拒绝运行，避免静默使用未校验的二进制。
// 下载与解压产物只落在 .tmp/compat/ 下，不入库、不纳入 Git（规格 §3.9）。
import { createHash } from 'node:crypto';
import { createWriteStream, existsSync, mkdirSync, readFileSync } from 'node:fs';
import path from 'node:path';
import { Readable } from 'node:stream';
import { pipeline } from 'node:stream/promises';
import { fileURLToPath } from 'node:url';

import { runCommand } from '../command.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');

// FrpcVersion 是被测官方 frpc 版本号。
export const FrpcVersion = '0.70.0';

// 官方发行资产摘要登记表：键为资产文件名，值为 SHA-256。
//
// 摘要取自官方发行附件 `frp_sha256_checksums.txt`（同一文件里已登记的 windows_amd64 摘要
// 与本表逐字符一致，可交叉验证登记方式本身），并且每个平台的归档都实际下载后重算摘要比对过。
const assetDigests = {
  'frp_0.70.0_windows_amd64.zip':
    '8407f83429643aa3fa9590d0c87a46b1ac14660efb96e46c955a4c2802f744b0',
  // linux_amd64 用于在 CI 的 Linux runner 上跑官方客户端互操作（跨平台的运行时一半）。
  'frp_0.70.0_linux_amd64.tar.gz':
    '281cb31e6b915113179c6ebb65b5977a5d9d7fb96f9a70867be83dee3b657721',
};

// 官方发行资产按平台命名，归档格式在 Windows 与其他平台间不同。
function assetName() {
  const platform = { win32: 'windows', linux: 'linux', darwin: 'darwin' }[process.platform];
  const arch = { x64: 'amd64', arm64: 'arm64' }[process.arch];
  if (!platform || !arch) {
    throw new Error(`未支持的黑盒运行平台：${process.platform}/${process.arch}`);
  }
  const suffix = process.platform === 'win32' ? 'zip' : 'tar.gz';
  return { name: `frp_${FrpcVersion}_${platform}_${arch}.${suffix}`, platform, arch };
}

// sha256File 计算文件摘要。
function sha256File(file) {
  const hash = createHash('sha256');
  hash.update(readFileSync(file));
  return hash.digest('hex');
}

// downloadAsset 下载官方发行包到目标路径。
async function downloadAsset(url, target) {
  const response = await fetch(url, { redirect: 'follow' });
  if (!response.ok || !response.body) {
    throw new Error(`下载官方 frpc 发行包失败：HTTP ${response.status}`);
  }
  await pipeline(Readable.fromWeb(response.body), createWriteStream(target));
}

// extractAsset 解压发行包；Windows 用系统自带 tar（bsdtar 支持 zip），其余平台用 tar.gz。
function extractAsset(archive, directory) {
  runCommand('tar', ['-xf', archive, '-C', directory]);
}

// ensureFrpcBinary 准备官方 frpc 可执行文件并返回其位置。
//
// 已下载且摘要匹配时直接复用；摘要不匹配视为污染，报错退出而不是继续运行。
export async function ensureFrpcBinary(workDirectory = path.join(root, '.tmp', 'compat', 'bin')) {
  const { name, platform, arch } = assetName();
  const expectedDigest = assetDigests[name];
  if (!expectedDigest) {
    throw new Error(`官方资产 ${name} 尚未登记摘要，拒绝使用未校验的二进制`);
  }
  mkdirSync(workDirectory, { recursive: true });

  const archive = path.join(workDirectory, name);
  if (!existsSync(archive)) {
    const url = `https://github.com/fatedier/frp/releases/download/v${FrpcVersion}/${name}`;
    process.stdout.write(`下载官方 frpc ${FrpcVersion}：${name}\n`);
    await downloadAsset(url, archive);
  }
  const actualDigest = sha256File(archive);
  if (actualDigest !== expectedDigest) {
    throw new Error(
      `官方 frpc 发行包摘要不匹配（${name}）：期望 ${expectedDigest}，实际 ${actualDigest}`,
    );
  }

  const extracted = path.join(workDirectory, `frp_${FrpcVersion}_${platform}_${arch}`);
  const executableName = process.platform === 'win32' ? 'frpc.exe' : 'frpc';
  const executable = path.join(extracted, executableName);
  if (!existsSync(executable)) {
    extractAsset(archive, workDirectory);
  }
  if (!existsSync(executable)) {
    throw new Error(`解压后未找到 frpc 可执行文件：${executable}`);
  }
  return { version: FrpcVersion, executable, directory: extracted };
}
