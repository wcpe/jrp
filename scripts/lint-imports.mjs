import { readdirSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { captureCommand, runCommand } from './command.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const mode = process.argv[2];
const sourceRoots = ['core', 'apps/jrps', 'apps/jrpc'];

if (mode !== '--check' && mode !== '--write') {
  process.stderr.write('用法：node scripts/lint-imports.mjs --check|--write\n');
  process.exit(2);
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

const files = sourceRoots.flatMap((directory) => collectGoFiles(path.join(root, directory))).sort();

// goimports 除格式化外还负责导入分组与增删，gofmt 不覆盖这部分：
// gofmt 只管排版，多出来的导入、漏掉的导入、分组顺序都要靠 goimports 发现。
// 两者都在根 Taskfile 内执行，CI 与本地走同一入口。
const batchSize = 100;

try {
  if (mode === '--write') {
    for (let index = 0; index < files.length; index += batchSize) {
      // goimports -w 与 gofmt -w 的差异仅在于导入处理，重复执行是幂等的。
      runCommand('goimports', ['-w', ...files.slice(index, index + batchSize)]);
    }
    process.stdout.write(`已按 goimports 整理 ${files.length} 个 Go 文件的导入。\n`);
  } else {
    const unformatted = [];
    for (let index = 0; index < files.length; index += batchSize) {
      // goimports -l 在存在待改文件时仍返回 0，只能靠输出判断。
      const output = captureCommand('goimports', ['-l', ...files.slice(index, index + batchSize)]);
      if (output) {
        unformatted.push(...output.split(/\r?\n/u).filter((line) => line.length > 0));
      }
    }
    if (unformatted.length > 0) {
      process.stderr.write(
        `以下 Go 文件的导入未通过 goimports，用 task fmt:go 修正：\n${unformatted.join('\n')}\n`,
      );
      process.exit(1);
    }
    process.stdout.write(`Go 导入检查通过，共 ${files.length} 个文件。\n`);
  }
} catch (error) {
  process.stderr.write(`${error.message}\n`);
  process.exit(1);
}
