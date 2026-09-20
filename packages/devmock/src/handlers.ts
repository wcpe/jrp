import { delay, http, HttpResponse } from 'msw';

export interface MockHealthResponse {
  core_baseline: string;
  protocol: string;
  status: string;
  version: string;
  wire_versions: string[];
}

export const mockHealthResponse: MockHealthResponse = {
  core_baseline: 'jrp@e1f1d6ab1b591ec9b596f4b1b6a81cd2960c6314',
  protocol: 'jrp',
  status: 'ok',
  version: '0.1.0-dev',
  wire_versions: ['v1', 'v2'],
};

/** 已认证会话的固定夹具：与 jrps 的 sessionResponse 同形。 */
export const mockSession = {
  username: 'admin',
  expiresAt: '2026-01-01T00:00:00Z',
  csrfToken: 'mock-csrf-token',
};

/** 一条通知目标夹具：与 jrps 的 NotificationTargetView 同形。 */
export const mockNotificationTarget = {
  id: 'nt_mock_1',
  name: '运维群',
  type: 'webhook',
  enabled: true,
  maskedSecret: '****cret',
  summary: 'Webhook hooks.example.com',
  webhookUrl: 'https://hooks.example.com/hook',
  smtpHost: '',
  smtpPort: 0,
  smtpFrom: '',
  smtpTo: [],
  smtpSecurity: '',
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z',
};

/** 一条失败终态的投递记录夹具：与 jrps 的投递结果响应项同形。 */
export const mockFailedDelivery = {
  id: 42,
  eventId: 'evt_mock_failed',
  targetId: 'nt_mock_1',
  eventType: 'notification_target_created',
  status: 'failed',
  attempts: 4,
  lastError: '投递失败：连接被拒绝',
  nextAttemptAt: '',
  stoppedAt: '2026-01-01T00:05:00Z',
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:05:00Z',
};

function problem(status: number, detail: string) {
  return HttpResponse.json({ title: '请求失败', status, detail, requestId: 'mock' }, { status });
}

export const handlers = [
  http.get('/healthz', async () => {
    await delay(80);
    return HttpResponse.json(mockHealthResponse);
  }),
  http.get('/readyz', () => HttpResponse.json(mockHealthResponse)),
  // 默认未登录：需要已登录的场景用 createAuthenticatedSessionHandler 覆盖。
  http.get('/api/v1/session', () => problem(401, '未认证')),
];

export function createHealthFailureHandler() {
  return http.get('/healthz', () =>
    HttpResponse.json({ error: '模拟的 jrps 健康检查失败' }, { status: 503 }),
  );
}

export function createMalformedHealthHandler() {
  return http.get('/healthz', () =>
    HttpResponse.json({ ...mockHealthResponse, wire_versions: 'v1,v2' }),
  );
}

/** 已认证会话：GET /api/v1/session 返回夹具。 */
export function createAuthenticatedSessionHandler() {
  return http.get('/api/v1/session', () => HttpResponse.json(mockSession));
}

/** 未认证会话：GET /api/v1/session 返回 401。 */
export function createUnauthenticatedSessionHandler() {
  return http.get('/api/v1/session', () => problem(401, '未认证'));
}

/** 登录失败：POST /api/v1/session 返回统一的 401 问题详情。 */
export function createLoginFailureHandler(detail = '用户名或密码错误') {
  return http.post('/api/v1/session', () => problem(401, detail));
}

/** 通知目标列表：与 jrps 的 `{items:[...]}` 包裹结构同形，可传入自定义集合。 */
export function createTargetListHandler(targets: unknown[] = [mockNotificationTarget]) {
  return http.get('/api/v1/notification-targets', () => HttpResponse.json({ items: targets }));
}

/** 投递结果：默认返回一条失败终态记录，与 jrps 的包裹结构同形。 */
export function createDeliveryListHandler(deliveries: unknown[] = [mockFailedDelivery]) {
  return http.get('/api/v1/notification-deliveries', () =>
    HttpResponse.json({ items: deliveries }),
  );
}

/**
 * 测试通知发送成功。
 *
 * 路径按契约使用 `{targetId}:test` 形式（gin 侧以 catch-all 实现），
 * 不是 `/targets/{id}/test`——写成后者在实机上会 404。
 */
export function createTestNotificationHandler() {
  return http.post(/\/api\/v1\/notification-targets\/[^/]+:test$/, () =>
    HttpResponse.json({ message: '测试通知已发送', status: 'ok' }),
  );
}

/** 修改类请求被拒：用于验证前端如实展示 CSRF 失败。 */
export function createCSRFRejectionHandler() {
  return http.post('/api/v1/notification-targets', () => problem(403, 'CSRF 校验失败'));
}
