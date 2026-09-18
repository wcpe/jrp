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

export const handlers = [
  http.get('/healthz', async () => {
    await delay(80);
    return HttpResponse.json(mockHealthResponse);
  }),
  http.get('/readyz', () => HttpResponse.json(mockHealthResponse)),
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
