import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import {
  createAuthenticatedSessionHandler,
  createCSRFRejectionHandler,
  createDeliveryListHandler,
  createTargetListHandler,
  http,
  HttpResponse,
  mockFailedDelivery,
  mockNotificationTarget,
  mockSession,
} from '@jrp/devmock';
import { mockServer } from '@jrp/devmock/server';

import { App } from '../app';

// 进入首页并等待通知面板就绪的公共前置。
//
// 投递结果 handler 必须一并覆盖：面板现在会查询该端点，缺失会让未处理的
// 请求直接失败、页面多出一个错误提示，把用例的断言引到错误的地方。
async function renderConsole(targets: unknown[] = [], deliveries: unknown[] = []) {
  mockServer.use(
    createAuthenticatedSessionHandler(),
    createTargetListHandler(targets),
    createDeliveryListHandler(deliveries),
  );
  render(<App />);
  await screen.findByRole('heading', { name: '通知目标' });
}

describe('通知目标管理', () => {
  it('展示已配置的目标与摘要', async () => {
    await renderConsole([mockNotificationTarget]);

    expect(await screen.findByText('运维群')).toBeInTheDocument();
    expect(screen.getByText('Webhook hooks.example.com')).toBeInTheDocument();
  });

  it('创建请求携带会话下发的 CSRF token', async () => {
    const user = userEvent.setup();
    const seenTokens: (string | null)[] = [];
    mockServer.use(
      http.post('/api/v1/notification-targets', ({ request }) => {
        seenTokens.push(request.headers.get('X-CSRF-Token'));
        return HttpResponse.json(mockNotificationTarget, { status: 201 });
      }),
    );

    await renderConsole([]);
    await user.type(screen.getByLabelText('名称'), '验收目标');
    await user.type(screen.getByLabelText('Webhook 地址'), 'https://hooks.example.com/hook');
    await user.click(screen.getByRole('button', { name: '创建目标' }));

    expect(seenTokens).toHaveLength(1);
    // 缺失 CSRF 时服务端返回 403；这里断言前端确实带上了会话下发的值。
    expect(seenTokens[0]).toBe(mockSession.csrfToken);
  });

  it('删除请求携带 CSRF token', async () => {
    const user = userEvent.setup();
    const seenTokens: (string | null)[] = [];
    mockServer.use(
      http.delete('/api/v1/notification-targets/:targetId', ({ request }) => {
        seenTokens.push(request.headers.get('X-CSRF-Token'));
        return new HttpResponse(null, { status: 204 });
      }),
    );

    await renderConsole([mockNotificationTarget]);
    await user.click(await screen.findByRole('button', { name: '删除' }));

    expect(seenTokens).toHaveLength(1);
    expect(seenTokens[0]).toBe(mockSession.csrfToken);
  });

  it('CSRF 校验失败时展示服务端中文说明', async () => {
    const user = userEvent.setup();
    mockServer.use(createCSRFRejectionHandler());

    await renderConsole([]);
    await user.type(screen.getByLabelText('名称'), '验收目标');
    await user.type(screen.getByLabelText('Webhook 地址'), 'https://hooks.example.com/hook');
    await user.click(screen.getByRole('button', { name: '创建目标' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('CSRF 校验失败');
  });

  it('展示停止重试的投递记录与脱敏失败摘要', async () => {
    await renderConsole([mockNotificationTarget], [mockFailedDelivery]);

    expect(await screen.findByText('最终失败')).toBeInTheDocument();
    expect(screen.getByText('notification_target_created · 尝试 4 次')).toBeInTheDocument();
    expect(screen.getByText('投递失败：连接被拒绝')).toBeInTheDocument();
  });

  it('没有停止重试的记录时展示空状态', async () => {
    await renderConsole([mockNotificationTarget], []);

    expect(await screen.findByText('暂无停止重试的投递记录。')).toBeInTheDocument();
  });

  it('测试通知请求携带 CSRF token 并展示结果', async () => {
    const user = userEvent.setup();
    const seenTokens: (string | null)[] = [];
    mockServer.use(
      http.post(/\/api\/v1\/notification-targets\/[^/]+:test$/, ({ request }) => {
        seenTokens.push(request.headers.get('X-CSRF-Token'));
        return HttpResponse.json({ message: '测试通知已发送', status: 'ok' });
      }),
    );

    await renderConsole([mockNotificationTarget]);
    await user.click(await screen.findByRole('button', { name: '测试' }));

    expect(await screen.findByText('测试通知已发送')).toBeInTheDocument();
    expect(seenTokens).toHaveLength(1);
    expect(seenTokens[0]).toBe(mockSession.csrfToken);
  });
});
