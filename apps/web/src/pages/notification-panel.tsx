import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import { SignalTag } from '@jrp/ui';

import { createWebhookTarget, deleteTarget, fetchTargets } from '../api/notification';

/**
 * 通知目标管理面板。
 *
 * 这是管理台里第一个真实的修改类操作入口：创建与删除都会触发服务端的 CSRF
 * 校验，前端负责携带 token 并把失败原因如实展示——FR-02 的实机验收正是要
 * 在真实浏览器里走通这条路径。
 */
export function NotificationPanel({ csrfToken }: { csrfToken: string }) {
  const queryClient = useQueryClient();
  const [name, setName] = useState('');
  const [webhookUrl, setWebhookUrl] = useState('');
  const [secret, setSecret] = useState('');

  const targets = useQuery({
    queryFn: fetchTargets,
    queryKey: ['notification-targets'],
  });

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ['notification-targets'] });

  const create = useMutation({
    mutationFn: () => createWebhookTarget({ name, secret, webhookUrl }, csrfToken),
    onSuccess: async () => {
      setName('');
      setWebhookUrl('');
      setSecret('');
      await invalidate();
    },
  });

  const remove = useMutation({
    mutationFn: (id: string) => deleteTarget(id, csrfToken),
    onSuccess: invalidate,
  });

  return (
    <section className="notification-section" aria-labelledby="notification-title">
      <div className="section-heading">
        <div>
          <p className="section-code">管理能力 / 通知目标</p>
          <h2 id="notification-title">通知目标</h2>
        </div>
        <SignalTag tone={targets.data && targets.data.length > 0 ? 'active' : 'muted'}>
          {targets.data ? `${targets.data.length} 个` : '读取中'}
        </SignalTag>
      </div>

      <div className="notification-grid">
        <div className="notification-list">
          {targets.isPending ? <p>正在读取通知目标…</p> : null}
          {targets.isError ? (
            <p className="form-error" role="alert">
              {targets.error.message}
            </p>
          ) : null}
          {targets.data && targets.data.length === 0 ? <p>尚未配置通知目标。</p> : null}
          <ul>
            {(targets.data ?? []).map((target) => (
              <li key={target.id}>
                <div>
                  <strong>{target.name}</strong>
                  <span>{target.summary}</span>
                </div>
                <button
                  className="list-action"
                  disabled={remove.isPending}
                  onClick={() => remove.mutate(target.id)}
                  type="button"
                >
                  删除
                </button>
              </li>
            ))}
          </ul>
          {remove.isError ? (
            <p className="form-error" role="alert">
              {remove.error.message}
            </p>
          ) : null}
        </div>

        <form
          className="notification-form"
          onSubmit={(event) => {
            event.preventDefault();
            create.mutate();
          }}
        >
          <h3>新增 Webhook 目标</h3>
          <label className="login-field">
            <span>名称</span>
            <input
              name="targetName"
              onChange={(event) => setName(event.target.value)}
              required
              type="text"
              value={name}
            />
          </label>
          <label className="login-field">
            <span>Webhook 地址</span>
            <input
              name="webhookUrl"
              onChange={(event) => setWebhookUrl(event.target.value)}
              placeholder="https://example.com/hook"
              required
              type="url"
              value={webhookUrl}
            />
          </label>
          <label className="login-field">
            <span>签名密钥</span>
            <input
              name="secret"
              onChange={(event) => setSecret(event.target.value)}
              type="password"
              value={secret}
            />
          </label>
          <button className="login-submit" disabled={create.isPending} type="submit">
            {create.isPending ? '创建中…' : '创建目标'}
          </button>
          {create.isError ? (
            <p className="form-error" role="alert">
              {create.error.message}
            </p>
          ) : null}
        </form>
      </div>
    </section>
  );
}
