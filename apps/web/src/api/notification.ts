/**
 * 通知目标的 API 客户端。
 *
 * 修改类请求统一携带 `X-CSRF-Token`：服务端对缺失或错误的 token 返回 403，
 * 前端负责把中文说明如实展示给管理员。
 */

import { problemDetail } from './session';

export interface NotificationTarget {
  id: string;
  name: string;
  type: string;
  enabled: boolean;
  maskedSecret: string;
  summary: string;
  webhookUrl: string;
  smtpHost: string;
  smtpPort: number;
  smtpFrom: string;
  smtpTo: string[];
  smtpSecurity: string;
  createdAt: string;
  updatedAt: string;
}

export interface WebhookTargetInput {
  name: string;
  webhookUrl: string;
  secret: string;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null;
}

function isTarget(value: unknown): value is NotificationTarget {
  return (
    isRecord(value) &&
    typeof value.id === 'string' &&
    typeof value.name === 'string' &&
    typeof value.type === 'string' &&
    typeof value.enabled === 'boolean' &&
    typeof value.summary === 'string'
  );
}

/** 读取全部通知目标。 */
export async function fetchTargets(): Promise<NotificationTarget[]> {
  const response = await fetch('/api/v1/notification-targets');
  if (!response.ok) {
    throw new Error(await problemDetail(response));
  }
  const payload: unknown = await response.json();
  // 列表由服务端以 `{items:[...]}` 包裹（jrps 的 notificationTargetsResponse），
  // 不是裸数组；按裸数组解析会在实机上整表读不出来。
  if (!isRecord(payload) || !Array.isArray(payload.items) || !payload.items.every(isTarget)) {
    throw new Error('通知目标列表返回了无法识别的数据');
  }
  return payload.items;
}

/** 创建一个 Webhook 类型的目标。 */
export async function createWebhookTarget(
  input: WebhookTargetInput,
  csrfToken: string,
): Promise<NotificationTarget> {
  const response = await fetch('/api/v1/notification-targets', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrfToken },
    body: JSON.stringify({
      name: input.name,
      type: 'webhook',
      enabled: true,
      webhookUrl: input.webhookUrl,
      secret: input.secret,
    }),
  });
  if (!response.ok) {
    throw new Error(await problemDetail(response));
  }
  const payload: unknown = await response.json();
  if (!isTarget(payload)) {
    throw new Error('创建响应缺少目标字段');
  }
  return payload;
}

/** 删除一个目标。 */
export async function deleteTarget(id: string, csrfToken: string): Promise<void> {
  const response = await fetch(`/api/v1/notification-targets/${encodeURIComponent(id)}`, {
    method: 'DELETE',
    headers: { 'X-CSRF-Token': csrfToken },
  });
  if (!response.ok) {
    throw new Error(await problemDetail(response));
  }
}
