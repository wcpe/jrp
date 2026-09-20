import { fileURLToPath, URL } from 'node:url';

import react from '@vitejs/plugin-react';
import { defineConfig } from 'vite';

export default defineConfig(({ command }) => ({
  build: {
    emptyOutDir: true,
    outDir: fileURLToPath(new URL('../jrps/internal/webui/dist', import.meta.url)),
  },
  plugins: [react()],
  publicDir: command === 'serve' ? 'public' : false,
  server: {
    host: '127.0.0.1',
    port: 5173,
    proxy: {
      '/healthz': 'http://127.0.0.1:7500',
      '/readyz': 'http://127.0.0.1:7500',
      // 管理 API 与页面同源部署（生产由 jrps 内嵌资源提供）；开发态经此代理
      // 转发到本地 jrps，使会话 Cookie 与 CSRF 流程在 dev 下与生产一致。
      '/api': 'http://127.0.0.1:7500',
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: './src/test/setup.ts',
  },
}));
