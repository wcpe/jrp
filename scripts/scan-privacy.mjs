// 隐私扫描门禁：阻止真实主机地址、凭据与其它环境私密信息进入版本库。
//
// 为什么需要它：实机验收会把公网服务器地址、数据面 token、管理员口令这类
// 环境信息带到工作区，靠人记得不写进文档是不牢靠的——本仓库就发生过一次
// （公网服务器地址随验收结论进了 CHANGELOG 与规格，只能靠改写历史清除）。
// 文档只需要拓扑（服务端是否原生公网、客户端是否位于 NAT 后）就能支撑结论，
// 具体地址不提供额外信息量，却会泄露一台可定位的主机。
//
// 扫描对象是**已跟踪文件**（未跟踪的临时产物由 .gitignore 兜底），因此它
// 既能拦住"忘了删"，也能在 CI 上拦住"已经提交"。
//
// 用法：
//   node scripts/scan-privacy.mjs              # 检查，命中即非 0 退出
//   node scripts/scan-privacy.mjs --baseline   # 忽略既有命中（仅用于迁移期）
import { readFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

// trackedFiles 列出版本库当前跟踪的文件。
function trackedFiles() {
  const result = spawnSync('git', ['ls-files', '-z'], { cwd: root, encoding: 'utf8' });
  if (result.status !== 0) {
    throw new Error('读取已跟踪文件列表失败');
  }
  return result.stdout.split('\0').filter(Boolean);
}

// reservedPrefixes 是私有、回环、链路本地与文档保留段（RFC 1918 / 5737 等）。
// 这些地址在代码与测试中大量出现且不含隐私，必须放行，否则门禁全是噪音。
const reservedPrefixes = [
  '0.',
  '10.',
  '127.',
  '100.64.', // 运营商级 NAT（CGNAT，RFC 6598）
  '169.254.', // 链路本地
  '192.0.2.', // TEST-NET-1
  '198.51.100.', // TEST-NET-2
  '203.0.113.', // TEST-NET-3
  '224.',
  '240.',
  '255.',
];

// isReserved 判断地址是否落在保留段，或属于私有 172.16/12 与 192.168/16。
function isReserved(address) {
  if (reservedPrefixes.some((prefix) => address.startsWith(prefix))) {
    return true;
  }
  const octets = address.split('.').map(Number);
  if (octets[0] === 172 && octets[1] >= 16 && octets[1] <= 31) {
    return true;
  }
  if (octets[0] === 192 && octets[1] === 168) {
    return true;
  }
  // 192.0.0.0/24（IETF 协议分配）与 198.18.0.0/15（基准测试）在代码里是网段常量。
  if (octets[0] === 192 && octets[1] === 0 && octets[2] === 0) {
    return true;
  }
  if (octets[0] === 198 && octets[1] === 18) {
    return true;
  }
  return false;
}

// isTestFile 判断是否为测试文件：测试夹具中的地址与固定常量不含隐私，
// 且必须保持可读（脱敏功能的用例本身就要用"像私钥的字符串"做输入），
// 一律放行——否则门禁全是噪音，反而没人看。
function isTestFile(file) {
  return file.endsWith('_test.go') || file.includes('/test/') || file.includes('.test.');
}

// rules 是全部检查规则；每条给出命中说明，便于执行者一眼看懂该改哪里。
const rules = [
  {
    name: '公网 IPv4 地址',
    pattern: /(?<![\d.])((?:\d{1,3}\.){3}\d{1,3})(?![\d.])/gu,
    // 网段常量与文档保留段放行（见 isReserved）；测试夹具同样放行。
    accept: (match, file) => isReserved(match[1]) || isTestFile(file),
    advice:
      '写入具体主机地址会暴露可定位的机器；改用拓扑描述（如“原生公网地址的 Linux 主机”“位于 NAT 后的客户端”）。',
  },
  {
    name: '私钥或证书私钥块',
    pattern: /-----BEGIN [A-Z ]*PRIVATE KEY-----/gu,
    // 脱敏功能的测试用例需要"像私钥的字符串"作为输入，属夹具而非真实密钥。
    skipPath: isTestFile,
    advice: '私钥不得入库：证书与密钥只留在部署环境与忽略的临时目录。',
  },
  {
    name: 'SSH 私钥文件',
    pattern: /^[\w./-]*(id_rsa|id_ed25519|id_ecdsa|jp_root|.*\.pem)$/gu,
    advice: 'SSH 私钥与 PEM 不得入库。',
  },
  {
    name: '疑似 token 字面量',
    pattern:
      /(?:token|secret|password|passwd|apiKey|api_key|credential)\s*[:=]\s*['"][A-Za-z0-9_\-]{20,}['"]/giu,
    // 别名赋值（clientToken, adminPassword = …）在兼容性脚本里是运行期夹具，
    // 值固定且一眼可辨，与"真实凭据"无关；生产配置经环境变量或部署注入。
    skipPath: (file) => isTestFile(file) || file.startsWith('scripts/compat/'),
    advice: '凭据明文不得入库；测试请用一眼可辨的夹具常量，生产配置经环境变量或部署注入。',
  },
];

// main 扫描全部已跟踪文件并汇总命中。
function main() {
  const baselineMode = process.argv.includes('--baseline');
  const files = trackedFiles();
  const findings = [];

  for (const file of files) {
    // 扫工作区文件而不是 git show HEAD:<file>：只扫已提交内容的话，刚写进文档
    // 还没提交的地址会漏过去，而那正是最该拦住的时点（提交后再清就要改写历史）。
    let content;
    try {
      content = readFileSync(path.join(root, file), 'utf8');
    } catch {
      continue;
    }
    for (const rule of rules) {
      if (rule.skipPath && rule.skipPath(file)) {
        continue;
      }
      rule.pattern.lastIndex = 0;
      let match;
      while ((match = rule.pattern.exec(content)) !== null) {
        if (rule.accept && rule.accept(match, file)) {
          continue;
        }
        const offset = match.index;
        const line = content.slice(0, offset).split('\n').length;
        findings.push({
          file,
          line,
          rule: rule.name,
          hit: match[0].slice(0, 80),
          advice: rule.advice,
        });
        break; // 每个文件每条规则只报一次，避免刷屏
      }
    }
  }

  if (findings.length === 0) {
    process.stdout.write('隐私扫描通过：未发现公网地址、私钥或凭据字面量。\n');
    return;
  }
  process.stdout.write(`隐私扫描命中 ${findings.length} 处：\n\n`);
  for (const finding of findings) {
    process.stdout.write(`  ${finding.file}:${finding.line}  [${finding.rule}]\n`);
    process.stdout.write(`    命中：${finding.hit}\n`);
    process.stdout.write(`    处置：${finding.advice}\n\n`);
  }
  if (baselineMode) {
    process.stdout.write('--baseline：仅报告，不阻断（迁移期专用）。\n');
    return;
  }
  process.exit(1);
}

main();
