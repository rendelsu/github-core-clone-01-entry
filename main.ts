// main.ts — the repository entrypoint. It spawns the Go forge binary
// (./forge, statically linked Linux amd64, committed beside this file) and
// waits for it: the binary binds the ephemeral port, registers with the env
// and exits 0 when the suite reports done. The exit code is the binary's.
//
// The forge itself is Go (see the .go sources in this repository); this file
// only launches it, because the trial runs node /bot/main.ts.

import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const here = path.dirname(fileURLToPath(import.meta.url));
const binary = path.join(here, 'forge');
const result = spawnSync(binary, { stdio: 'inherit', env: process.env });
process.exit(result.status ?? 1);
