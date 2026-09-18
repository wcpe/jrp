import { readdirSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { captureCommand, runCommand } from './command.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const mode = process.argv[2];
const sourceRoots = ['core', 'apps/jrps', 'apps/jrpc'];

if (mode !== '--check' && mode !== '--write') {
  process.stderr.write('用法：node scripts/format-go.mjs --check|--write\n');
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
const batchSize = 100;

try {
  if (mode === '--write') {
    for (let index = 0; index < files.length; index += batchSize) {
      runCommand('gofmt', ['-w', ...files.slice(index, index + batchSize)]);
    }
    process.stdout.write(`已格式化 ${files.length} 个 Go 文件。\n`);
  } else {
    const unformatted = [];
    for (let index = 0; index < files.length; index += batchSize) {
      const output = captureCommand('gofmt', ['-l', ...files.slice(index, index + batchSize)]);
      if (output) {
        unformatted.push(...output.split(/\r?\n/u));
      }
    }
    if (unformatted.length > 0) {
      process.stderr.write(`以下 Go 文件未通过 gofmt：\n${unformatted.join('\n')}\n`);
      process.exit(1);
    }
    process.stdout.write(`Go 格式检查通过，共 ${files.length} 个文件。\n`);
  }
} catch (error) {
  process.stderr.write(`${error.message}\n`);
  process.exit(1);
}
