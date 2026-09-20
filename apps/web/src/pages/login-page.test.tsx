import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import {
  createLoginFailureHandler,
  createTargetListHandler,
  createUnauthenticatedSessionHandler,
  http,
  HttpResponse,
  mockSession,
} from '@jrp/devmock';
import { mockServer } from '@jrp/devmock/server';

import { App } from '../app';

describe('管理员登录流程', () => {
  it('未登录时首页跳转到登录页', async () => {
    render(<App />);

    expect(await screen.findByRole('heading', { name: '管理员登录' })).toBeInTheDocument();
  });

  it('登录成功后回到控制台并展示当前管理员', async () => {
    const user = userEvent.setup();
    // 状态化 handler：登录前会话查询必须是 401，否则首页不会跳到登录页。
    let authenticated = false;
    mockServer.use(
      createTargetListHandler([]),
      http.get('/api/v1/session', () =>
        authenticated
          ? HttpResponse.json(mockSession)
          : HttpResponse.json({ detail: '未认证', status: 401 }, { status: 401 }),
      ),
      http.post('/api/v1/session', () => {
        authenticated = true;
        return HttpResponse.json(mockSession);
      }),
    );
    render(<App />);

    await screen.findByRole('heading', { name: '管理员登录' });
    await user.type(screen.getByLabelText('用户名'), 'admin');
    await user.type(screen.getByLabelText('密码'), 'correct-horse-battery');
    await user.click(screen.getByRole('button', { name: '登录' }));

    expect(await screen.findByText('已登录：admin')).toBeInTheDocument();
  });

  it('登录失败展示服务端返回的中文说明', async () => {
    const user = userEvent.setup();
    mockServer.use(createLoginFailureHandler());
    render(<App />);

    await screen.findByRole('heading', { name: '管理员登录' });
    await user.type(screen.getByLabelText('用户名'), 'admin');
    await user.type(screen.getByLabelText('密码'), 'wrong-password');
    await user.click(screen.getByRole('button', { name: '登录' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('用户名或密码错误');
  });

  it('登出后会话失效并回到登录页', async () => {
    const user = userEvent.setup();
    // 状态化 handler：登出后会话查询必须转为 401，否则测不出"会话失效"。
    let authenticated = true;
    mockServer.use(
      createTargetListHandler([]),
      http.get('/api/v1/session', () =>
        authenticated
          ? HttpResponse.json(mockSession)
          : HttpResponse.json({ detail: '未认证', status: 401 }, { status: 401 }),
      ),
      http.delete('/api/v1/session', () => {
        authenticated = false;
        // 与服务端一致：登出返回 200 与中文状态体，而不是 204。
        return HttpResponse.json({ status: '已登出' });
      }),
    );
    render(<App />);

    expect(await screen.findByText('已登录：admin')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '登出' }));

    expect(await screen.findByRole('heading', { name: '管理员登录' })).toBeInTheDocument();
  });

  it('未认证 handler 也可驱动跳转（覆盖默认 401 路径）', async () => {
    mockServer.use(createUnauthenticatedSessionHandler());
    render(<App />);

    expect(await screen.findByRole('heading', { name: '管理员登录' })).toBeInTheDocument();
  });
});
