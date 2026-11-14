// rest.ts — the GitHub REST v3 surface. Handlers are pure functions over the
// Store: they take the decoded path segments, the query and the parsed JSON
// body, and return {status, body, headers}. The HTTP server (server.ts) owns
// auth, body parsing and serialization.

import * as G from './gitobj.ts';
import { Store, VIEWER_EMAIL, VIEWER_ID, VIEWER_LOGIN, isoTime, type Comment, type Issue, type Pull, type Repo } from './store.ts';

export interface RestResult {
  status: number;
  body: unknown;
  headers?: Record<string, string>;
}

function ok(body: unknown, status = 200, headers?: Record<string, string>): RestResult {
  return { status, body, headers };
}

function err(status: number, message: string): RestResult {
  return { status, body: { message } };
}

function globalId(kind: string, ...parts: Array<string | number>): string {
  return Buffer.from(`${kind}:${parts.join(':')}`, 'utf8').toString('base64url');
}

function repoJson(repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    node_id: globalId('R', repo.id),
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    private: repo.priv,
    owner: { login: repo.owner, id: VIEWER_ID, type: 'User' },
    description: repo.description,
    default_branch: repo.defaultBranch,
    created_at: isoTime(repo.seq),
    updated_at: isoTime(repo.seq),
    pushed_at: isoTime(repo.seq),
    html_url: `https://github.com/${repo.owner}/${repo.name}`,
  };
}

function labelJson(label: { id: number; name: string; color: string; description: string }): Record<string, unknown> {
  return { id: label.id, node_id: globalId('L', label.id), name: label.name, color: label.color, description: label.description };
}

function issueJson(repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: issue.id,
    node_id: globalId('I', repo.id, issue.number),
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => {
      const label = repo.labels.get(name);
      return label !== undefined ? labelJson(label) : { name };
    }),
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    created_at: isoTime(issue.seq),
    updated_at: isoTime(issue.seq),
    comments: issue.comments.length,
    html_url: `https://github.com/${repo.owner}/${repo.name}/issues/${issue.number}`,
  };
}

function commentJson(repo: Repo, issue: Issue, comment: Comment): Record<string, unknown> {
  return {
    id: comment.id,
    node_id: globalId('C', repo.id, issue.number, comment.id),
    body: comment.body,
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    created_at: isoTime(comment.seq),
    updated_at: isoTime(comment.seq),
  };
}

function pullJson(repo: Repo, pull: Pull): Record<string, unknown> {
  const tips = repo.refs.get(`refs/heads/${pull.headRef}`);
  const baseTip = repo.refs.get(`refs/heads/${pull.baseRef}`);
  return {
    id: pull.id,
    node_id: globalId('P', repo.id, pull.number),
    number: pull.number,
    state: pull.state,
    title: pull.title,
    body: pull.body,
    head: {
      ref: pull.headRef,
      sha: tips ?? pull.headSha,
      label: `${repo.owner}:${pull.headRef}`,
      user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    },
    base: {
      ref: pull.baseRef,
      sha: baseTip ?? pull.baseSha,
      label: `${repo.owner}:${pull.baseRef}`,
      user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    },
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    created_at: isoTime(pull.seq),
    updated_at: isoTime(pull.seq),
    html_url: `https://github.com/${repo.owner}/${repo.name}/pull/${pull.number}`,
  };
}

/** Pagination: per_page (default 30, 1..100) and page (default 1). */
function pageParams(query: URLSearchParams): { perPage: number; page: number } {
  const perPageRaw = parseInt(query.get('per_page') ?? '', 10);
  const pageRaw = parseInt(query.get('page') ?? '', 10);
  const perPage = Number.isFinite(perPageRaw) ? Math.min(100, Math.max(1, perPageRaw)) : 30;
  const page = Number.isFinite(pageRaw) ? Math.max(1, pageRaw) : 1;
  return { perPage, page };
}

/** Slice a list and build the GitHub `Link` header for the next page. */
function paginate<T>(items: readonly T[], query: URLSearchParams, baseUrl: string): { page: T[]; headers: Record<string, string> } {
  const { perPage, page } = pageParams(query);
  const start = (page - 1) * perPage;
  const sliced = items.slice(start, start + perPage);
  const headers: Record<string, string> = {};
  if (start + perPage < items.length) {
    const next = new URL(baseUrl);
    next.searchParams.set('per_page', String(perPage));
    next.searchParams.set('page', String(page + 1));
    const lastPage = Math.ceil(items.length / perPage);
    const last = new URL(baseUrl);
    last.searchParams.set('per_page', String(perPage));
    last.searchParams.set('page', String(lastPage));
    headers.link = `<${next.toString()}>; rel="next", <${last.toString()}>; rel="last"`;
  }
  return { page: [...sliced], headers };
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function asString(value: unknown): string | null {
  return typeof value === 'string' ? value : null;
}

/** Resolve a ref name or oid to a commit oid, or null. */
function resolveCommit(store: Store, repo: Repo, ref: string | null): string | null {
  if (ref === null) {
    return repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? null;
  }
  if (G.isOid(ref)) return repo.objects.has(ref) ? ref : null;
  return repo.refs.get(`refs/heads/${ref}`) ?? null;
}

/** Walk a tree to a path: the object and its type, or null when absent. */
function lookupPath(
  store: Store,
  repo: Repo,
  treeOid: string,
  segments: readonly string[],
): { type: 'blob' | 'tree'; oid: string; mode: string } | null {
  let current = treeOid;
  for (let index = 0; index < segments.length; index += 1) {
    const entries = store.treeEntries(repo, current);
    if (entries === null) return null;
    const entry = entries.find((candidate) => candidate.name === segments[index]);
    if (entry === undefined) return null;
    if (index === segments.length - 1) {
      return entry.mode === '40000'
        ? { type: 'tree', oid: entry.oid, mode: entry.mode }
        : { type: 'blob', oid: entry.oid, mode: entry.mode };
    }
    if (entry.mode !== '40000') return null;
    current = entry.oid;
  }
  return null;
}

function parseIdent(line: string): { name: string; email: string; date: string } {
  const match = /^(.*) <(.*)> (\d+) [+-]\d{4}$/.exec(line);
  if (match === null) return { name: line, email: '', date: isoTime(0) };
  return { name: match[1]!, email: match[2]!, date: new Date(parseInt(match[3]!, 10) * 1000).toISOString() };
}

/**
 * Write (PUT) or remove (DELETE, newContent null) one file on a branch and
 * commit it. Returns the commit and blob ids, or an error status.
 */
function contentsCommit(
  store: Store,
  repo: Repo,
  branch: string | null,
  path: string,
  message: string,
  newContent: Buffer | null,
  expectedSha: string | null,
): { status: number; commitSha?: string; contentSha?: string; created?: boolean; message?: string } {
  const branchName = branch ?? repo.defaultBranch;
  let tip = repo.refs.get(`refs/heads/${branchName}`);
  if (tip === undefined && branchName !== repo.defaultBranch) {
    // a new branch starts at the default branch's tip (or nothing when unborn)
    tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
  }
  let flat = new Map<string, { oid: string; mode: string }>();
  if (tip !== undefined) {
    const info = store.commitInfo(repo, tip);
    if (info === null) return { status: 409, message: 'the branch tip is not a commit' };
    flat = store.flattenTree(repo, info.tree);
  }
  const existing = flat.get(path);
  let contentSha: string | undefined;
  let created = true;
  if (newContent !== null) {
    contentSha = store.putObject(repo, 'blob', newContent);
    if (existing !== undefined) {
      created = false;
      if (expectedSha !== null && expectedSha.toLowerCase() !== existing.oid.toLowerCase()) {
        return { status: 409, message: 'the file has changed since the given sha' };
      }
    }
    flat.set(path, { oid: contentSha, mode: '100644' });
  } else {
    if (existing === undefined) return { status: 404, message: 'Not Found' };
    if (expectedSha !== null && expectedSha.toLowerCase() !== existing.oid.toLowerCase()) {
      return { status: 409, message: 'the file has changed since the given sha' };
    }
    flat.delete(path);
  }
  const tree = store.buildTreeFromFlat(repo, flat);
  const parents = tip === undefined ? [] : [tip];
  const commitSha = store.createCommit(repo, tree, parents, message);
  repo.refs.set(`refs/heads/${branchName}`, commitSha);
  return { status: 200, commitSha, contentSha, created };
}

// ---------------------------------------------------------------------------
// the dispatcher
// ---------------------------------------------------------------------------

export function handleRest(
  store: Store,
  method: string,
  segments: string[],
  query: URLSearchParams,
  body: unknown,
  host: string,
): RestResult {
  const base = `http://${host}`;
  const first = segments[0] ?? '';

  // GET /api/v3/ — the API root.
  if (segments.length === 0 || (segments.length === 1 && first === '')) {
    if (method !== 'GET') return err(404, 'Not Found');
    return ok(
      {
        current_user_url: `${base}/api/v3/user`,
        repository_url: `${base}/api/v3/repos/{owner}/{repo}`,
        user_repositories_url: `${base}/api/v3/user/repos`,
      },
      200,
      { 'x-oauth-scopes': 'repo, read:org' },
    );
  }

  if (first === 'meta') {
    if (method !== 'GET') return err(404, 'Not Found');
    return ok({ installed_version: '1.0.0' });
  }

  if (first === 'user') {
    if (segments.length === 1) {
      if (method !== 'GET') return err(404, 'Not Found');
      return ok({
        login: VIEWER_LOGIN,
        id: VIEWER_ID,
        node_id: globalId('U', VIEWER_ID),
        type: 'User',
        name: VIEWER_LOGIN,
        email: VIEWER_EMAIL,
      });
    }
    if (segments[1] === 'repos') {
      if (method === 'POST') {
        if (!isRecord(body)) return err(422, 'a repository name is required');
        const name = asString(body.name);
        if (name === null || name === '' || !/^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$/.test(name)) {
          return err(422, 'a valid repository name is required');
        }
        const priv = body.private === true;
        const description = asString(body.description) ?? '';
        const repo = store.createRepo(name, priv, description);
        if (repo === null) return err(422, 'Repository already exists');
        return ok(repoJson(repo), 201);
      }
      if (method === 'GET') {
        const list = store.listRepos().map((repo) => repoJson(repo));
        const { page, headers } = paginate(list, query, `${base}/api/v3/user/repos`);
        return ok(page, 200, headers);
      }
      return err(404, 'Not Found');
    }
    return err(404, 'Not Found');
  }

  if (first !== 'repos') return err(404, 'Not Found');

  const owner = segments[1] ?? '';
  const name = segments[2] ?? '';
  if (owner === '' || name === '') return err(404, 'Not Found');
  const repo = store.getRepo(owner, name);
  const rest = segments.slice(3);

  // GET / PATCH / DELETE /api/v3/repos/{owner}/{repo}
  if (rest.length === 0) {
    if (repo === undefined) return err(404, 'Not Found');
    if (method === 'GET') return ok(repoJson(repo));
    if (method === 'PATCH') {
      if (!isRecord(body)) return err(422, 'a JSON object body is required');
      const description = asString(body.description);
      if (description !== null) repo.description = description;
      if (typeof body.private === 'boolean') repo.priv = body.private;
      return ok(repoJson(repo));
    }
    if (method === 'DELETE') {
      store.deleteRepo(repo);
      return { status: 204, body: null };
    }
    return err(404, 'Not Found');
  }

  const section = rest[0]!;

  if (section === 'branches') {
    if (repo === undefined) return err(404, 'Not Found');
    if (rest.length === 1) {
      if (method !== 'GET') return err(404, 'Not Found');
      const branches = [...repo.refs.entries()]
        .filter(([refName]) => refName.startsWith('refs/heads/'))
        .map(([refName, oid]) => ({ name: refName.slice('refs/heads/'.length), commit: { sha: oid } }))
        .sort((a, b) => (a.name < b.name ? -1 : 1));
      return ok(branches);
    }
    const branch = rest.slice(1).join('/');
    const oid = repo.refs.get(`refs/heads/${branch}`);
    if (oid === undefined) return err(404, 'Not Found');
    if (method !== 'GET') return err(404, 'Not Found');
    return ok({ name: branch, commit: { sha: oid } });
  }

  if (section === 'git' && rest[1] === 'refs') {
    if (repo === undefined) return err(404, 'Not Found');
    if (method === 'POST' && rest.length === 2) {
      if (!isRecord(body)) return err(422, 'a ref and sha are required');
      const ref = asString(body.ref);
      const sha = asString(body.sha);
      if (ref === null || !ref.startsWith('refs/') || sha === null || !G.isOid(sha)) {
        return err(422, 'a ref under refs/ and a 40-hex sha are required');
      }
      if (repo.refs.has(ref)) return err(422, 'Reference already exists');
      const object = repo.objects.get(sha);
      if (object === undefined) return err(422, 'the sha names no object in this repository');
      repo.refs.set(ref, sha);
      return ok({ ref, object: { sha, type: object.type } }, 201);
    }
    if (method === 'GET') {
      if (rest.length === 2) {
        const refs = [...repo.refs.entries()]
          .map(([refName, oid]) => ({ ref: refName, object: { sha: oid, type: repo.objects.get(oid)?.type ?? 'commit' } }))
          .sort((a, b) => (a.ref < b.ref ? -1 : 1));
        return ok(refs);
      }
      const refName = `refs/${rest.slice(2).join('/')}`;
      const oid = repo.refs.get(refName);
      if (oid === undefined) return err(404, 'Not Found');
      return ok({ ref: refName, object: { sha: oid, type: repo.objects.get(oid)?.type ?? 'commit' } });
    }
    return err(404, 'Not Found');
  }

  if (section === 'commits') {
    if (repo === undefined) return err(404, 'Not Found');
    if (method !== 'GET') return err(404, 'Not Found');
    const tip = resolveCommit(store, repo, query.get('sha'));
    const out: Array<Record<string, unknown>> = [];
    let current = tip;
    const seen = new Set<string>();
    while (current !== null && !seen.has(current) && out.length < 100) {
      seen.add(current);
      const info = store.commitInfo(repo, current);
      if (info === null) break;
      const author = parseIdent(info.author);
      const committer = parseIdent(info.committer);
      out.push({
        sha: current,
        commit: {
          message: info.message,
          author: { name: author.name, email: author.email, date: author.date },
          committer: { name: committer.name, email: committer.email, date: committer.date },
          tree: { sha: info.tree },
        },
        parents: info.parents.map((parent) => ({ sha: parent })),
      });
      current = info.parents[0] ?? null;
    }
    return ok(out);
  }

  if (section === 'contents') {
    if (repo === undefined) return err(404, 'Not Found');
    const path = rest.slice(1).join('/');
    if (method === 'GET') {
      const tip = resolveCommit(store, repo, query.get('ref'));
      if (tip === null) return err(404, 'Not Found');
      const info = store.commitInfo(repo, tip);
      if (info === null) return err(404, 'Not Found');
      if (path === '') {
        const entries = store.treeEntries(repo, info.tree) ?? [];
        return ok(
          entries.map((entry) => ({
            type: entry.mode === '40000' ? 'dir' : 'file',
            name: entry.name,
            path: entry.name,
            sha: entry.oid,
            size: entry.mode === '40000' ? 0 : (repo.objects.get(entry.oid)?.data.length ?? 0),
          })),
        );
      }
      const segments = path.split('/');
      const found = lookupPath(store, repo, info.tree, segments);
      if (found === null) return err(404, 'Not Found');
      if (found.type === 'tree') {
        const entries = store.treeEntries(repo, found.oid) ?? [];
        return ok(
          entries.map((entry) => ({
            type: entry.mode === '40000' ? 'dir' : 'file',
            name: entry.name,
            path: `${path}/${entry.name}`,
            sha: entry.oid,
            size: entry.mode === '40000' ? 0 : (repo.objects.get(entry.oid)?.data.length ?? 0),
          })),
        );
      }
      const object = repo.objects.get(found.oid);
      if (object === undefined) return err(404, 'Not Found');
      return ok({
        type: 'file',
        encoding: 'base64',
        size: object.data.length,
        name: segments[segments.length - 1]!,
        path,
        content: object.data.toString('base64'),
        sha: found.oid,
      });
    }
    if (method === 'PUT') {
      if (!isRecord(body)) return err(422, 'a JSON object body is required');
      const content = asString(body.content);
      const message = asString(body.message);
      if (content === null || message === null) return err(422, 'message and base64 content are required');
      const branch = asString(body.branch);
      const sha = asString(body.sha);
      const bytes = Buffer.from(content.replace(/[\r\n\t ]+/g, ''), 'base64');
      const result = contentsCommit(store, repo, branch, path, message, bytes, sha);
      if (result.status === 409 || result.status === 404) return err(result.status, result.message ?? 'Conflict');
      return ok(
        {
          content: {
            name: path.split('/').pop() ?? path,
            path,
            sha: result.contentSha,
            size: bytes.length,
            type: 'file',
            encoding: 'base64',
          },
          commit: { sha: result.commitSha, message },
        },
        result.created === true ? 201 : 200,
      );
    }
    if (method === 'DELETE') {
      if (!isRecord(body)) return err(422, 'a JSON object body is required');
      const message = asString(body.message) ?? '';
      const branch = asString(body.branch);
      const sha = asString(body.sha);
      const result = contentsCommit(store, repo, branch, path, message, null, sha);
      if (result.status === 409 || result.status === 404) return err(result.status, result.message ?? 'Conflict');
      return ok({ commit: { sha: result.commitSha, message } });
    }
    return err(404, 'Not Found');
  }

  if (section === 'labels') {
    if (repo === undefined) return err(404, 'Not Found');
    if (method !== 'POST') return err(404, 'Not Found');
    if (!isRecord(body)) return err(422, 'a label name and color are required');
    const name = asString(body.name);
    const color = asString(body.color) ?? 'ededed';
    if (name === null || name === '') return err(422, 'a label name is required');
    const label = store.createLabel(repo, name, color);
    if (label === null) return err(422, 'Label already exists');
    return ok(labelJson(label), 201);
  }

  if (section === 'issues') {
    if (repo === undefined) return err(404, 'Not Found');
    if (rest.length === 1) {
      if (method === 'POST') {
        if (!isRecord(body)) return err(422, 'a title is required');
        const title = asString(body.title);
        if (title === null || title === '') return err(422, 'a title is required');
        const bodyText = asString(body.body) ?? '';
        const labels = Array.isArray(body.labels) ? body.labels.filter((item): item is string => typeof item === 'string') : [];
        for (const label of labels) {
          if (!repo.labels.has(label)) return err(422, `Label does not exist: ${label}`);
        }
        const issue = store.createIssue(repo, title, bodyText, labels);
        return ok(issueJson(repo, issue), 201);
      }
      if (method === 'GET') {
        const state = query.get('state') ?? 'open';
        const direction = query.get('direction') ?? 'desc';
        const sort = query.get('sort') ?? 'created';
        let list = [...repo.issues.values()];
        if (state === 'open') list = list.filter((issue) => issue.state === 'open');
        else if (state === 'closed') list = list.filter((issue) => issue.state === 'closed');
        if (sort === 'created') {
          list.sort((a, b) => (direction === 'asc' ? a.seq - b.seq : b.seq - a.seq));
        }
        const { page, headers } = paginate(list.map((issue) => issueJson(repo, issue)), query, `${base}/api/v3/repos/${owner}/${name}/issues`);
        return ok(page, 200, headers);
      }
      return err(404, 'Not Found');
    }
    const number = parseInt(rest[1]!, 10);
    if (!Number.isInteger(number) || number < 1) return err(404, 'Not Found');
    const issue = repo.issues.get(number);
    if (rest.length === 2) {
      if (issue === undefined) return err(404, 'Not Found');
      if (method === 'GET') return ok(issueJson(repo, issue));
      if (method === 'PATCH') {
        if (!isRecord(body)) return err(422, 'a JSON object body is required');
        const state = asString(body.state);
        if (state === 'open' || state === 'closed') issue.state = state;
        const title = asString(body.title);
        if (title !== null) issue.title = title;
        const bodyText = asString(body.body);
        if (bodyText !== null) issue.body = bodyText;
        return ok(issueJson(repo, issue));
      }
      return err(404, 'Not Found');
    }
    if (rest[2] === 'comments') {
      if (issue === undefined) return err(404, 'Not Found');
      if (method === 'POST') {
        if (!isRecord(body)) return err(422, 'a comment body is required');
        const text = asString(body.body);
        if (text === null) return err(422, 'a comment body is required');
        const comment = store.createComment(repo, issue, text);
        return ok(commentJson(repo, issue, comment), 201);
      }
      if (method === 'GET') {
        return ok(issue.comments.map((comment) => commentJson(repo, issue, comment)));
      }
      return err(404, 'Not Found');
    }
    return err(404, 'Not Found');
  }

  if (section === 'pulls') {
    if (repo === undefined) return err(404, 'Not Found');
    if (rest.length === 1) {
      if (method === 'POST') {
        if (!isRecord(body)) return err(422, 'head, base and title are required');
        const head = asString(body.head);
        const baseBranch = asString(body.base);
        const title = asString(body.title);
        if (head === null || baseBranch === null || title === null || title === '') {
          return err(422, 'head, base and title are required');
        }
        const bodyText = asString(body.body) ?? '';
        const headSha = repo.refs.get(`refs/heads/${head}`);
        const baseSha = repo.refs.get(`refs/heads/${baseBranch}`);
        if (headSha === undefined || baseSha === undefined) return err(422, 'head and base must name existing branches');
        for (const existing of repo.pulls.values()) {
          if (existing.state === 'open' && existing.headRef === head && existing.baseRef === baseBranch) {
            return err(422, 'A pull request already exists for this head and base');
          }
        }
        const pull = store.createPull(repo, head, baseBranch, headSha, baseSha, title, bodyText);
        return ok(pullJson(repo, pull), 201);
      }
      if (method === 'GET') {
        const state = query.get('state') ?? 'open';
        let list = [...repo.pulls.values()];
        if (state === 'open') list = list.filter((pull) => pull.state === 'open');
        else if (state === 'closed') list = list.filter((pull) => pull.state === 'closed');
        list.sort((a, b) => b.seq - a.seq);
        const { page, headers } = paginate(list.map((pull) => pullJson(repo, pull)), query, `${base}/api/v3/repos/${owner}/${name}/pulls`);
        return ok(page, 200, headers);
      }
      return err(404, 'Not Found');
    }
    const number = parseInt(rest[1]!, 10);
    if (!Number.isInteger(number) || number < 1) return err(404, 'Not Found');
    const pull = repo.pulls.get(number);
    if (rest.length === 2) {
      if (pull === undefined) return err(404, 'Not Found');
      if (method !== 'GET') return err(404, 'Not Found');
      return ok(pullJson(repo, pull));
    }
    if (rest[2] === 'files') {
      if (pull === undefined) return err(404, 'Not Found');
      if (method !== 'GET') return err(404, 'Not Found');
      return ok(store.pullFiles(repo, pull));
    }
    if (rest[2] === 'merge') {
      if (pull === undefined) return err(404, 'Not Found');
      if (method !== 'PUT') return err(404, 'Not Found');
      if (pull.state !== 'open') return err(405, 'Pull request is already closed');
      const merged = store.squashMerge(repo, pull);
      if (!merged.ok) {
        if (merged.reason === 'conflict') return err(405, 'Merge conflict');
        return err(405, 'Pull request cannot be merged');
      }
      return ok({ sha: merged.sha, merged: true, message: 'Pull request successfully merged' });
    }
    return err(404, 'Not Found');
  }

  return err(404, 'Not Found');
}
