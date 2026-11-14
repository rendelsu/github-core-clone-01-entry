// store.ts — the forge's whole state, in memory. One process holds one store;
// every trial starts a fresh process, so nothing here is ever persisted or read
// back from disk. Ids are sequential counters and timestamps come from a logical
// clock (a fixed epoch plus a sequence), so two runs that see the same requests
// build byte-identical state.

import { deflateSync } from 'node:zlib';
import * as G from './gitobj.ts';

/** The token owner's login: the scenario's fixed viewer (see wire.ts buildScenario). */
export const VIEWER_LOGIN = 'adaiversen';
export const VIEWER_ID = 1000;
export const VIEWER_EMAIL = 'adaiversen@example.invalid';
export const VIEWER_IDENT = `${VIEWER_LOGIN} <${VIEWER_EMAIL}>`;
export const STRANGER_LOGIN = 'olgabauer';
export const DEFAULT_BRANCH = 'main';

/** The fixed epoch every generated timestamp derives from (2025-01-01T00:00:00Z). */
export const EPOCH_SEC = 1735689600;
/** The fixed epoch commit objects created through the API carry. */
export const COMMIT_EPOCH_SEC = 1700000000;

export function isoTime(seq: number): string {
  return new Date((EPOCH_SEC + seq) * 1000).toISOString();
}

export interface Label {
  id: number;
  name: string;
  color: string;
  description: string;
}

export interface Comment {
  id: number;
  body: string;
  seq: number;
}

export interface Issue {
  number: number;
  id: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  comments: Comment[];
  seq: number;
}

export interface Pull {
  number: number;
  id: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  headRef: string;
  baseRef: string;
  headSha: string;
  baseSha: string;
  seq: number;
}

export interface Repo {
  id: number;
  name: string;
  owner: string;
  priv: boolean;
  description: string;
  defaultBranch: string;
  seq: number;
  /** ref name -> commit/tag oid. */
  refs: Map<string, string>;
  /** oid -> object. */
  objects: Map<string, G.GitObject>;
  /** oid -> cached zlib stream of the object's bytes (objects never change). */
  deflateCache: Map<string, Buffer>;
  /** oid -> parsed commit (cache; commits never change). */
  commitCache: Map<string, G.CommitInfo>;
  /** oid -> parsed tree entries (cache; trees never change). */
  treeCache: Map<string, G.TreeEntry[]>;
  labels: Map<string, Label>;
  labelSeq: number;
  issues: Map<number, Issue>;
  pulls: Map<number, Pull>;
  /** The shared issue + pull-request counter (GitHub numbers both from one counter). */
  nextNumber: number;
  commentSeq: number;
  /** Logical clock for commits this forge creates through the API. */
  commitSeq: number;
  /** Back-reference to the owning Store (used by the GraphQL layer). */
  __store?: Store;
}

export class Store {
  repos = new Map<string, Repo>();
  repoIdSeq = 0;
  issueIdSeq = 0;
  pullIdSeq = 0;
  globalSeq = 0;

  repoKey(owner: string, name: string): string {
    return `${owner}/${name}`;
  }

  createRepo(name: string, priv: boolean, description: string): Repo | null {
    const key = this.repoKey(VIEWER_LOGIN, name);
    if (this.repos.has(key)) return null;
    const repo: Repo = {
      id: ++this.repoIdSeq,
      name,
      owner: VIEWER_LOGIN,
      priv,
      description,
      defaultBranch: DEFAULT_BRANCH,
      seq: ++this.globalSeq,
      refs: new Map(),
      objects: new Map(),
      deflateCache: new Map(),
      commitCache: new Map(),
      treeCache: new Map(),
      labels: new Map(),
      labelSeq: 0,
      issues: new Map(),
      pulls: new Map(),
      nextNumber: 1,
      commentSeq: 0,
      commitSeq: 0,
      __store: this,
    };
    this.repos.set(key, repo);
    return repo;
  }

  /** A repository under the viewer; any other owner is a 404 (never a 403). */
  getRepo(owner: string, name: string): Repo | undefined {
    if (owner !== VIEWER_LOGIN) return undefined;
    return this.repos.get(this.repoKey(owner, name));
  }

  deleteRepo(repo: Repo): void {
    this.repos.delete(this.repoKey(repo.owner, repo.name));
  }

  /** Every repository the viewer owns, in creation order. */
  listRepos(): Repo[] {
    return [...this.repos.values()].sort((a, b) => a.seq - b.seq);
  }

  /** All repositories keyed by their numeric id (for global-id lookups). */
  reposById(): Map<number, Repo> {
    const out = new Map<number, Repo>();
    for (const repo of this.repos.values()) out.set(repo.id, repo);
    return out;
  }

  // -------------------------------------------------------------------------
  // objects
  // -------------------------------------------------------------------------

  /** Store an object (idempotent) and return its id. */
  putObject(repo: Repo, type: G.ObjType, data: Buffer): string {
    const oid = G.hashObject(type, data);
    if (!repo.objects.has(oid)) repo.objects.set(oid, { type, data });
    return oid;
  }

  getObject(repo: Repo, oid: string): G.GitObject | undefined {
    return repo.objects.get(oid);
  }

  /** A commit's parsed headers, cached (commits are immutable). */
  commitInfo(repo: Repo, oid: string): G.CommitInfo | null {
    const cached = repo.commitCache.get(oid);
    if (cached !== undefined) return cached;
    const object = repo.objects.get(oid);
    if (object === undefined || object.type !== 'commit') return null;
    const parsed = G.parseCommit(object.data);
    repo.commitCache.set(oid, parsed);
    return parsed;
  }

  /** A tree's entries, cached (trees are immutable). */
  treeEntries(repo: Repo, oid: string): G.TreeEntry[] | null {
    const cached = repo.treeCache.get(oid);
    if (cached !== undefined) return cached;
    const object = repo.objects.get(oid);
    if (object === undefined || object.type !== 'tree') return null;
    const parsed = G.parseTree(object.data);
    repo.treeCache.set(oid, parsed);
    return parsed;
  }

  /** A deflated copy of an object's bytes, cached (objects are immutable). */
  deflated(repo: Repo, oid: string): Buffer | null {
    const cached = repo.deflateCache.get(oid);
    if (cached !== undefined) return cached;
    const object = repo.objects.get(oid);
    if (object === undefined) return null;
    const deflated = deflateSync(object.data);
    repo.deflateCache.set(oid, deflated);
    return deflated;
  }

  // -------------------------------------------------------------------------
  // reachability
  // -------------------------------------------------------------------------

  /**
   * Every object reachable from `roots`. `depth` limits the commit generations
   * (1 = the roots only, no parents); trees and blobs of an included commit are
   * always included. A tag root includes the tag and its target at the same depth.
   * Commits in `cut` are included with their trees, but their parents are not
   * followed: that is how a shallow client's `have` lines behave, because the
   * client holds the boundary commit but none of its history.
   */
  reachable(
    repo: Repo,
    roots: Iterable<string>,
    depth: number = Number.POSITIVE_INFINITY,
    cut: ReadonlySet<string> = new Set(),
  ): Set<string> {
    const seen = new Set<string>();
    const queue: Array<{ oid: string; dist: number }> = [];
    for (const root of roots) {
      if (G.isOid(root)) queue.push({ oid: root, dist: 0 });
    }
    while (queue.length > 0) {
      const item = queue.pop()!;
      if (seen.has(item.oid)) continue;
      const object = repo.objects.get(item.oid);
      if (object === undefined) continue;
      seen.add(item.oid);
      if (object.type === 'commit') {
        const info = this.commitInfo(repo, item.oid);
        if (info === null) continue;
        // the commit's own tree always travels with it, whatever the depth
        queue.push({ oid: info.tree, dist: item.dist });
        if (item.dist + 1 >= depth) continue;
        if (cut.has(item.oid)) continue;
        for (const parent of info.parents) queue.push({ oid: parent, dist: item.dist + 1 });
      } else if (object.type === 'tag') {
        const tag = G.parseTag(object.data);
        queue.push({ oid: tag.object, dist: item.dist });
      } else if (object.type === 'tree') {
        const entries = this.treeEntries(repo, item.oid);
        if (entries === null) continue;
        for (const entry of entries) {
          if (entry.mode === '160000') continue; // a gitlink names a commit the repo does not hold
          queue.push({ oid: entry.oid, dist: item.dist });
        }
      }
    }
    return seen;
  }

  /**
   * The commits reachable from `roots` at each distance (0 = the roots), with no
   * depth limit. Used for shallow-boundary bookkeeping. `maxDist` caps the walk:
   * only entries at distance <= maxDist are recorded. The shallow-boundary code
   * only ever reads entries at distance depth-1 or below, so a fetch with a
   * finite depth does not need the whole history walked. A merge commit inside
   * the walked region makes first-discovery distances order-dependent past the
   * cutoff, so in that case the full walk is run instead (exact either way).
   */
  commitDistances(repo: Repo, roots: Iterable<string>, maxDist: number = Number.POSITIVE_INFINITY): Map<string, number> {
    const run = (limit: number): { dist: Map<string, number>; sawMerge: boolean } => {
      const dist = new Map<string, number>();
      const queue: string[] = [];
      let sawMerge = false;
      for (const root of roots) {
        if (!G.isOid(root) || dist.has(root)) continue;
        const object = repo.objects.get(root);
        if (object === undefined) continue;
        if (object.type === 'tag') {
          const tag = G.parseTag(object.data);
          if (!dist.has(tag.object)) queue.push(tag.object);
          continue;
        }
        if (object.type !== 'commit') continue;
        dist.set(root, 0);
        queue.push(root);
      }
      while (queue.length > 0) {
        const oid = queue.pop()!;
        const current = dist.get(oid)!;
        if (current >= limit) continue;
        const info = this.commitInfo(repo, oid);
        if (info === null) continue;
        if (info.parents.length > 1) sawMerge = true;
        for (const parent of info.parents) {
          if (!dist.has(parent) && repo.objects.has(parent)) {
            dist.set(parent, current + 1);
            queue.push(parent);
          }
        }
      }
      return { dist, sawMerge };
    };
    const bounded = run(maxDist);
    if (maxDist === Number.POSITIVE_INFINITY || !bounded.sawMerge) return bounded.dist;
    return run(Number.POSITIVE_INFINITY).dist;
  }

  /** True when `ancestor` is an ancestor of `descendant` (or the same commit). */
  isAncestor(repo: Repo, ancestor: string, descendant: string): boolean {
    if (ancestor === descendant) return true;
    const seen = new Set<string>();
    const queue: string[] = [descendant];
    while (queue.length > 0) {
      const oid = queue.pop()!;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const info = this.commitInfo(repo, oid);
      if (info === null) continue;
      for (const parent of info.parents) {
        if (parent === ancestor) return true;
        queue.push(parent);
      }
    }
    return false;
  }

  /** The merge-base of two commits: the first commit reachable from both. */
  mergeBase(repo: Repo, a: string, b: string): string | null {
    const ancestorsA = new Set<string>();
    const queueA: string[] = [a];
    while (queueA.length > 0) {
      const oid = queueA.pop()!;
      if (ancestorsA.has(oid)) continue;
      ancestorsA.add(oid);
      const info = this.commitInfo(repo, oid);
      if (info === null) continue;
      for (const parent of info.parents) queueA.push(parent);
    }
    const queueB: string[] = [b];
    const seenB = new Set<string>();
    while (queueB.length > 0) {
      const oid = queueB.pop()!;
      if (seenB.has(oid)) continue;
      seenB.add(oid);
      if (ancestorsA.has(oid)) return oid;
      const info = this.commitInfo(repo, oid);
      if (info === null) continue;
      for (const parent of info.parents) queueB.push(parent);
    }
    return null;
  }

  // -------------------------------------------------------------------------
  // trees as flat path maps
  // -------------------------------------------------------------------------

  /** Flatten a tree into path -> {oid, mode}, recursing into directories. */
  flattenTree(repo: Repo, treeOid: string): Map<string, { oid: string; mode: string }> {
    const flat = new Map<string, { oid: string; mode: string }>();
    const walk = (oid: string, prefix: string): void => {
      const entries = this.treeEntries(repo, oid);
      if (entries === null) return;
      for (const entry of entries) {
        const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
        if (entry.mode === '40000') walk(entry.oid, path);
        else flat.set(path, { oid: entry.oid, mode: entry.mode });
      }
    };
    walk(treeOid, '');
    return flat;
  }

  /** Build (and store) nested trees from a flat path map; returns the root tree id. */
  buildTreeFromFlat(repo: Repo, flat: ReadonlyMap<string, { oid: string; mode: string }>): string {
    interface Node {
      files: Map<string, { oid: string; mode: string }>;
      dirs: Map<string, Node>;
    }
    const make = (): Node => ({ files: new Map(), dirs: new Map() });
    const root = make();
    for (const [path, value] of flat) {
      const segments = path.split('/');
      let node = root;
      for (let index = 0; index < segments.length - 1; index += 1) {
        const name = segments[index]!;
        let child = node.dirs.get(name);
        if (child === undefined) {
          child = make();
          node.dirs.set(name, child);
        }
        node = child;
      }
      node.files.set(segments[segments.length - 1]!, value);
    }
    const build = (node: Node): string => {
      const entries: G.TreeEntry[] = [];
      for (const [name, value] of node.files) entries.push({ mode: value.mode, name, oid: value.oid });
      for (const [name, child] of node.dirs) entries.push({ mode: '40000', name, oid: build(child) });
      return this.putObject(repo, 'tree', G.buildTree(entries));
    };
    return build(root);
  }

  /** Create a commit object on `parents` with `tree` and store it. */
  createCommit(
    repo: Repo,
    tree: string,
    parents: readonly string[],
    message: string,
  ): string {
    repo.commitSeq += 1;
    const timestamp = COMMIT_EPOCH_SEC + repo.commitSeq;
    const data = G.buildCommit(tree, parents, VIEWER_IDENT, VIEWER_IDENT, timestamp, message);
    return this.putObject(repo, 'commit', data);
  }

  // -------------------------------------------------------------------------
  // diffs and merges
  // -------------------------------------------------------------------------

  /** A line diff: additions and deletions between two texts (small contents). */
  lineDiff(oldText: string, newText: string): { additions: number; deletions: number } {
    const oldLines = oldText === '' ? [] : oldText.split('\n');
    const newLines = newText === '' ? [] : newText.split('\n');
    // Longest common subsequence on lines; contents here are tiny.
    const rows = oldLines.length + 1;
    const cols = newLines.length + 1;
    const table = new Uint32Array(rows * cols);
    for (let i = 1; i < rows; i += 1) {
      for (let j = 1; j < cols; j += 1) {
        table[i * cols + j] =
          oldLines[i - 1] === newLines[j - 1]
            ? table[(i - 1) * cols + (j - 1)]! + 1
            : Math.max(table[(i - 1) * cols + j]!, table[i * cols + (j - 1)]!);
      }
    }
    const common = table[oldLines.length * cols + newLines.length]!;
    return { additions: newLines.length - common, deletions: oldLines.length - common };
  }

  /** The changed paths between two trees: added, removed or modified. */
  diffTrees(
    repo: Repo,
    baseTree: string,
    headTree: string,
  ): Array<{ path: string; status: 'added' | 'removed' | 'modified'; oldOid: string | null; newOid: string | null }> {
    const base = this.flattenTree(repo, baseTree);
    const head = this.flattenTree(repo, headTree);
    const out: Array<{ path: string; status: 'added' | 'removed' | 'modified'; oldOid: string | null; newOid: string | null }> = [];
    for (const [path, value] of head) {
      const old = base.get(path);
      if (old === undefined) out.push({ path, status: 'added', oldOid: null, newOid: value.oid });
      else if (old.oid !== value.oid) out.push({ path, status: 'modified', oldOid: old.oid, newOid: value.oid });
    }
    for (const [path, value] of base) {
      if (!head.has(path)) out.push({ path, status: 'removed', oldOid: value.oid, newOid: null });
    }
    out.sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));
    return out;
  }

  /**
   * A three-way merge of `headTip` into `baseTip` over their merge-base.
   * Returns the merged flat tree, or `conflict: true` when both sides changed
   * the same path differently.
   */
  threeWayMerge(
    repo: Repo,
    baseTip: string,
    headTip: string,
  ): { conflict: boolean; tree: Map<string, { oid: string; mode: string }> | null } {
    const baseInfo = this.commitInfo(repo, baseTip);
    const headInfo = this.commitInfo(repo, headTip);
    if (baseInfo === null || headInfo === null) return { conflict: true, tree: null };
    const mergeBase = this.mergeBase(repo, baseTip, headTip);
    if (mergeBase === null) return { conflict: true, tree: null };
    const baseBaseInfo = this.commitInfo(repo, mergeBase);
    if (baseBaseInfo === null) return { conflict: true, tree: null };
    const baseFlat = this.flattenTree(repo, baseInfo.tree);
    const headFlat = this.flattenTree(repo, headInfo.tree);
    const mergeFlat = this.flattenTree(repo, baseBaseInfo.tree);
    const result = new Map<string, { oid: string; mode: string }>(baseFlat);
    const paths = new Set<string>([...mergeFlat.keys(), ...headFlat.keys(), ...baseFlat.keys()]);
    for (const path of paths) {
      const ancestor = mergeFlat.get(path);
      const head = headFlat.get(path);
      const current = baseFlat.get(path);
      const headChanged = (head?.oid ?? null) !== (ancestor?.oid ?? null);
      const currentChanged = (current?.oid ?? null) !== (ancestor?.oid ?? null);
      if (!headChanged) continue; // the head did not touch this path: keep the base side
      if (!currentChanged) {
        // only the head changed it: take the head's version (or its deletion)
        if (head === undefined) result.delete(path);
        else result.set(path, head);
        continue;
      }
      // both sides changed it: fine when they agree, a conflict otherwise
      if ((head?.oid ?? null) === (current?.oid ?? null)) continue;
      return { conflict: true, tree: null };
    }
    return { conflict: false, tree: result };
  }

  // -------------------------------------------------------------------------
  // issues, pull requests, labels, comments
  // -------------------------------------------------------------------------

  createLabel(repo: Repo, name: string, color: string): Label | null {
    if (repo.labels.has(name)) return null;
    const label: Label = { id: ++repo.labelSeq, name, color, description: '' };
    repo.labels.set(name, label);
    return label;
  }

  createIssue(repo: Repo, title: string, body: string, labels: readonly string[]): Issue {
    const number = repo.nextNumber;
    repo.nextNumber += 1;
    const issue: Issue = {
      number,
      id: ++this.issueIdSeq,
      title,
      body,
      state: 'open',
      labels: [...labels],
      comments: [],
      seq: ++this.globalSeq,
    };
    repo.issues.set(number, issue);
    return issue;
  }

  createComment(repo: Repo, issue: Issue, body: string): Comment {
    const comment: Comment = { id: ++repo.commentSeq, body, seq: ++this.globalSeq };
    issue.comments.push(comment);
    return comment;
  }

  createPull(
    repo: Repo,
    headRef: string,
    baseRef: string,
    headSha: string,
    baseSha: string,
    title: string,
    body: string,
  ): Pull {
    const number = repo.nextNumber;
    repo.nextNumber += 1;
    const pull: Pull = {
      number,
      id: ++this.pullIdSeq,
      title,
      body,
      state: 'open',
      merged: false,
      mergedAt: null,
      mergeCommitSha: null,
      headRef,
      baseRef,
      headSha,
      baseSha,
      seq: ++this.globalSeq,
    };
    repo.pulls.set(number, pull);
    return pull;
  }

  /** A pull request's current head/base tips (branches move; the PR follows them). */
  pullTips(repo: Repo, pull: Pull): { headSha: string | null; baseSha: string | null } {
    return {
      headSha: repo.refs.get(`refs/heads/${pull.headRef}`) ?? pull.headSha,
      baseSha: repo.refs.get(`refs/heads/${pull.baseRef}`) ?? pull.baseSha,
    };
  }

  /** Whether a pull request merges cleanly: MERGEABLE, CONFLICTING or UNKNOWN. */
  pullMergeable(repo: Repo, pull: Pull): 'MERGEABLE' | 'CONFLICTING' | 'UNKNOWN' {
    if (pull.state !== 'open') return 'UNKNOWN';
    const tips = this.pullTips(repo, pull);
    if (tips.headSha === null || tips.baseSha === null) return 'UNKNOWN';
    if (this.isAncestor(repo, tips.headSha, tips.baseSha)) return 'MERGEABLE';
    const merged = this.threeWayMerge(repo, tips.baseSha, tips.headSha);
    return merged.conflict ? 'CONFLICTING' : 'MERGEABLE';
  }

  /** The files a pull request changes (merge-base tree vs head tree). */
  pullFiles(
    repo: Repo,
    pull: Pull,
  ): Array<{ filename: string; status: 'added' | 'removed' | 'modified'; additions: number; deletions: number; changes: number }> {
    const tips = this.pullTips(repo, pull);
    if (tips.headSha === null || tips.baseSha === null) return [];
    const headInfo = this.commitInfo(repo, tips.headSha);
    if (headInfo === null) return [];
    const mergeBase = this.mergeBase(repo, tips.baseSha, tips.headSha);
    if (mergeBase === null) return [];
    const baseInfo = this.commitInfo(repo, mergeBase);
    if (baseInfo === null) return [];
    const diffs = this.diffTrees(repo, baseInfo.tree, headInfo.tree);
    const out: Array<{ filename: string; status: 'added' | 'removed' | 'modified'; additions: number; deletions: number; changes: number }> = [];
    for (const diff of diffs) {
      let additions = 0;
      let deletions = 0;
      const oldText = diff.oldOid === null ? '' : (repo.objects.get(diff.oldOid)?.data.toString('utf8') ?? '');
      const newText = diff.newOid === null ? '' : (repo.objects.get(diff.newOid)?.data.toString('utf8') ?? '');
      if (diff.status === 'added') {
        additions = newText === '' ? 0 : newText.split('\n').length;
      } else if (diff.status === 'removed') {
        deletions = oldText === '' ? 0 : oldText.split('\n').length;
      } else {
        const counted = this.lineDiff(oldText, newText);
        additions = counted.additions;
        deletions = counted.deletions;
        if (additions === 0 && deletions === 0) additions = 1; // a changed file always adds at least one line
      }
      out.push({ filename: diff.path, status: diff.status, additions, deletions, changes: additions + deletions });
    }
    return out;
  }

  /**
   * Squash-merge an open pull request onto its base branch. Returns the merge
   * commit id, or an error kind the caller maps to a status.
   */
  squashMerge(repo: Repo, pull: Pull): { ok: true; sha: string } | { ok: false; reason: 'conflict' | 'unknown' | 'closed' } {
    if (pull.state !== 'open') return { ok: false, reason: 'closed' };
    const tips = this.pullTips(repo, pull);
    if (tips.headSha === null || tips.baseSha === null) return { ok: false, reason: 'unknown' };
    if (this.isAncestor(repo, tips.headSha, tips.baseSha)) {
      // already contained in the base: nothing to merge, report the base tip
      return { ok: true, sha: tips.baseSha };
    }
    const merged = this.threeWayMerge(repo, tips.baseSha, tips.headSha);
    if (merged.conflict || merged.tree === null) return { ok: false, reason: 'conflict' };
    const tree = this.buildTreeFromFlat(repo, merged.tree);
    const sha = this.createCommit(repo, tree, [tips.baseSha], pull.title);
    repo.refs.set(`refs/heads/${pull.baseRef}`, sha);
    pull.state = 'closed';
    pull.merged = true;
    pull.mergedAt = isoTime(++this.globalSeq);
    pull.mergeCommitSha = sha;
    return { ok: true, sha };
  }
}
