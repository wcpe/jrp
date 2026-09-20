import '@testing-library/jest-dom/vitest';
import { cleanup } from '@testing-library/react';
import { afterAll, afterEach, beforeAll } from 'vitest';

import { mockServer } from '@jrp/devmock/server';

beforeAll(() => mockServer.listen({ onUnhandledRequest: 'error' }));
afterEach(() => {
  cleanup();
  mockServer.resetHandlers();
  // 地址栏在 jsdom 中跨用例保留：复位到首页，避免上一个用例的跳转改变下一个用例的初始位置。
  window.history.replaceState(null, '', '/');
});
afterAll(() => mockServer.close());

Object.defineProperty(window, 'scrollTo', {
  value: () => undefined,
  writable: true,
});

Object.defineProperty(window, 'matchMedia', {
  value: (query: string) => ({
    addEventListener: () => undefined,
    addListener: () => undefined,
    dispatchEvent: () => false,
    matches: false,
    media: query,
    onchange: null,
    removeEventListener: () => undefined,
    removeListener: () => undefined,
  }),
  writable: true,
});
