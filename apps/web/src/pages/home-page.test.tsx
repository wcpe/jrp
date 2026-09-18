import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { createHealthFailureHandler, createMalformedHealthHandler } from '@jrp/devmock';
import { mockServer } from '@jrp/devmock/server';

import { App } from '../app';

describe('JRP 首页', () => {
  it('在 MSW 健康响应下展示 jrps 与固定兼容基线', async () => {
    const { container } = render(<App />);

    expect(await screen.findByText('jrps 在线')).toBeInTheDocument();
    expect(screen.getByText('版本 0.1.0-dev · 协议 jrp')).toBeInTheDocument();
    expect(screen.getAllByText('jrp@e1f1d6ab1b591ec9b596f4b1b6a81cd2960c6314')).not.toHaveLength(0);
    expect(container.querySelector('[data-ui="jrp-theme-provider"]')).toBeInTheDocument();
    expect(container.querySelectorAll('[data-ui="telemetry-panel"]')).toHaveLength(5);
  });

  it('健康检查失败后可以手动恢复探测', async () => {
    const user = userEvent.setup();
    mockServer.use(createHealthFailureHandler());
    render(<App />);

    expect(await screen.findByText('jrps 无法连接')).toBeInTheDocument();
    expect(screen.getByText('健康检查返回 HTTP 503')).toBeInTheDocument();
    expect(screen.getByText('链路异常')).toBeInTheDocument();

    mockServer.resetHandlers();
    await user.click(screen.getByRole('button', { name: '重新探测' }));
    expect(await screen.findByText('jrps 在线')).toBeInTheDocument();
  });

  it('健康响应契约异常时展示可理解的错误', async () => {
    mockServer.use(createMalformedHealthHandler());
    render(<App />);

    expect(await screen.findByText('jrps 无法连接')).toBeInTheDocument();
    expect(screen.getByText('健康检查返回了无法识别的数据')).toBeInTheDocument();
  });
});
