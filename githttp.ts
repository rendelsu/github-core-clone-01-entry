// githttp.ts — the git smart-HTTP surface: ref advertisements (v0 and v2),
// ls-refs, fetch (v2 sections and the v0 sideband form) and receive-pack pushes
// with per-ref report-status. All state lives in the Store; every response is
// built from the repo's refs and objects.

import * as G from './gitobj.ts';
import { Store, type Repo } from './store.ts';

export const AGENT = 'github-core-clone/1.0';

const UPLOAD_CAPS = `multi_ack thin-pack side-band-64k ofs-delta shallow symref=HEAD:refs/heads/main agent=${AGENT}`;
const RECEIVE_CAPS = `report-status side-band-64k delete-refs ofs-delta agent=${AGENT}`;

/** Refs in advertisement order: HEAD first, then heads, then tags, each sorted. */
function sortedRefs(repo: Repo): Array<{ name: string; oid: string }> {
  const heads: Array<{ name: string; oid: string }> = [];
  const tags: Array<{ name: string; oid: string }> = [];
  for (const [name, oid] of repo.refs) {
    if (name.startsWith('refs/heads/')) heads.push({ name, oid });
    else if (name.startsWith('refs/tags/')) tags.push({ name, oid });
    else heads.push({ name, oid });
  }
  heads.sort((a, b) => (a.name < b.name ? -1 : 1));
  tags.sort((a, b) => (a.name < b.name ? -1 : 1));
  return [...heads, ...tags];
}

/** The peeled commit of an annotated tag ref, or null. */
function peeledOf(repo: Repo, oid: string): string | null {
  const object = repo.objects.get(oid);
  if (object === undefined || object.type !== 'tag') return null;
  const tag = G.parseTag(object.data);
  const target = repo.objects.get(tag.object);
  if (target === undefined) return null;
  if (target.type === 'tag') return peeledOf(repo, tag.object);
  return tag.object;
}

/** The v0 advertisement body for a service. */
export function v0Advertisement(repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const caps = service === 'git-upload-pack' ? UPLOAD_CAPS : RECEIVE_CAPS;
  const lines: Array<Buffer | string | 'flush'> = [`# service=${service}\n`];
  const refs = sortedRefs(repo);
  if (refs.length === 0) {
    lines.push(`${G.ZERO_OID} capabilities^{}\0${caps}\n`);
  } else {
    const headOid = repo.refs.get('refs/heads/main') ?? refs[0]!.oid;
    lines.push(`${headOid} HEAD\0${caps}\n`);
    for (const ref of refs) {
      lines.push(`${ref.oid} ${ref.name}\n`);
      const peeled = peeledOf(repo, ref.oid);
      if (peeled !== null) lines.push(`${peeled} ${ref.name}^{}\n`);
    }
  }
  lines.push('flush');
  return G.encodePktLines(lines);
}

/** The v2 capability advertisement body. */
export function v2Advertisement(): Buffer {
  return G.encodePktLines([
    'version 2\n',
    `agent=${AGENT}\n`,
    'ls-refs=unborn\n',
    'fetch=shallow wait-for-done\n',
    'object-format=sha1\n',
    'flush',
  ]);
}

/** The lines of a v2 request body, as text (control packets dropped). */
function requestLines(body: Buffer): string[] {
  const { packets } = G.decodePktLines(body);
  const lines: string[] = [];
  for (const packet of packets) {
    if (packet.kind === 'data') lines.push(packet.payload.toString('utf8'));
  }
  return lines;
}

interface LsRefsArgs {
  peel: boolean;
  symrefs: boolean;
  unborn: boolean;
  prefixes: string[];
}

/** The v2 ls-refs answer. */
export function lsRefsResponse(repo: Repo, body: Buffer): Buffer {
  const lines = requestLines(body);
  const args: LsRefsArgs = { peel: false, symrefs: false, unborn: false, prefixes: [] };
  for (const line of lines) {
    const text = line.replace(/\n$/, '');
    if (text === 'peel') args.peel = true;
    else if (text === 'symrefs') args.symrefs = true;
    else if (text === 'unborn') args.unborn = true;
    else if (text.startsWith('ref-prefix ')) args.prefixes.push(text.slice('ref-prefix '.length));
  }
  const out: Array<Buffer | string | 'flush'> = [];
  const keep = (name: string): boolean =>
    args.prefixes.length === 0 || args.prefixes.some((prefix) => name.startsWith(prefix));
  const headOid = repo.refs.get('refs/heads/main');
  if (args.prefixes.length === 0) {
    if (headOid === undefined) {
      out.push('unborn HEAD symref-target:refs/heads/main\n');
    } else {
      out.push(`${headOid} HEAD symref-target:refs/heads/main\n`);
    }
  }
  for (const ref of sortedRefs(repo)) {
    if (!keep(ref.name)) continue;
    let line = `${ref.oid} ${ref.name}`;
    if (args.peel) {
      const peeled = peeledOf(repo, ref.oid);
      if (peeled !== null) line += ` peeled:${peeled}`;
    }
    out.push(`${line}\n`);
  }
  out.push('flush');
  return G.encodePktLines(out);
}

interface FetchArgs {
  wants: string[];
  haves: string[];
  shallow: string[];
  depth: number | null;
}

/** Parse a v2 fetch request body (or a v0 want/have body) into its arguments. */
function parseFetchArgs(body: Buffer): FetchArgs {
  const lines = requestLines(body);
  const args: FetchArgs = { wants: [], haves: [], shallow: [], depth: null };
  for (const line of lines) {
    const text = line.replace(/\n$/, '');
    if (text.startsWith('want ')) args.wants.push(text.slice(5).split(' ')[0]!);
    else if (text.startsWith('have ')) args.haves.push(text.slice(5));
    else if (text.startsWith('shallow ')) args.shallow.push(text.slice(8));
    else if (text.startsWith('deepen ')) {
      const n = parseInt(text.slice(7), 10);
      if (Number.isFinite(n) && n > 0) args.depth = n;
    }
  }
  return args;
}

interface FetchPlan {
  /** Objects to send, in deterministic order. */
  send: string[];
  /** The new shallow boundary commits. */
  shallow: string[];
  /** Previously shallow commits that are now fully included. */
  unshallow: string[];
  /** An ack line for the negotiation, or 'NAK'. */
  ack: string;
  /** Whether a common object was found (an ACK, else a NAK). */
  acked: boolean;
}

/**
 * Plan a fetch: which objects go out, which shallow lines the answer carries,
 * and the negotiation line. `send` is reachable(wants, depth) minus
 * reachable(haves).
 */
export function planFetch(store: Store, repo: Repo, args: FetchArgs): FetchPlan {
  const wants = [...new Set(args.wants)].filter((oid) => G.isOid(oid) && repo.objects.has(oid));
  const haves = args.haves.filter((oid) => G.isOid(oid) && repo.objects.has(oid));
  const depth = args.depth ?? Number.POSITIVE_INFINITY;
  const wantReach = store.reachable(repo, wants, depth);
  // the client's shallow boundaries cut the have closure: it holds the
  // boundary commit and its tree, but none of that commit's history
  const cut = new Set(args.shallow);
  const haveReach = haves.length > 0 ? store.reachable(repo, haves, Number.POSITIVE_INFINITY, cut) : new Set<string>();
  const send = [...wantReach].filter((oid) => !haveReach.has(oid)).sort();
  // The shallow boundary: included commits whose parents fall outside the cut.
  const distances = depth === Number.POSITIVE_INFINITY ? null : store.commitDistances(repo, wants);
  const shallow: string[] = [];
  const unshallow: string[] = [];
  if (distances !== null) {
    for (const [oid, dist] of distances) {
      if (dist !== depth - 1) continue;
      const info = store.commitInfo(repo, oid);
      if (info !== null && info.parents.length > 0) shallow.push(oid);
    }
    shallow.sort();
    // a previous boundary is unshallowed only when its parents are now included
    for (const boundary of args.shallow) {
      const dist = distances.get(boundary);
      if (dist === undefined) continue;
      if (shallow.includes(boundary)) continue;
      if (dist < depth - 1) unshallow.push(boundary);
    }
    unshallow.sort();
  } else {
    // a full (unbounded) fetch unshallows every boundary now included
    for (const boundary of args.shallow) {
      if (wantReach.has(boundary)) unshallow.push(boundary);
    }
    unshallow.sort();
  }
  // Negotiation: an ACK names the first have that is an ancestor of a want.
  let ack = 'NAK';
  let acked = false;
  const wantClosure = store.reachable(repo, wants);
  for (const have of haves) {
    if (wantClosure.has(have)) {
      ack = `ACK ${have}`;
      acked = true;
      break;
    }
  }
  return { send, shallow, unshallow, ack, acked };
}

/** A pack's bytes as sideband band-1 pkt-lines (chunks of at most 65515 bytes). */
function sidebandPack(pack: Buffer): Buffer[] {
  const parts: Buffer[] = [];
  const CHUNK = 65515;
  for (let off = 0; off < pack.length; off += CHUNK) {
    const slice = pack.subarray(off, Math.min(pack.length, off + CHUNK));
    parts.push(G.pktLine(Buffer.concat([Buffer.from([1]), slice])));
  }
  return parts;
}

/** Build a pack from object ids, using the per-object deflate cache. */
function buildPackFor(store: Store, repo: Repo, oids: readonly string[]): Buffer {
  const objects: Array<{ type: G.ObjType; data: Buffer; deflated?: Buffer }> = [];
  for (const oid of oids) {
    const object = repo.objects.get(oid);
    if (object === undefined) continue;
    objects.push({ type: object.type, data: object.data, deflated: store.deflated(repo, oid) ?? undefined });
  }
  return G.buildPack(objects);
}

/** The v2 fetch answer: acknowledgments, shallow-info, wanted-refs, packfile. */
export function fetchV2Response(store: Store, repo: Repo, body: Buffer): Buffer {
  const args = parseFetchArgs(body);
  const plan = planFetch(store, repo, args);
  const parts: Buffer[] = [];
  const ackLines: Array<Buffer | string | 'delim'> = ['acknowledgments\n', `${plan.ack}\n`];
  if (plan.acked) ackLines.push('ready\n');
  ackLines.push('delim');
  parts.push(G.encodePktLines(ackLines));
  if (plan.shallow.length > 0 || plan.unshallow.length > 0) {
    const lines: Array<Buffer | string | 'delim'> = ['shallow-info\n'];
    for (const oid of plan.shallow) lines.push(`shallow ${oid}\n`);
    for (const oid of plan.unshallow) lines.push(`unshallow ${oid}\n`);
    lines.push('delim');
    parts.push(G.encodePktLines(lines));
  }
  const wantedRefs: string[] = [];
  for (const want of args.wants) {
    for (const [name, oid] of repo.refs) {
      if (oid === want) wantedRefs.push(`${oid} ${name}\n`);
    }
  }
  if (wantedRefs.length > 0) {
    parts.push(G.encodePktLines(['wanted-refs\n', ...wantedRefs, 'delim']));
  }
  if (plan.send.length > 0) {
    const pack = buildPackFor(store, repo, plan.send);
    parts.push(G.pktLine('packfile\n'));
    parts.push(...sidebandPack(pack));
  }
  parts.push(G.PKT_FLUSH);
  return Buffer.concat(parts);
}

/** The v0 fetch answer: NAK/ACK lines, shallow lines, then the sideband pack. */
export function fetchV0Response(store: Store, repo: Repo, body: Buffer): Buffer {
  const args = parseFetchArgs(body);
  const plan = planFetch(store, repo, args);
  const parts: Buffer[] = [];
  parts.push(G.pktLine(`${plan.ack}\n`));
  if (plan.acked) parts.push(G.pktLine('ready\n'));
  for (const oid of plan.shallow) parts.push(G.pktLine(`shallow ${oid}\n`));
  for (const oid of plan.unshallow) parts.push(G.pktLine(`unshallow ${oid}\n`));
  if (plan.send.length > 0) {
    const pack = buildPackFor(store, repo, plan.send);
    parts.push(...sidebandPack(pack));
  }
  parts.push(G.PKT_FLUSH);
  return Buffer.concat(parts);
}

export interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

export interface PushOutcome {
  /** The unpack line's verdict: 'ok' or a human error. */
  unpack: string;
  refs: Array<{ ref: string; ok: boolean; reason: string }>;
}

/**
 * Apply a receive-pack request: unpack the pack, then validate and apply each
 * command in order. A refused ref never moves; the accepted ones do.
 */
export function applyPush(store: Store, repo: Repo, body: Buffer): PushOutcome {
  const { packets, rest } = G.decodePktLines(body);
  const commands: PushCommand[] = [];
  for (const packet of packets) {
    if (packet.kind !== 'data') continue;
    let line = packet.payload.toString('utf8').replace(/\n$/, '');
    const nul = line.indexOf('\0');
    if (nul >= 0) line = line.slice(0, nul); // capabilities ride the first command line
    const parts = line.split(' ');
    if (parts.length < 3) continue;
    commands.push({ oldOid: parts[0]!, newOid: parts[1]!, ref: parts[2]! });
  }
  // Unpack first: objects land even when a ref is refused.
  if (rest.length > 0) {
    let objects: G.GitObject[];
    try {
      objects = G.readPack(rest);
    } catch (err) {
      return { unpack: err instanceof Error ? err.message : 'unpack failed', refs: [] };
    }
    for (const object of objects) store.putObject(repo, object.type, object.data);
  }
  const refs: Array<{ ref: string; ok: boolean; reason: string }> = [];
  for (const command of commands) {
    const current = repo.refs.get(command.ref);
    if (command.newOid === G.ZERO_OID) {
      // delete
      if (current === undefined) {
        refs.push({ ref: command.ref, ok: false, reason: 'no such ref' });
      } else if (command.oldOid !== current) {
        refs.push({ ref: command.ref, ok: false, reason: 'stale info' });
      } else {
        repo.refs.delete(command.ref);
        refs.push({ ref: command.ref, ok: true, reason: '' });
      }
      continue;
    }
    if (!G.isOid(command.newOid) || !repo.objects.has(command.newOid)) {
      refs.push({ ref: command.ref, ok: false, reason: 'missing object' });
      continue;
    }
    if (command.oldOid === G.ZERO_OID) {
      // create
      if (current !== undefined) {
        refs.push({ ref: command.ref, ok: false, reason: 'already exists' });
      } else {
        repo.refs.set(command.ref, command.newOid);
        refs.push({ ref: command.ref, ok: true, reason: '' });
      }
      continue;
    }
    // update
    if (current === undefined) {
      refs.push({ ref: command.ref, ok: false, reason: 'no such ref' });
      continue;
    }
    if (command.oldOid !== current) {
      refs.push({ ref: command.ref, ok: false, reason: 'stale info' });
      continue;
    }
    if (command.newOid === current) {
      refs.push({ ref: command.ref, ok: true, reason: '' });
      continue;
    }
    if (command.ref.startsWith('refs/tags/')) {
      // a tag is not fast-forwarded: an update without force is refused
      refs.push({ ref: command.ref, ok: false, reason: 'already exists' });
      continue;
    }
    if (!store.isAncestor(repo, command.oldOid, command.newOid)) {
      refs.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
      continue;
    }
    repo.refs.set(command.ref, command.newOid);
    refs.push({ ref: command.ref, ok: true, reason: '' });
  }
  return { unpack: 'ok', refs };
}

/** The receive-pack answer: the report-status stream inside sideband band 1. */
export function pushResponse(outcome: PushOutcome): Buffer {
  const inner: Array<Buffer | string | 'flush'> = [`unpack ${outcome.unpack}\n`];
  for (const ref of outcome.refs) {
    inner.push(ref.ok ? `ok ${ref.ref}\n` : `ng ${ref.ref} ${ref.reason}\n`);
  }
  inner.push('flush');
  const report = G.encodePktLines(inner);
  const parts: Buffer[] = [];
  const CHUNK = 65515;
  for (let off = 0; off < report.length; off += CHUNK) {
    const slice = report.subarray(off, Math.min(report.length, off + CHUNK));
    parts.push(G.pktLine(Buffer.concat([Buffer.from([1]), slice])));
  }
  parts.push(G.PKT_FLUSH);
  return Buffer.concat(parts);
}
