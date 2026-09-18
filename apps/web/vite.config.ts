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
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: './src/test/setup.ts',
  },
}));
