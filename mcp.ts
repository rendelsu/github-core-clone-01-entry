// mcp.ts — the Model Context Protocol surface over POST /mcp (Streamable
// HTTP, JSON-RPC 2.0). The modern 2026-07-28 discovery handshake is served,
// with the older `initialize` handshake as a fallback. Six tools expose the
// forge to agents; every tool reads and writes the same Store the REST and
// GraphQL surfaces use, so work done through MCP shows up in /api/v3.

import { Store, VIEWER_LOGIN, isoTime, type Repo } from './store.ts';

const SERVER_NAME = 'github-core-clone';
const SERVER_VERSION = '1.0.0';

export interface McpResult {
  status: number;
  /** null means "no body" (a notification got a bare 202). */
  body: unknown;
}

interface ToolDef {
  name: string;
  description: string;
  inputSchema: {
    type: 'object';
    properties: Record<string, { type: string; description: string }>;
    required: string[];
  };
}

/** The tool list is a static constant, so two tools/list calls answer byte-identically. */
const TOOLS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository owned by the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'The name of the repository to create.' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The owner of the repository.' },
        repo: { type: 'string', description: 'The name of the repository.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The owner of the repository.' },
        repo: { type: 'string', description: 'The name of the repository.' },
        title: { type: 'string', description: 'The title of the issue.' },
        body: { type: 'string', description: 'The body of the issue.' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List issues in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The owner of the repository.' },
        repo: { type: 'string', description: 'The name of the repository.' },
        state: { type: 'string', description: 'Filter by state: open, closed or all.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request from a head branch onto a base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The owner of the repository.' },
        repo: { type: 'string', description: 'The name of the repository.' },
        title: { type: 'string', description: 'The title of the pull request.' },
        head: { type: 'string', description: 'The head branch name.' },
        base: { type: 'string', description: 'The base branch name.' },
        body: { type: 'string', description: 'The body of the pull request.' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Squash-merge a pull request.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The owner of the repository.' },
        repo: { type: 'string', description: 'The name of the repository.' },
        pull_number: { type: 'integer', description: 'The pull request number.' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function asString(value: unknown): string | null {
  return typeof value === 'string' ? value : null;
}

/** Pull one argument out of the call, accepting the common spelling variants. */
function arg(args: Record<string, unknown>, ...names: string[]): unknown {
  for (const name of names) {
    if (args[name] !== undefined) return args[name];
  }
  return undefined;
}

/** Resolve owner/repo from the arguments, including a full_name fallback. */
function resolveRepo(store: Store, args: Record<string, unknown>): Repo | null {
  let owner = asString(arg(args, 'owner'));
  let repoName = asString(arg(args, 'repo', 'repository'));
  const fullName = asString(arg(args, 'full_name', 'fullName'));
  if ((owner === null || repoName === null) && fullName !== null) {
    const parts = fullName.split('/');
    if (parts.length === 2) {
      owner ??= parts[0]!;
      repoName ??= parts[1]!;
    }
  }
  if (owner === null || repoName === null) return null;
  return store.getRepo(owner, repoName) ?? null;
}

function textResult(value: unknown): Record<string, unknown> {
  return {
    structuredContent: value,
    content: [{ type: 'text', text: JSON.stringify(value) }],
  };
}

function errorResult(message: string): Record<string, unknown> {
  return {
    content: [{ type: 'text', text: message }],
    isError: true,
  };
}

/** Run one tool call; the result is a tools/call result object. */
function callTool(store: Store, name: string, args: Record<string, unknown>): Record<string, unknown> {
  const normalized = name.toLowerCase().replace(/^[^a-z0-9]+/, '');
  const tool = TOOLS.find((candidate) => normalized === candidate.name || normalized.endsWith(`__${candidate.name}`));
  if (tool === undefined) {
    return { error: { code: -32602, message: `unknown tool: ${name}` } };
  }
  // required arguments, with the documented aliases accepted
  for (const required of tool.inputSchema.required) {
    const aliases: Record<string, string[]> = {
      repo: ['repo', 'repository'],
      pull_number: ['pull_number', 'pullNumber', 'number'],
      name: ['name', 'repo', 'repository'],
    };
    const present = arg(args, ...(aliases[required] ?? [required])) !== undefined;
    if (!present) return errorResult(`missing required argument: ${required}`);
  }

  if (tool.name === 'create_repo') {
    const repoName = asString(arg(args, 'name', 'repo', 'repository')) ?? asString(arg(args, 'full_name'))?.split('/').pop() ?? null;
    if (repoName === null || repoName === '') return errorResult('missing required argument: name');
    const repo = store.createRepo(repoName, false, '');
    if (repo === null) return errorResult(`Repository already exists: ${VIEWER_LOGIN}/${repoName}`);
    return textResult({
      full_name: `${repo.owner}/${repo.name}`,
      name: repo.name,
      owner: repo.owner,
      private: repo.priv,
      default_branch: repo.defaultBranch,
      description: repo.description,
    });
  }

  if (tool.name === 'get_repo') {
    const repo = resolveRepo(store, args);
    if (repo === null) return errorResult('Repository not found');
    return textResult({
      full_name: `${repo.owner}/${repo.name}`,
      name: repo.name,
      owner: repo.owner,
      private: repo.priv,
      default_branch: repo.defaultBranch,
      description: repo.description,
      created_at: isoTime(repo.seq),
    });
  }

  if (tool.name === 'create_issue') {
    const repo = resolveRepo(store, args);
    if (repo === null) return errorResult('Repository not found');
    const title = asString(arg(args, 'title'));
    if (title === null || title === '') return errorResult('missing required argument: title');
    const body = asString(arg(args, 'body')) ?? '';
    const issue = store.createIssue(repo, title, body, []);
    return textResult({
      number: issue.number,
      id: issue.id,
      title: issue.title,
      body: issue.body,
      state: issue.state,
      created_at: isoTime(issue.seq),
    });
  }

  if (tool.name === 'list_issues') {
    const repo = resolveRepo(store, args);
    if (repo === null) return errorResult('Repository not found');
    const state = asString(arg(args, 'state')) ?? 'open';
    let list = [...repo.issues.values()];
    if (state === 'open') list = list.filter((issue) => issue.state === 'open');
    else if (state === 'closed') list = list.filter((issue) => issue.state === 'closed');
    list.sort((a, b) => a.seq - b.seq);
    return textResult({
      issues: list.map((issue) => ({
        number: issue.number,
        title: issue.title,
        body: issue.body,
        state: issue.state,
        created_at: isoTime(issue.seq),
      })),
    });
  }

  if (tool.name === 'create_pull_request') {
    const repo = resolveRepo(store, args);
    if (repo === null) return errorResult('Repository not found');
    const title = asString(arg(args, 'title'));
    const head = asString(arg(args, 'head'));
    const baseBranch = asString(arg(args, 'base'));
    if (title === null || head === null || baseBranch === null) {
      return errorResult('title, head and base are required');
    }
    const headSha = repo.refs.get(`refs/heads/${head}`);
    const baseSha = repo.refs.get(`refs/heads/${baseBranch}`);
    if (headSha === undefined || baseSha === undefined) {
      return errorResult('head and base must name existing branches');
    }
    const existing = [...repo.pulls.values()].find(
      (pull) => pull.state === 'open' && pull.headRef === head && pull.baseRef === baseBranch,
    );
    if (existing !== undefined) {
      return textResult({ number: existing.number, title: existing.title, state: existing.state, head, base: baseBranch });
    }
    const body = asString(arg(args, 'body')) ?? '';
    const pull = store.createPull(repo, head, baseBranch, headSha, baseSha, title, body);
    return textResult({
      number: pull.number,
      id: pull.id,
      title: pull.title,
      body: pull.body,
      state: pull.state,
      head,
      base: baseBranch,
    });
  }

  if (tool.name === 'merge_pull_request') {
    const repo = resolveRepo(store, args);
    if (repo === null) return errorResult('Repository not found');
    const raw = arg(args, 'pull_number', 'pullNumber', 'number');
    const number = typeof raw === 'number' ? raw : parseInt(String(raw ?? ''), 10);
    const pull = repo.pulls.get(number);
    if (pull === undefined) return errorResult(`Pull request not found: ${number}`);
    if (pull.state !== 'open') return errorResult('Pull request is already closed');
    const merged = store.squashMerge(repo, pull);
    if (!merged.ok) return errorResult(`Pull request cannot be merged: ${merged.reason}`);
    return textResult({ number: pull.number, merged: true, sha: merged.sha, state: pull.state });
  }

  return { error: { code: -32602, message: `unknown tool: ${name}` } };
}

function rpcResult(id: unknown, result: unknown): Record<string, unknown> {
  return { jsonrpc: '2.0', id: id ?? null, result };
}

function rpcError(id: unknown, code: number, message: string): Record<string, unknown> {
  return { jsonrpc: '2.0', id: id ?? null, error: { code, message } };
}

/** Handle one JSON-RPC request object. */
function handleOne(store: Store, request: unknown): McpResult | null {
  if (!isRecord(request)) return { status: 400, body: rpcError(null, -32600, 'Invalid Request') };
  const id = request.id ?? null;
  const isNotification = request.id === undefined;
  const method = typeof request.method === 'string' ? request.method : '';
  const params = isRecord(request.params) ? request.params : {};

  if (method === 'server/discover') {
    return {
      status: 200,
      body: rpcResult(id, {
        supportedVersions: ['2026-07-28', '2025-11-25'],
        serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
        capabilities: { tools: {} },
        _meta: { 'io.modelcontextprotocol/serverInfo': { name: SERVER_NAME, version: SERVER_VERSION } },
      }),
    };
  }
  if (method === 'initialize') {
    return {
      status: 200,
      body: rpcResult(id, {
        protocolVersion: '2025-11-25',
        capabilities: { tools: {} },
        serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
      }),
    };
  }
  if (method === 'notifications/initialized' || method.startsWith('notifications/')) {
    // a notification is answered with nothing (202, no body)
    return isNotification ? { status: 202, body: null } : { status: 200, body: rpcResult(id, {}) };
  }
  if (method === 'ping') {
    return { status: 200, body: rpcResult(id, {}) };
  }
  if (method === 'tools/list') {
    return { status: 200, body: rpcResult(id, { tools: TOOLS }) };
  }
  if (method === 'tools/call') {
    const name = typeof params.name === 'string' ? params.name : '';
    const args = isRecord(params.arguments) ? (params.arguments as Record<string, unknown>) : {};
    const result = callTool(store, name, args);
    if (isRecord(result) && 'error' in result && isRecord(result.error)) {
      const error = result.error as Record<string, unknown>;
      return { status: 200, body: rpcError(id, typeof error.code === 'number' ? error.code : -32602, String(error.message ?? 'tool error')) };
    }
    return { status: 200, body: rpcResult(id, result) };
  }
  if (method === 'prompts/list') {
    return { status: 200, body: rpcResult(id, { prompts: [] }) };
  }
  if (method === 'resources/list') {
    return { status: 200, body: rpcResult(id, { resources: [] }) };
  }
  return { status: 200, body: rpcError(id, -32601, `Method not found: ${method}`) };
}

/** Handle a POST /mcp body (raw bytes; a bad body answers -32700). */
export function handleMcp(store: Store, rawBody: Buffer): McpResult {
  let parsed: unknown;
  try {
    parsed = JSON.parse(rawBody.toString('utf8'));
  } catch {
    // try to recover the request id from the raw text, else null
    const match = /"id"\s*:\s*("[^"]*"|\d+|null)/.exec(rawBody.toString('utf8'));
    let id: unknown = null;
    if (match !== null) {
      try {
        id = JSON.parse(match[1]!);
      } catch {
        id = null;
      }
    }
    return { status: 400, body: rpcError(id, -32700, 'Parse error') };
  }
  if (Array.isArray(parsed)) {
    const results: unknown[] = [];
    for (const item of parsed) {
      const outcome = handleOne(store, item);
      if (outcome !== null && outcome.body !== null) results.push(outcome.body);
    }
    return { status: 200, body: results };
  }
  const outcome = handleOne(store, parsed);
  if (outcome === null) return { status: 202, body: null };
  return outcome;
}
