import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useNavigate } from '@tanstack/react-router';
import { useState } from 'react';

import { login } from '../api/session';

/**
 * 管理员登录页。
 *
 * 会话令牌由服务端写入 HttpOnly Cookie，本页只负责提交凭据与展示失败原因；
 * 成功后把会话写入缓存并回到控制台首页。
 */
export function LoginPage() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');

  const mutation = useMutation({
    mutationFn: () => login({ username, password }),
    onSuccess: async (session) => {
      // 直接把登录响应写入会话缓存：若只做失效处理，首页会先用旧的空会话
      // 渲染一次，把刚登录成功的管理员又弹回登录页。
      queryClient.setQueryData(['session'], session);
      await navigate({ to: '/' });
    },
  });

  return (
    <div className="console-shell">
      <div className="signal-noise" aria-hidden="true" />
      <header className="console-header">
        <div className="brand-lockup">
          <span className="brand-mark" aria-hidden="true">
            J
          </span>
          <div>
            <strong>JRP</strong>
            <span>网络信号台</span>
          </div>
        </div>
        <div className="header-channel">
          <span className="channel-light" aria-hidden="true" />
          控制通道 / 管理员
        </div>
      </header>
      <main>
        <section className="login-section" aria-labelledby="login-title">
          <p className="section-code">控制界面 / 登录</p>
          <h1 id="login-title">管理员登录</h1>
          <form
            className="login-form"
            onSubmit={(event) => {
              event.preventDefault();
              mutation.mutate();
            }}
          >
            <label className="login-field">
              <span>用户名</span>
              <input
                autoComplete="username"
                name="username"
                onChange={(event) => setUsername(event.target.value)}
                required
                type="text"
                value={username}
              />
            </label>
            <label className="login-field">
              <span>密码</span>
              <input
                autoComplete="current-password"
                name="password"
                onChange={(event) => setPassword(event.target.value)}
                required
                type="password"
                value={password}
              />
            </label>
            <button className="login-submit" disabled={mutation.isPending} type="submit">
              {mutation.isPending ? '登录中…' : '登录'}
            </button>
            {mutation.isError ? (
              <p className="login-error" role="alert">
                {mutation.error.message}
              </p>
            ) : null}
          </form>
        </section>
      </main>
      <footer>
        <span>JRP / 工业控制界面</span>
        <span>Core 边界：独立</span>
      </footer>
    </div>
  );
}
