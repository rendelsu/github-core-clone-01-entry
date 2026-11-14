// main.ts — the entrypoint. One process, one in-memory forge.
//
// Lifecycle (from the competition rules):
//   1. bind an ephemeral port (never 8080 — that port belongs to the env),
//   2. POST {"port": <port>} to $MAKEFUN_ENV_URL/register (the first
//      registration wins; a later one is answered 409),
//   3. poll GET $MAKEFUN_ENV_URL/done with exponential backoff
//      (250 ms -> 2.000 ms) until it answers {"done": true}, then exit 0.
// Without MAKEFUN_ENV_URL the forge runs standalone on its ephemeral port
// and keeps serving (handy for local runs against sdk/run.ts).

import { Store } from './store.ts';
import { createServer } from './server.ts';

const ENV_URL = process.env.MAKEFUN_ENV_URL;
const HOST = '127.0.0.1';

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function main(): Promise<void> {
  const store = new Store();
  const server = createServer(store);

  const port = await new Promise<number>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, HOST, () => {
      const address = server.address();
      if (address === null || typeof address === 'string') {
        reject(new Error('could not read the bound port'));
        return;
      }
      resolve(address.port);
    });
  });

  if (ENV_URL === undefined || ENV_URL === '') {
    console.log(`github-core-clone listening on http://${HOST}:${port} (standalone, no MAKEFUN_ENV_URL)`);
    return;
  }

  // register this port with the environment
  const register = await fetch(`${ENV_URL}/register`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ port }),
  });
  if (!register.ok) {
    console.error(`register failed: HTTP ${register.status}`);
    server.close();
    process.exit(1);
  }

  // poll /done until the suite finishes, then exit cleanly
  let delay = 250;
  for (;;) {
    await sleep(delay);
    delay = Math.min(2000, delay * 2);
    let done = false;
    try {
      const response = await fetch(`${ENV_URL}/done`);
      const payload = (await response.json()) as { done?: unknown };
      done = payload.done === true;
    } catch {
      // the env may not answer yet; keep polling
    }
    if (done) {
      server.close();
      process.exit(0);
    }
  }
}

main().catch((err: unknown) => {
  console.error(err instanceof Error ? err.message : err);
  process.exit(1);
});
