import { existsSync, mkdirSync, readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { runCommand } from './command.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const target = process.argv[2];
const version = readFileSync(path.join(root, 'VERSION'), 'utf8').trim();
const extension = process.platform === 'win32' ? '.exe' : '';

const targets = {
  jrps: {
    directory: 'apps/jrps',
    package: './cmd/jrps',
    versionVariable: 'github.com/wcpe/jrp/apps/jrps/internal/buildinfo.Version',
    tags: ['webui'],
  },
  'jrps-fallback': {
    directory: 'apps/jrps',
    package: './cmd/jrps',
    versionVariable: 'github.com/wcpe/jrp/apps/jrps/internal/buildinfo.Version',
    tags: [],
  },
  jrpc: {
    directory: 'apps/jrpc',
    package: './cmd/jrpc',
    versionVariable: 'github.com/wcpe/jrp/apps/jrpc/internal/buildinfo.Version',
    tags: [],
  },
};

if (!version) {
  process.stderr.write('根 VERSION 不能为空。\n');
  process.exit(1);
}

const configuration = targets[target];
if (!configuration) {
  process.stderr.write('用法：node scripts/build.mjs jrps|jrps-fallback|jrpc\n');
  process.exit(2);
}

if (target === 'jrps' && !existsSync(path.join(root, 'apps/jrps/internal/webui/dist/index.html'))) {
  process.stderr.write('缺少 Web 生产资源，请先运行 task build:web。\n');
  process.exit(1);
}

const binaryName = target === 'jrps-fallback' ? 'jrps' : target;
const outputDirectory = path.join(root, 'bin');
const outputPath = path.join(outputDirectory, `${binaryName}${extension}`);
const argumentsList = [
  'build',
  '-trimpath',
  '-ldflags',
  `-s -w -X ${configuration.versionVariable}=${version}`,
  '-o',
  outputPath,
];

if (configuration.tags.length > 0) {
  argumentsList.push('-tags', configuration.tags.join(','));
}
argumentsList.push(configuration.package);
mkdirSync(outputDirectory, { recursive: true });

try {
  runCommand('go', argumentsList, { cwd: path.join(root, configuration.directory) });
  process.stdout.write(`已构建 ${outputPath}，版本 ${version}。\n`);
} catch (error) {
  process.stderr.write(`${error.message}\n`);
  process.exit(1);
}
