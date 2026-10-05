// A static server for the built SPA and a stubbed API, so the screenshots need no lab server and no
// network.
import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';

const TYPES = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript',
  '.css': 'text/css',
  '.svg': 'image/svg+xml',
  '.woff2': 'font/woff2',
  '.png': 'image/png',
  '.json': 'application/json',
  '.webmanifest': 'application/manifest+json',
  '.ico': 'image/x-icon',
};

/** Serves `dir` on a free local port; any path that is not a file gets index.html (SPA fallback). */
export function serveDist(dir) {
  const root = path.resolve(dir);
  const server = http.createServer((req, res) => {
    let file = path.join(root, decodeURIComponent(new URL(req.url, 'http://x').pathname));
    if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) {
      file = path.join(root, 'index.html');
    }
    res.writeHead(200, { 'content-type': TYPES[path.extname(file)] ?? 'application/octet-stream' });
    fs.createReadStream(file).pipe(res);
  });
  return new Promise((resolve) =>
    server.listen(0, '127.0.0.1', () =>
      resolve({ url: `http://127.0.0.1:${server.address().port}`, close: () => server.close() }),
    ),
  );
}

const json = (route, status, body) =>
  route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) });

/**
 * Answers every /api/v1/** request of `page` from `routes`, keyed "METHOD /path" (query string
 * included; a key without one also matches any query). A value is a body or a function
 * (request) => body; a body `{ __status, ...payload }` answers with that status. The event stream
 * delivers `events` once and then stays open. Returns the unanswered requests in `misses`.
 */
export async function stubApi(page, routes, events = []) {
  const misses = [];
  let streamed = false;
  await page.route('**/api/v1/**', async (route, request) => {
    const url = new URL(request.url());
    if (url.pathname === '/api/v1/events') {
      if (streamed) return; // left pending: the app sees a quiet, open stream
      streamed = true;
      const body = events.map(
        ([type, data]) => `event: ${type}\ndata: ${JSON.stringify(data)}\n\n`,
      );
      return route.fulfill({ status: 200, contentType: 'text/event-stream', body: body.join('') });
    }
    const key = `${request.method()} ${url.pathname}`;
    const value = routes[key + url.search] ?? routes[key];
    if (value === undefined) {
      misses.push(key + url.search);
      return json(route, 404, { error: `not stubbed: ${key}` });
    }
    const body = typeof value === 'function' ? await value(request) : value;
    if (body && typeof body === 'object' && '__status' in body) {
      const { __status, ...payload } = body;
      return json(route, __status, payload);
    }
    return json(route, 200, body);
  });
  return { misses };
}
