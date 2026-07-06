import { defineConfig } from 'vitest/config';
import solid from 'vite-plugin-solid';

// The dev server proxies the API and health endpoints to a locally running
// `lab` server. /api covers /api/v1/events (SSE): timeouts are disabled so the
// stream is never cut, and changeOrigin stays off so the backend's
// Origin-vs-Host CSRF check keeps working through the proxy.
const backend = 'http://localhost:8080';

export default defineConfig({
  plugins: [solid()],
  server: {
    proxy: {
      '/api': {
        target: backend,
        // SSE-friendly: never time out the request or the upstream socket.
        timeout: 0,
        proxyTimeout: 0,
      },
      '/healthz': { target: backend },
    },
  },
  build: {
    outDir: 'dist',
  },
  test: {
    environment: 'jsdom',
  },
});
