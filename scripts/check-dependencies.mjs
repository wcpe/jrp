import { readFileSync, readdirSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { captureCommand } from './command.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const coreDirectory = path.join(root, 'core');
const workspaceEnvironment = { ...process.env, GOWORK: path.join(root, 'go.work') };
const standaloneEnvironment = { ...process.env, GOWORK: 'off' };
const violations = [];

const forbiddenCoreDependencies = [
  ['frp', /^github\.com\/fatedier\/frp(?:\/|$)/u],
  ['Gin', /^github\.com\/gin-gonic\/gin(?:\/|$)/u],
  ['GORM', /^gorm\.io\/(?:gorm|driver)(?:\/|$)/u],
  [
    'SQLite',
    /^(?:modernc\.org\/sqlite|github\.com\/(?:mattn\/go-sqlite3|glebarez\/sqlite|ncruces\/go-sqlite3))(?:\/|$)/u,
  ],
  ['apps module', /^github\.com\/wcpe\/jrp\/apps(?:\/|$)/u],
];

// Core 只承载数据面能力：源码不得读取环境变量、文件或数据库（FR-32 §5 与 architecture-invariants §2）。
const forbiddenCoreSourcePatterns = [
  ['环境变量', /\bos\.(?:Getenv|LookupEnv|Environ)\b/u],
  ['文件读取', /\bos\.(?:Open|OpenFile|ReadFile)\b/u],
  ['数据库', /"database\/sql"/u],
];

function firstFieldLines(output) {
  return output
    .split(/\r?\n/u)
    .map((line) => line.trim().split(/\s+/u)[0])
    .filter(Boolean);
}

function checkCoreEntries(source, entries) {
  for (const entry of entries) {
    for (const [label, pattern] of forbiddenCoreDependencies) {
      if (pattern.test(entry)) {
        violations.push(`Core ${source} 出现禁止依赖 ${label}：${entry}`);
      }
    }
  }
}

function packageDependencies(packageJson) {
  return [
    ...Object.keys(packageJson.dependencies ?? {}),
    ...Object.keys(packageJson.devDependencies ?? {}),
    ...Object.keys(packageJson.peerDependencies ?? {}),
    ...Object.keys(packageJson.optionalDependencies ?? {}),
  ];
}

function collectGoFiles(directory) {
  const files = [];
  for (const entry of readdirSync(directory, { withFileTypes: true })) {
    const entryPath = path.join(directory, entry.name);
    if (entry.isDirectory()) {
      files.push(...collectGoFiles(entryPath));
    } else if (entry.isFile() && entry.name.endsWith('.go')) {
      files.push(entryPath);
    }
  }
  return files;
}

// Core 源码（含测试）不得读取运行环境：构建器只接受宿主在代码中传入的值。
function checkCoreSources(directory) {
  for (const file of collectGoFiles(directory)) {
    const relativePath = path.relative(root, file).split(path.sep).join('/');
    const source = readFileSync(file, 'utf8');
    for (const [label, pattern] of forbiddenCoreSourcePatterns) {
      if (pattern.test(source)) {
        violations.push(`Core 源码出现禁止的${label}调用：${relativePath}`);
      }
    }
  }
}

try {
  const coreGoMod = readFileSync(path.join(coreDirectory, 'go.mod'), 'utf8');
  checkCoreEntries('go.mod', coreGoMod.split(/\s+/u).filter(Boolean));

  const modules = captureCommand('go', ['list', '-m', 'all'], {
    cwd: coreDirectory,
    env: standaloneEnvironment,
  });
  checkCoreEntries('模块图', firstFieldLines(modules));

  const corePackages = captureCommand('go', ['list', '-deps', './...'], {
    cwd: coreDirectory,
    env: standaloneEnvironment,
  });
  checkCoreEntries('包依赖图', firstFieldLines(corePackages));

  checkCoreSources(coreDirectory);

  const jrpsPackages = captureCommand('go', ['list', '-deps', './...'], {
    cwd: path.join(root, 'apps/jrps'),
    env: workspaceEnvironment,
  });
  for (const dependency of firstFieldLines(jrpsPackages)) {
    if (dependency.startsWith('github.com/wcpe/jrp/apps/jrpc')) {
      violations.push(`jrps 禁止导入 jrpc：${dependency}`);
    }
  }

  const jrpcPackages = captureCommand('go', ['list', '-deps', './...'], {
    cwd: path.join(root, 'apps/jrpc'),
    env: workspaceEnvironment,
  });
  for (const dependency of firstFieldLines(jrpcPackages)) {
    if (dependency.startsWith('github.com/wcpe/jrp/apps/jrps')) {
      violations.push(`jrpc 禁止导入 jrps：${dependency}`);
    }
  }

  const packagesDirectory = path.join(root, 'packages');
  for (const entry of readdirSync(packagesDirectory, { withFileTypes: true })) {
    if (!entry.isDirectory()) {
      continue;
    }
    const packagePath = path.join(packagesDirectory, entry.name, 'package.json');
    const packageJson = JSON.parse(readFileSync(packagePath, 'utf8'));
    if (packageDependencies(packageJson).includes('@jrp/web')) {
      violations.push(`${packageJson.name ?? entry.name} 禁止反向依赖 @jrp/web`);
    }
  }

  // 前端第三方依赖版本统一由 pnpm-workspace.yaml 的 catalog 提供，禁止在包内写死版本号。
  const frontendPackagePaths = [
    path.join(root, 'apps/web/package.json'),
    ...readdirSync(packagesDirectory, { withFileTypes: true })
      .filter((entry) => entry.isDirectory())
      .map((entry) => path.join(packagesDirectory, entry.name, 'package.json')),
  ];
  for (const packagePath of frontendPackagePaths) {
    const packageJson = JSON.parse(readFileSync(packagePath, 'utf8'));
    for (const field of ['dependencies', 'devDependencies', 'optionalDependencies']) {
      for (const [name, range] of Object.entries(packageJson[field] ?? {})) {
        if (name.startsWith('@jrp/')) {
          continue;
        }
        if (range !== 'catalog:') {
          violations.push(
            `${packageJson.name ?? packagePath} 的 ${name} 版本应使用 catalog:，实际为 ${range}`,
          );
        }
      }
    }
  }
} catch (error) {
  process.stderr.write(`依赖边界检查无法完成：${error.message}\n`);
  process.exit(1);
}

if (violations.length > 0) {
  process.stderr.write(`依赖边界检查失败：\n- ${violations.join('\n- ')}\n`);
  process.exit(1);
}

process.stdout.write('依赖边界检查通过：Core、jrps、jrpc 与 Web packages 方向均符合约束。\n');
