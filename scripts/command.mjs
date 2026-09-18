import { spawnSync } from 'node:child_process';

function executableName(command) {
  if (process.platform === 'win32' && command === 'pnpm') {
    return `${command}.cmd`;
  }
  return command;
}

function execute(command, args, options, stdio) {
  const result = spawnSync(executableName(command), args, {
    cwd: options.cwd,
    env: options.env ?? process.env,
    encoding: 'utf8',
    stdio,
  });

  if (result.error) {
    throw new Error(`无法执行命令 ${command}：${result.error.message}`);
  }
  if (result.status !== 0) {
    if (stdio === 'pipe') {
      process.stdout.write(result.stdout ?? '');
      process.stderr.write(result.stderr ?? '');
    }
    throw new Error(`命令 ${command} 执行失败，退出码 ${result.status}`);
  }
  return result;
}

export function runCommand(command, args, options = {}) {
  execute(command, args, options, 'inherit');
}

export function captureCommand(command, args, options = {}) {
  return execute(command, args, options, 'pipe').stdout.trim();
}
