// server.ts — the HTTP front door. One node:http server routes four surfaces
// over a single Store: the git smart-HTTP endpoints (no auth, public-clone
// semantics), REST v3, GraphQL v4 and MCP (all behind the Bearer token).
// Every reply carries X-GitHub-Request-Id; every REST-shaped error is JSON.

import http from 'node:http';
import { Store, VIEWER_LOGIN } from './store.ts';
import * as G from './githttp.ts';
import * as Git from './gitobj.ts';
import { handleRest } from './rest.ts';
import { handleGraphQL } from './graphql.ts';
import { handleMcp } from './mcp.ts';

const MAX_BODY_BYTES = 128 * 1024 * 1024;

export function createServer(store: Store): http.Server {
  let requestSeq = 0;

  function sendJson(
    res: http.ServerResponse,
    status: number,
    value: unknown,
    extraHeaders: Record<string, string> = {},
  ): void {
    const body = value === null ? '' : JSON.stringify(value);
    res.writeHead(status, {
      'content-type': 'application/json',
      'x-github-request-id': `req-${++requestSeq}`,
      ...extraHeaders,
    });
    res.end(body);
  }

  function sendBuffer(
    res: http.ServerResponse,
    status: number,
    data: Buffer,
    contentType: string,
  ): void {
    res.writeHead(status, {
      'content-type': contentType,
      'x-github-request-id': `req-${++requestSeq}`,
    });
    res.end(data);
  }

  /** The API token: when the env var is unset, any non-empty Bearer is accepted. */
  function authorized(req: http.IncomingMessage): boolean {
    const header = req.headers.authorization ?? '';
    if (!header.startsWith('Bearer ')) return false;
    const token = header.slice('Bearer '.length);
    if (token === '') return false;
    const expected = process.env.MAKEFUN_SHOP_TOKEN;
    if (expected === undefined || expected === '') return true;
    return token === expected;
  }

  function readBody(req: http.IncomingMessage): Promise<Buffer> {
    return new Promise((resolve, reject) => {
      const chunks: Buffer[] = [];
      let size = 0;
      req.on('data', (chunk: Buffer) => {
        size += chunk.length;
        if (size > MAX_BODY_BYTES) {
          reject(new Error('request body too large'));
          req.destroy();
          return;
        }
        chunks.push(chunk);
      });
      req.on('end', () => resolve(Buffer.concat(chunks)));
      req.on('error', reject);
    });
  }

  function parseJsonBody(raw: Buffer): { ok: true; value: unknown } | { ok: false } {
    if (raw.length === 0) return { ok: true, value: null };
    try {
      return { ok: true, value: JSON.parse(raw.toString('utf8')) };
    } catch {
      return { ok: false };
    }
  }

  const server = http.createServer(async (req, res) => {
    try {
      const url = new URL(req.url ?? '/', `http://${req.headers.host ?? 'localhost'}`);
      const method = req.method ?? 'GET';
      const segments = url.pathname.split('/').filter((part) => part !== '');

      // GET /healthz — the boot probe, no token.
      if (url.pathname === '/healthz' && method === 'GET') {
        sendJson(res, 200, { ok: true });
        return;
      }

      // ---- git smart HTTP: /{owner}/{repo}.git/{info/refs,git-upload-pack,git-receive-pack}
      if (segments.length >= 2 && segments[1]!.endsWith('.git')) {
        const owner = segments[0]!;
        const repoName = segments[1]!.slice(0, -'.git'.length);
        const repo = owner === VIEWER_LOGIN ? store.getRepo(owner, repoName) : undefined;
        const tail = segments.slice(2);
        if (repo === undefined) {
          sendJson(res, 404, { message: 'Not Found' });
          return;
        }
        if (tail.length === 2 && tail[0] === 'info' && tail[1] === 'refs' && method === 'GET') {
          const service = url.searchParams.get('service');
          const wantsV2 = (req.headers['git-protocol'] ?? '') === 'version=2';
          if (service === 'git-upload-pack') {
            const body = wantsV2 ? G.v2Advertisement() : G.v0Advertisement(repo, 'git-upload-pack');
            sendBuffer(res, 200, body, 'application/x-git-upload-pack-advertisement');
            return;
          }
          if (service === 'git-receive-pack') {
            sendBuffer(res, 200, G.v0Advertisement(repo, 'git-receive-pack'), 'application/x-git-receive-pack-advertisement');
            return;
          }
          sendJson(res, 400, { message: 'a service query parameter is required' });
          return;
        }
        if (tail.length === 1 && tail[0] === 'git-upload-pack' && method === 'POST') {
          const raw = await readBody(req);
          const wantsV2 = (req.headers['git-protocol'] ?? '') === 'version=2';
          if (!wantsV2) {
            sendBuffer(res, 200, G.fetchV0Response(store, repo, raw), 'application/x-git-upload-pack-result');
            return;
          }
          const { packets } = Git.decodePktLines(raw);
          const first = packets.find((packet) => packet.kind === 'data');
          const command = first?.kind === 'data' ? first.payload.toString('utf8').replace(/\n$/, '') : '';
          if (command === 'command=ls-refs') {
            sendBuffer(res, 200, G.lsRefsResponse(repo, raw), 'application/x-git-upload-pack-result');
            return;
          }
          sendBuffer(res, 200, G.fetchV2Response(store, repo, raw), 'application/x-git-upload-pack-result');
          return;
        }
        if (tail.length === 1 && tail[0] === 'git-receive-pack' && method === 'POST') {
          const raw = await readBody(req);
          const outcome = G.applyPush(store, repo, raw);
          sendBuffer(res, 200, G.pushResponse(outcome), 'application/x-git-receive-pack-result');
          return;
        }
        sendJson(res, 404, { message: 'Not Found' });
        return;
      }

      // ---- the API surfaces all need the Bearer token
      if (segments[0] === 'api' && segments[1] === 'v3') {
        if (!authorized(req)) {
          sendJson(res, 401, { message: 'Requires authentication' });
          return;
        }
        const restSegments = segments.slice(2);
        const raw = await readBody(req);
        const parsed = parseJsonBody(raw);
        if (!parsed.ok) {
          sendJson(res, 400, { message: 'Invalid JSON' });
          return;
        }
        const result = handleRest(store, method, restSegments, url.searchParams, parsed.value, req.headers.host ?? 'localhost');
        const extra: Record<string, string> = { 'x-oauth-scopes': 'repo, read:org' };
        if (result.headers !== undefined) {
          for (const [key, value] of Object.entries(result.headers)) extra[key.toLowerCase()] = value;
        }
        sendJson(res, result.status, result.body, extra);
        return;
      }

      if (segments[0] === 'api' && segments[1] === 'graphql') {
        if (!authorized(req)) {
          sendJson(res, 401, { message: 'Requires authentication' });
          return;
        }
        if (method !== 'POST') {
          sendJson(res, 404, { message: 'Not Found' });
          return;
        }
        const raw = await readBody(req);
        const parsed = parseJsonBody(raw);
        if (!parsed.ok) {
          sendJson(res, 400, { errors: [{ message: 'Invalid JSON' }] });
          return;
        }
        const result = handleGraphQL(store, parsed.value);
        sendJson(res, result.status, result.body);
        return;
      }

      if (segments[0] === 'mcp') {
        if (!authorized(req)) {
          sendJson(res, 401, { message: 'Requires authentication' });
          return;
        }
        if (method !== 'POST') {
          sendJson(res, 404, { message: 'Not Found' });
          return;
        }
        const raw = await readBody(req);
        const result = handleMcp(store, raw);
        if (result.body === null) {
          res.writeHead(result.status, { 'x-github-request-id': `req-${++requestSeq}` });
          res.end();
          return;
        }
        sendJson(res, result.status, result.body);
        return;
      }

      sendJson(res, 404, { message: 'Not Found' });
    } catch (err) {
      // never leak a stack or an HTML error page to the harness
      if (!res.headersSent) sendJson(res, 500, { message: err instanceof Error ? err.message : 'Internal Server Error' });
      else res.end();
    }
  });

  return server;
}
