import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useNavigate } from '@tanstack/react-router';

import { logout } from '../api/session';

/**
 * 会话条：展示当前管理员并处理登出。
 *
 * 登出是修改类请求，需要 CSRF token；登出后失效会话缓存并回到登录页，
 * 使"登出后受保护端点不可用"在界面上可直接验证。
 */
export function SessionBar({ username, csrfToken }: { username: string; csrfToken: string }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();

  const mutation = useMutation({
    mutationFn: () => logout(csrfToken),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['session'] });
      await navigate({ to: '/login' });
    },
  });

  return (
    <div className="session-bar">
      <span className="session-identity">已登录：{username}</span>
      <button
        className="list-action"
        disabled={mutation.isPending}
        onClick={() => mutation.mutate()}
        type="button"
      >
        {mutation.isPending ? '登出中…' : '登出'}
      </button>
    </div>
  );
}
