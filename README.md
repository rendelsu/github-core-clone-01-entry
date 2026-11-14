# github-core-clone-01-entry

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/rendelsu/github-core-clone-01-entry/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/rendelsu/github-core-clone-01-entry/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge in one Node process: git smart HTTP (upload-pack v2/v0, receive-pack v0), REST v3, GraphQL v4 and MCP, with no dependencies — only the Node 22 standard library. All state lives in memory, every id is a counter, and every timestamp comes from a logical clock, so replays are deterministic.

## Run it

```sh
node main.ts
```

With `MAKEFUN_ENV_URL` set, the forge binds an ephemeral port, registers it with the environment, and exits 0 when the suite reports done. Without it, it serves standalone on an ephemeral port (set `MAKEFUN_SHOP_TOKEN` to require the Bearer token on the API surfaces).

---

<sub>Entered on [make.fun](https://make.fun/?ref=rkkz4bh8&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
