/**
 * 管理会话的 API 客户端。
 *
 * 会话令牌在 HttpOnly Cookie 中，前端拿不到也不需要；CSRF token 由服务端
 * 随登录响应与会话查询下发，修改类请求必须原样带回 `X-CSRF-Token` 头。
 */

export interface SessionSnapshot {
  username: string;
  expiresAt: string;
  csrfToken: string;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null;
}

function isSessionSnapshot(value: unknown): value is SessionSnapshot {
  return (
    isRecord(value) &&
    typeof value.username === 'string' &&
    typeof value.expiresAt === 'string' &&
    typeof value.csrfToken === 'string'
  );
}

/** 从问题详情中提取可读的中文说明；解析失败时退回状态码描述。 */
async function problemDetail(response: Response): Promise<string> {
  try {
    const payload: unknown = await response.json();
    if (isRecord(payload) && typeof payload.detail === 'string' && payload.detail !== '') {
      return payload.detail;
    }
  } catch {
    // 落回状态码描述：响应体不是 JSON 时不该掩盖原始失败。
  }
  return `请求失败（HTTP ${response.status}）`;
}

/** 登录；失败时抛出带中文说明的错误。 */
export async function login(input: {
  username: string;
  password: string;
}): Promise<SessionSnapshot> {
  const response = await fetch('/api/v1/session', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  });
  if (!response.ok) {
    throw new Error(await problemDetail(response));
  }
  const payload: unknown = await response.json();
  if (!isSessionSnapshot(payload)) {
    throw new Error('登录响应缺少会话字段');
  }
  return payload;
}

/**
 * 查询当前会话；未登录返回 null 而不是抛错。
 *
 * 未登录是首页的正常状态（据此跳登录页），不该与"查询失败"混为一谈。
 */
export async function fetchSession(): Promise<SessionSnapshot | null> {
  const response = await fetch('/api/v1/session');
  if (response.status === 401) {
    return null;
  }
  if (!response.ok) {
    throw new Error(await problemDetail(response));
  }
  const payload: unknown = await response.json();
  if (!isSessionSnapshot(payload)) {
    throw new Error('会话响应缺少会话字段');
  }
  return payload;
}

/** 登出；需要 CSRF token。 */
export async function logout(csrfToken: string): Promise<void> {
  const response = await fetch('/api/v1/session', {
    method: 'DELETE',
    headers: { 'X-CSRF-Token': csrfToken },
  });
  if (!response.ok && response.status !== 401) {
    throw new Error(await problemDetail(response));
  }
}

export { problemDetail };
