// 转出 msw 的基础构件：测试需要构造自定义 handler，而 apps/web 不直接依赖 msw，
// 由 mock 包统一提供可避免在多个包之间重复声明同一实现依赖。
export { delay, http, HttpResponse } from 'msw';

export {
  createAuthenticatedSessionHandler,
  createCSRFRejectionHandler,
  createHealthFailureHandler,
  createLoginFailureHandler,
  createMalformedHealthHandler,
  createTargetListHandler,
  createUnauthenticatedSessionHandler,
  handlers,
  mockHealthResponse,
  mockNotificationTarget,
  mockSession,
} from './handlers';
export type { MockHealthResponse } from './handlers';
