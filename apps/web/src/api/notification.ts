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

/** 发送一条测试通知；服务端投递失败时返回 502，此处如实抛错。 */
export async function sendTestNotification(id: string, csrfToken: string): Promise<string> {
  const response = await fetch(`/api/v1/notification-targets/${encodeURIComponent(id)}:test`, {
    method: 'POST',
    headers: { 'X-CSRF-Token': csrfToken },
  });
  if (!response.ok) {
    throw new Error(await problemDetail(response));
  }
  const payload: unknown = await response.json();
  if (isRecord(payload) && typeof payload.message === 'string') {
    return payload.message;
  }
  return '测试通知已发送';
}

export interface NotificationDelivery {
  id: number;
  eventId: string;
  targetId: string;
  eventType: string;
  status: string;
  attempts: number;
  lastError: string;
  nextAttemptAt: string;
  stoppedAt: string;
  createdAt: string;
  updatedAt: string;
}

function isDelivery(value: unknown): value is NotificationDelivery {
  return (
    isRecord(value) &&
    typeof value.id === 'number' &&
    typeof value.eventId === 'string' &&
    typeof value.status === 'string' &&
    typeof value.attempts === 'number'
  );
}

/**
 * 读取投递结果，只取已停止重试的记录。
 *
 * 用 `stopped=true` 而不是在客户端过滤：页面要呈现的是"最终失败状态"
 * （FR-15 §5），把仍在重试的记录混进来会把"正在重试"误报成"已失败"。
 */
export async function fetchStoppedDeliveries(): Promise<NotificationDelivery[]> {
  const response = await fetch('/api/v1/notification-deliveries?stopped=true&limit=20');
  if (!response.ok) {
    throw new Error(await problemDetail(response));
  }
  const payload: unknown = await response.json();
  if (!isRecord(payload) || !Array.isArray(payload.items) || !payload.items.every(isDelivery)) {
    throw new Error('投递结果返回了无法识别的数据');
  }
  return payload.items;
}
