import { rmSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const targets = [
  'bin',
  'coverage',
  'apps/web/coverage',
  'apps/web/dist',
  'apps/jrps/internal/webui/dist',
];

for (const target of targets) {
  rmSync(path.join(root, target), { force: true, recursive: true });
}

process.stdout.write('构建与覆盖率产物已清理。\n');
