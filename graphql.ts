// graphql.ts — a small, self-contained GraphQL v4 surface: a lexer, a
// recursive-descent parser (operations, variables, fragments, inline
// fragments, aliases, batch bodies) and an executor over the same Store the
// REST surface uses. Unknown fields answer HTTP 200 with an `errors` array
// that names the field, exactly as the real API does.

import { Store, VIEWER_ID, VIEWER_LOGIN, isoTime, type Issue, type Pull, type Repo } from './store.ts';

// ---------------------------------------------------------------------------
// lexer
// ---------------------------------------------------------------------------

type TokenKind = 'name' | 'int' | 'float' | 'string' | 'punct' | 'eof';

interface Token {
  kind: TokenKind;
  value: string;
}

function lex(input: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  const n = input.length;
  while (i < n) {
    const ch = input[i]!;
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r' || ch === ',') {
      i += 1;
      continue;
    }
    if (ch === '#') {
      while (i < n && input[i] !== '\n') i += 1;
      continue;
    }
    if (ch === '.' && input.startsWith('...', i)) {
      tokens.push({ kind: 'punct', value: '...' });
      i += 3;
      continue;
    }
    if ('!$():=@[]{}|'.includes(ch)) {
      tokens.push({ kind: 'punct', value: ch });
      i += 1;
      continue;
    }
    if (ch === '"') {
      if (input.startsWith('"""', i)) {
        const end = input.indexOf('"""', i + 3);
        const raw = end === -1 ? input.slice(i + 3) : input.slice(i + 3, end);
        tokens.push({ kind: 'string', value: raw });
        i = end === -1 ? n : end + 3;
        continue;
      }
      let j = i + 1;
      let out = '';
      while (j < n && input[j] !== '"') {
        if (input[j] === '\\' && j + 1 < n) {
          const esc = input[j + 1]!;
          if (esc === 'n') out += '\n';
          else if (esc === 't') out += '\t';
          else if (esc === 'r') out += '\r';
          else if (esc === 'u' && j + 5 < n) {
            out += String.fromCharCode(parseInt(input.slice(j + 2, j + 6), 16));
            j += 4;
          } else out += esc;
          j += 2;
          continue;
        }
        out += input[j];
        j += 1;
      }
      tokens.push({ kind: 'string', value: out });
      i = j + 1;
      continue;
    }
    if (/[0-9-]/.test(ch) && (ch !== '-' || /[0-9]/.test(input[i + 1] ?? ''))) {
      let j = i;
      if (input[j] === '-') j += 1;
      while (j < n && /[0-9]/.test(input[j]!)) j += 1;
      let isFloat = false;
      if (input[j] === '.' && /[0-9]/.test(input[j + 1] ?? '')) {
        isFloat = true;
        j += 1;
        while (j < n && /[0-9]/.test(input[j]!)) j += 1;
      }
      if (input[j] === 'e' || input[j] === 'E') {
        isFloat = true;
        j += 1;
        if (input[j] === '+' || input[j] === '-') j += 1;
        while (j < n && /[0-9]/.test(input[j]!)) j += 1;
      }
      tokens.push({ kind: isFloat ? 'float' : 'int', value: input.slice(i, j) });
      i = j;
      continue;
    }
    if (/[A-Za-z_]/.test(ch)) {
      let j = i;
      while (j < n && /[A-Za-z0-9_]/.test(input[j]!)) j += 1;
      tokens.push({ kind: 'name', value: input.slice(i, j) });
      i = j;
      continue;
    }
    throw new Error(`unexpected character ${JSON.stringify(ch)} at ${i}`);
  }
  tokens.push({ kind: 'eof', value: '' });
  return tokens;
}

// ---------------------------------------------------------------------------
// parser
// ---------------------------------------------------------------------------

type ValueNode =
  | { kind: 'variable'; name: string }
  | { kind: 'int'; value: number }
  | { kind: 'float'; value: number }
  | { kind: 'string'; value: string }
  | { kind: 'boolean'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'list'; values: ValueNode[] }
  | { kind: 'object'; fields: Array<{ name: string; value: ValueNode }> };

interface FieldNode {
  kind: 'field';
  name: string;
  alias: string | null;
  args: Array<{ name: string; value: ValueNode }>;
  selections: SelectionNode[];
}

interface FragmentSpreadNode {
  kind: 'fragmentSpread';
  name: string;
}

interface InlineFragmentNode {
  kind: 'inlineFragment';
  typeCondition: string | null;
  selections: SelectionNode[];
}

type SelectionNode = FieldNode | FragmentSpreadNode | InlineFragmentNode;

interface OperationNode {
  kind: 'operation';
  operation: 'query' | 'mutation';
  name: string | null;
  variableDefinitions: Array<{ name: string; defaultValue: ValueNode | null }>;
  selections: SelectionNode[];
}

interface FragmentNode {
  kind: 'fragment';
  name: string;
  typeCondition: string;
  selections: SelectionNode[];
}

interface DocumentNode {
  definitions: Array<OperationNode | FragmentNode>;
}

class Parser {
  private pos = 0;
  private readonly tokens: Token[];

  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  private peek(): Token {
    return this.tokens[this.pos]!;
  }

  private next(): Token {
    const token = this.tokens[this.pos]!;
    this.pos += 1;
    return token;
  }

  private expectPunct(value: string): void {
    const token = this.next();
    if (token.kind !== 'punct' || token.value !== value) {
      throw new Error(`expected ${value}, got ${token.value}`);
    }
  }

  private expectName(value?: string): string {
    const token = this.next();
    if (token.kind !== 'name' || (value !== undefined && token.value !== value)) {
      throw new Error(`expected name ${value ?? ''}, got ${token.value}`);
    }
    return token.value;
  }

  private skipDirectives(): void {
    while (this.peek().kind === 'punct' && this.peek().value === '@') {
      this.next();
      this.expectName();
      if (this.peek().kind === 'punct' && this.peek().value === '(') this.parseArguments();
    }
  }

  parseDocument(): DocumentNode {
    const definitions: Array<OperationNode | FragmentNode> = [];
    while (this.peek().kind !== 'eof') {
      definitions.push(this.parseDefinition());
    }
    return { definitions };
  }

  private parseDefinition(): OperationNode | FragmentNode {
    const token = this.peek();
    if (token.kind === 'punct' && token.value === '{') {
      return { kind: 'operation', operation: 'query', name: null, variableDefinitions: [], selections: this.parseSelectionSet() };
    }
    if (token.kind === 'name' && (token.value === 'query' || token.value === 'mutation')) {
      this.next();
      let name: string | null = null;
      if (this.peek().kind === 'name') name = this.next().value;
      const variableDefinitions = this.parseVariableDefinitions();
      this.skipDirectives();
      return { kind: 'operation', operation: token.value as 'query' | 'mutation', name, variableDefinitions, selections: this.parseSelectionSet() };
    }
    if (token.kind === 'name' && token.value === 'fragment') {
      this.next();
      const name = this.expectName();
      this.expectName('on');
      const typeCondition = this.expectName();
      this.skipDirectives();
      return { kind: 'fragment', name, typeCondition, selections: this.parseSelectionSet() };
    }
    throw new Error(`unexpected definition ${token.value}`);
  }

  private parseVariableDefinitions(): Array<{ name: string; defaultValue: ValueNode | null }> {
    const defs: Array<{ name: string; defaultValue: ValueNode | null }> = [];
    if (this.peek().kind === 'punct' && this.peek().value === '(') {
      this.next();
      while (!(this.peek().kind === 'punct' && this.peek().value === ')')) {
        this.expectPunct('$');
        const name = this.expectName();
        this.expectPunct(':');
        this.parseType();
        let defaultValue: ValueNode | null = null;
        if (this.peek().kind === 'punct' && this.peek().value === '=') {
          this.next();
          defaultValue = this.parseValue(true);
        }
        defs.push({ name, defaultValue });
      }
      this.expectPunct(')');
    }
    return defs;
  }

  private parseType(): void {
    if (this.peek().kind === 'punct' && this.peek().value === '[') {
      this.next();
      this.parseType();
      this.expectPunct(']');
    } else {
      this.expectName();
    }
    if (this.peek().kind === 'punct' && this.peek().value === '!') this.next();
  }

  private parseSelectionSet(): SelectionNode[] {
    this.expectPunct('{');
    const selections: SelectionNode[] = [];
    while (!(this.peek().kind === 'punct' && this.peek().value === '}')) {
      selections.push(this.parseSelection());
    }
    this.expectPunct('}');
    return selections;
  }

  private parseSelection(): SelectionNode {
    if (this.peek().kind === 'punct' && this.peek().value === '...') {
      this.next();
      if (this.peek().kind === 'name' && this.peek().value !== 'on') {
        const name = this.next().value;
        this.skipDirectives();
        return { kind: 'fragmentSpread', name };
      }
      let typeCondition: string | null = null;
      if (this.peek().kind === 'name' && this.peek().value === 'on') {
        this.next();
        typeCondition = this.expectName();
      }
      this.skipDirectives();
      return { kind: 'inlineFragment', typeCondition, selections: this.parseSelectionSet() };
    }
    const first = this.expectName();
    let alias: string | null = null;
    let name = first;
    if (this.peek().kind === 'punct' && this.peek().value === ':') {
      this.next();
      alias = first;
      name = this.expectName();
    }
    const args = this.parseArguments();
    this.skipDirectives();
    const selections =
      this.peek().kind === 'punct' && this.peek().value === '{' ? this.parseSelectionSet() : [];
    return { kind: 'field', name, alias, args, selections };
  }

  private parseArguments(): Array<{ name: string; value: ValueNode }> {
    const args: Array<{ name: string; value: ValueNode }> = [];
    if (this.peek().kind === 'punct' && this.peek().value === '(') {
      this.next();
      while (!(this.peek().kind === 'punct' && this.peek().value === ')')) {
        const name = this.expectName();
        this.expectPunct(':');
        args.push({ name, value: this.parseValue(false) });
      }
      this.expectPunct(')');
    }
    return args;
  }

  private parseValue(isConst: boolean): ValueNode {
    const token = this.peek();
    if (token.kind === 'punct' && token.value === '$') {
      if (isConst) throw new Error('variable in constant value');
      this.next();
      return { kind: 'variable', name: this.expectName() };
    }
    if (token.kind === 'punct' && token.value === '[') {
      this.next();
      const values: ValueNode[] = [];
      while (!(this.peek().kind === 'punct' && this.peek().value === ']')) {
        values.push(this.parseValue(isConst));
      }
      this.expectPunct(']');
      return { kind: 'list', values };
    }
    if (token.kind === 'punct' && token.value === '{') {
      this.next();
      const fields: Array<{ name: string; value: ValueNode }> = [];
      while (!(this.peek().kind === 'punct' && this.peek().value === '}')) {
        const name = this.expectName();
        this.expectPunct(':');
        fields.push({ name, value: this.parseValue(isConst) });
      }
      this.expectPunct('}');
      return { kind: 'object', fields };
    }
    if (token.kind === 'int') {
      this.next();
      return { kind: 'int', value: parseInt(token.value, 10) };
    }
    if (token.kind === 'float') {
      this.next();
      return { kind: 'float', value: parseFloat(token.value) };
    }
    if (token.kind === 'string') {
      this.next();
      return { kind: 'string', value: token.value };
    }
    if (token.kind === 'name') {
      this.next();
      if (token.value === 'true') return { kind: 'boolean', value: true };
      if (token.value === 'false') return { kind: 'boolean', value: false };
      if (token.value === 'null') return { kind: 'null' };
      return { kind: 'enum', value: token.value };
    }
    throw new Error(`unexpected value ${token.value}`);
  }
}

// ---------------------------------------------------------------------------
// executor
// ---------------------------------------------------------------------------

interface GqlError {
  message: string;
  path?: Array<string | number>;
}

interface ExecCtx {
  store: Store;
  variables: Record<string, unknown>;
  fragments: Map<string, FragmentNode>;
  errors: GqlError[];
}

type Source = Record<string, unknown> & { __typename?: string };

function gid(kind: string, ...parts: Array<string | number>): string {
  return Buffer.from(`${kind}:${parts.join(':')}`, 'utf8').toString('base64url');
}

function decodeGid(id: string): { kind: string; parts: string[] } | null {
  try {
    const text = Buffer.from(id, 'base64url').toString('utf8');
    const [kind, ...parts] = text.split(':');
    if (kind === undefined || parts.length === 0) return null;
    return { kind, parts };
  } catch {
    return null;
  }
}

function evalValue(node: ValueNode, ctx: ExecCtx): unknown {
  switch (node.kind) {
    case 'variable':
      return ctx.variables[node.name];
    case 'int':
    case 'float':
    case 'string':
    case 'boolean':
      return node.value;
    case 'null':
      return null;
    case 'enum':
      return node.value;
    case 'list':
      return node.values.map((child) => evalValue(child, ctx));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const field of node.fields) out[field.name] = evalValue(field.value, ctx);
      return out;
    }
  }
}

function evalArgs(field: FieldNode, ctx: ExecCtx): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const arg of field.args) out[arg.name] = evalValue(arg.value, ctx);
  return out;
}

function cursorFor(seq: number): string {
  return Buffer.from(`c:${seq}`, 'utf8').toString('base64url');
}

function seqAfter(after: unknown): number {
  if (typeof after !== 'string') return 0;
  try {
    const text = Buffer.from(after, 'base64url').toString('utf8');
    const match = /^c:(\d+)$/.exec(text);
    return match === null ? 0 : parseInt(match[1]!, 10);
  } catch {
    return 0;
  }
}

/** A connection object over a seq-ordered list. */
function connection<T extends { seq: number }>(
  items: readonly T[],
  args: Record<string, unknown>,
  mapNode: (item: T) => Source,
  typename = 'IssueConnection',
): Source {
  const first = typeof args.first === 'number' ? Math.max(0, args.first) : items.length;
  const afterSeq = seqAfter(args.after);
  const filtered = items.filter((item) => item.seq > afterSeq);
  const page = filtered.slice(0, first);
  const nodes = page.map(mapNode);
  return {
    __typename: typename,
    nodes,
    edges: nodes.map((node, index) => ({ __typename: 'IssueEdge', node, cursor: cursorFor(page[index]!.seq) })),
    pageInfo: {
      __typename: 'PageInfo',
      hasNextPage: filtered.length > page.length,
      hasPreviousPage: afterSeq > 0,
      startCursor: page.length > 0 ? cursorFor(page[0]!.seq) : null,
      endCursor: page.length > 0 ? cursorFor(page[page.length - 1]!.seq) : null,
    },
    totalCount: filtered.length,
  };
}

function issueSource(repo: Repo, issue: Issue): Source {
  return {
    __typename: 'Issue',
    __repo: repo,
    __issue: issue,
    id: gid('I', repo.id, issue.number),
    databaseId: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state === 'open' ? 'OPEN' : 'CLOSED',
    createdAt: isoTime(issue.seq),
    updatedAt: isoTime(issue.seq),
    url: `https://github.com/${repo.owner}/${repo.name}/issues/${issue.number}`,
    author: { __typename: 'User', login: VIEWER_LOGIN, id: VIEWER_ID },
    repository: repoSource(repo),
    comments: connection(
      issue.comments,
      {},
      (comment) => ({
        __typename: 'IssueComment',
        id: gid('C', repo.id, issue.number, comment.id),
        databaseId: comment.id,
        body: comment.body,
        createdAt: isoTime(comment.seq),
        author: { __typename: 'User', login: VIEWER_LOGIN, id: VIEWER_ID },
      }),
      'IssueCommentConnection',
    ),
  };
}

function pullSource(repo: Repo, pull: Pull): Source {
  const files = repo.__store!.pullFiles(repo, pull);
  return {
    __typename: 'PullRequest',
    __repo: repo,
    __pull: pull,
    id: gid('P', repo.id, pull.number),
    databaseId: pull.id,
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.merged ? 'MERGED' : pull.state === 'open' ? 'OPEN' : 'CLOSED',
    merged: pull.merged,
    mergeable: (() => {
      const value = repo.__store!.pullMergeable(repo, pull);
      return value === 'UNKNOWN' ? 'CONFLICTING' : value;
    })(),
    headRefName: pull.headRef,
    baseRefName: pull.baseRef,
    createdAt: isoTime(pull.seq),
    updatedAt: isoTime(pull.seq),
    url: `https://github.com/${repo.owner}/${repo.name}/pull/${pull.number}`,
    author: { __typename: 'User', login: VIEWER_LOGIN, id: VIEWER_ID },
    repository: repoSource(repo),
    files: connection(
      files.map((file, index) => ({ ...file, seq: index + 1 })),
      {},
      (file) => ({
        __typename: 'PullRequestChangedFile',
        path: file.filename,
        additions: file.additions,
        deletions: file.deletions,
        changeType: file.status === 'added' ? 'ADDED' : 'MODIFIED',
      }),
      'PullRequestChangedFileConnection',
    ),
  };
}

function repoSource(repo: Repo): Source {
  return {
    __typename: 'Repository',
    __repo: repo,
    id: gid('R', repo.id),
    databaseId: repo.id,
    name: repo.name,
    nameWithOwner: `${repo.owner}/${repo.name}`,
    description: repo.description,
    url: `https://github.com/${repo.owner}/${repo.name}`,
    owner: { __typename: 'User', login: repo.owner, id: VIEWER_ID },
    defaultBranchRef: { __typename: 'Ref', name: repo.defaultBranch, prefix: 'refs/heads/' },
    viewerPermission: repo.owner === VIEWER_LOGIN ? 'ADMIN' : 'READ',
    hasIssuesEnabled: true,
    createdAt: isoTime(repo.seq),
    updatedAt: isoTime(repo.seq),
  };
}

// The fields each introspected type exposes (used by __type).
const TYPE_FIELDS: Record<string, string[]> = {
  Issue: ['id', 'databaseId', 'number', 'title', 'body', 'state', 'createdAt', 'updatedAt', 'author', 'repository', 'url', 'comments'],
  PullRequest: ['id', 'databaseId', 'number', 'title', 'body', 'state', 'merged', 'mergeable', 'headRefName', 'baseRefName', 'files', 'author', 'repository', 'url', 'createdAt', 'updatedAt'],
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'description', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequest', 'pullRequests', 'url', 'createdAt', 'updatedAt'],
  User: ['id', 'databaseId', 'login', 'name', 'email'],
  IssueConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  PullRequestConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  PageInfo: ['hasNextPage', 'hasPreviousPage', 'startCursor', 'endCursor'],
  Ref: ['name', 'prefix'],
};

const ENUM_VALUES: Record<string, string[]> = {
  SearchType: ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION'],
  IssueState: ['OPEN', 'CLOSED'],
  PullRequestState: ['OPEN', 'CLOSED', 'MERGED'],
};

function typeSource(name: string): Source | null {
  if (TYPE_FIELDS[name] !== undefined) {
    return {
      __typename: '__Type',
      name,
      kind: 'OBJECT',
      fields: TYPE_FIELDS[name]!.map((field) => ({
        __typename: '__Field',
        name: field,
        type: { __typename: '__Type', name: 'String', kind: 'SCALAR' },
      })),
      enumValues: null,
    };
  }
  if (ENUM_VALUES[name] !== undefined) {
    return {
      __typename: '__Type',
      name,
      kind: 'ENUM',
      fields: null,
      enumValues: ENUM_VALUES[name]!.map((value) => ({ __typename: '__EnumValue', name: value })),
    };
  }
  return null;
}

function resolveNode(store: Store, id: string): Source | null {
  const decoded = decodeGid(id);
  if (decoded === null) return null;
  const [first, second, third] = decoded.parts;
  if (decoded.kind === 'R') {
    const repo = store.reposById().get(parseInt(first!, 10));
    return repo === undefined ? null : repoSource(repo);
  }
  if (decoded.kind === 'I') {
    const repo = store.reposById().get(parseInt(first!, 10));
    const issue = repo?.issues.get(parseInt(second!, 10));
    return repo === undefined || issue === undefined ? null : issueSource(repo, issue);
  }
  if (decoded.kind === 'P') {
    const repo = store.reposById().get(parseInt(first!, 10));
    const pull = repo?.pulls.get(parseInt(second!, 10));
    return repo === undefined || pull === undefined ? null : pullSource(repo, pull);
  }
  if (decoded.kind === 'C') {
    const repo = store.reposById().get(parseInt(first!, 10));
    const issue = repo?.issues.get(parseInt(second!, 10));
    const comment = issue?.comments.find((candidate) => candidate.id === parseInt(third!, 10));
    if (repo === undefined || issue === undefined || comment === undefined) return null;
    return {
      __typename: 'IssueComment',
      id: gid('C', repo.id, issue.number, comment.id),
      databaseId: comment.id,
      body: comment.body,
      createdAt: isoTime(comment.seq),
      author: { __typename: 'User', login: VIEWER_LOGIN, id: VIEWER_ID },
    };
  }
  return null;
}

/** Resolve one field on a source object; null when the field does not exist. */
function resolveField(source: Source, field: FieldNode, ctx: ExecCtx, path: Array<string | number>): unknown {
  const typeName = source.__typename ?? 'Query';
  const args = evalArgs(field, ctx);
  const store = ctx.store;

  // fields carried directly on the source object
  if (typeName === 'Repository') {
    const repo = source.__repo as Repo;
    if (field.name === 'issue') {
      const issue = repo.issues.get(typeof args.number === 'number' ? args.number : -1);
      return issue === undefined ? null : issueSource(repo, issue);
    }
    if (field.name === 'issues') {
      let list = [...repo.issues.values()];
      if (Array.isArray(args.states)) {
        const wanted = new Set(args.states.map((state) => String(state).toLowerCase()));
        list = list.filter((issue) => wanted.has(issue.state));
      }
      if (isRecord(args.orderBy)) {
        const direction = String(args.orderBy.direction ?? 'ASC');
        list.sort((a, b) => (direction === 'DESC' ? b.seq - a.seq : a.seq - b.seq));
      } else {
        list.sort((a, b) => a.seq - b.seq);
      }
      return connection(list, args, (issue) => issueSource(repo, issue));
    }
    if (field.name === 'pullRequest') {
      const pull = repo.pulls.get(typeof args.number === 'number' ? args.number : -1);
      return pull === undefined ? null : pullSource(repo, pull);
    }
    if (field.name === 'pullRequests') {
      let list = [...repo.pulls.values()];
      if (Array.isArray(args.states)) {
        const wanted = new Set(
          args.states.map((state) => (String(state) === 'MERGED' ? 'closed' : String(state).toLowerCase())),
        );
        list = list.filter((pull) => wanted.has(pull.merged ? 'closed' : pull.state));
      } else {
        list = list.filter((pull) => pull.state === 'open');
      }
      list.sort((a, b) => a.seq - b.seq);
      return connection(list, args, (pull) => pullSource(repo, pull), 'PullRequestConnection');
    }
  }

  if (typeName === 'Query') {
    if (field.name === 'viewer') {
      return { __typename: 'User', login: VIEWER_LOGIN, id: VIEWER_ID, databaseId: VIEWER_ID, name: VIEWER_LOGIN, email: 'adaiversen@example.invalid' };
    }
    if (field.name === 'repository') {
      const repo = store.getRepo(String(args.owner ?? ''), String(args.name ?? ''));
      return repo === undefined ? null : repoSource(repo);
    }
    if (field.name === 'node') {
      return typeof args.id === 'string' ? resolveNode(store, args.id) : null;
    }
    if (field.name === '__type') {
      return typeof args.name === 'string' ? typeSource(args.name) : null;
    }
    if (field.name === '__schema') {
      return { __typename: '__Schema', types: [] };
    }
  }

  if (typeName === 'Mutation') {
    if (field.name === 'createIssue') {
      const input = isRecord(args.input) ? args.input : {};
      const repo = typeof input.repositoryId === 'string' ? resolveNode(store, input.repositoryId) : null;
      const real = repo?.__repo as Repo | undefined;
      if (real === undefined || typeof input.title !== 'string') {
        ctx.errors.push({ message: 'createIssue: the repositoryId names no repository', path });
        return null;
      }
      const issue = store.createIssue(real, input.title, typeof input.body === 'string' ? input.body : '', []);
      return { __typename: 'CreateIssuePayload', issue: issueSource(real, issue) };
    }
    if (field.name === 'closeIssue') {
      const input = isRecord(args.input) ? args.input : {};
      const node = typeof input.issueId === 'string' ? resolveNode(store, input.issueId) : null;
      const issue = node?.__issue as Issue | undefined;
      const repo = node?.__repo as Repo | undefined;
      if (issue === undefined || repo === undefined) {
        ctx.errors.push({ message: 'closeIssue: the issueId names no issue', path });
        return null;
      }
      issue.state = 'closed';
      return { __typename: 'CloseIssuePayload', issue: issueSource(repo, issue) };
    }
    if (field.name === 'createPullRequest') {
      const input = isRecord(args.input) ? args.input : {};
      const repo = typeof input.repositoryId === 'string' ? resolveNode(store, input.repositoryId) : null;
      const real = repo?.__repo as Repo | undefined;
      const head = typeof input.headRefName === 'string' ? input.headRefName : '';
      const baseBranch = typeof input.baseRefName === 'string' ? input.baseRefName : '';
      const title = typeof input.title === 'string' ? input.title : '';
      if (real === undefined || head === '' || baseBranch === '' || title === '') {
        ctx.errors.push({ message: 'createPullRequest: repositoryId, baseRefName, headRefName and title are required', path });
        return null;
      }
      const headSha = real.refs.get(`refs/heads/${head}`);
      const baseSha = real.refs.get(`refs/heads/${baseBranch}`);
      if (headSha === undefined || baseSha === undefined) {
        ctx.errors.push({ message: 'createPullRequest: head and base must name existing branches', path });
        return null;
      }
      const existing = [...real.pulls.values()].find(
        (pull) => pull.state === 'open' && pull.headRef === head && pull.baseRef === baseBranch,
      );
      if (existing !== undefined) return { __typename: 'CreatePullRequestPayload', pullRequest: pullSource(real, existing) };
      const pull = store.createPull(real, head, baseBranch, headSha, baseSha, title, typeof input.body === 'string' ? input.body : '');
      return { __typename: 'CreatePullRequestPayload', pullRequest: pullSource(real, pull) };
    }
    if (field.name === 'mergePullRequest') {
      const input = isRecord(args.input) ? args.input : {};
      const node = typeof input.pullRequestId === 'string' ? resolveNode(store, input.pullRequestId) : null;
      const pull = node?.__pull as Pull | undefined;
      const repo = node?.__repo as Repo | undefined;
      if (pull === undefined || repo === undefined) {
        ctx.errors.push({ message: 'mergePullRequest: the pullRequestId names no pull request', path });
        return null;
      }
      if (pull.state !== 'open') {
        return { __typename: 'MergePullRequestPayload', pullRequest: pullSource(repo, pull) };
      }
      const merged = store.squashMerge(repo, pull);
      if (!merged.ok) {
        ctx.errors.push({ message: `mergePullRequest: ${merged.reason}`, path });
      }
      return { __typename: 'MergePullRequestPayload', pullRequest: pullSource(repo, pull) };
    }
    if (field.name === 'addComment') {
      const input = isRecord(args.input) ? args.input : {};
      const node = typeof input.subjectId === 'string' ? resolveNode(store, input.subjectId) : null;
      const issue = node?.__issue as Issue | undefined;
      const repo = node?.__repo as Repo | undefined;
      if (issue === undefined || repo === undefined || typeof input.body !== 'string') {
        ctx.errors.push({ message: 'addComment: the subjectId names no issue', path });
        return null;
      }
      const comment = store.createComment(repo, issue, input.body);
      return {
        __typename: 'AddCommentPayload',
        commentEdge: {
          __typename: 'IssueCommentEdge',
          node: {
            __typename: 'IssueComment',
            id: gid('C', repo.id, issue.number, comment.id),
            databaseId: comment.id,
            body: comment.body,
            createdAt: isoTime(comment.seq),
            author: { __typename: 'User', login: VIEWER_LOGIN, id: VIEWER_ID },
          },
        },
      };
    }
  }

  // plain properties carried on the source (scalars and pre-built children)
  if (field.name in source && field.name !== '__typename') {
    return source[field.name];
  }
  return undefined;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function executeSelections(
  selections: SelectionNode[],
  source: Source,
  ctx: ExecCtx,
  path: Array<string | number>,
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  const typeName = source.__typename ?? 'Query';
  for (const selection of selections) {
    if (selection.kind === 'fragmentSpread') {
      const fragment = ctx.fragments.get(selection.name);
      if (fragment === undefined) {
        ctx.errors.push({ message: `unknown fragment "${selection.name}"`, path });
        continue;
      }
      Object.assign(out, executeSelections(fragment.selections, source, ctx, path));
      continue;
    }
    if (selection.kind === 'inlineFragment') {
      if (selection.typeCondition !== null && selection.typeCondition !== typeName) continue;
      Object.assign(out, executeSelections(selection.selections, source, ctx, path));
      continue;
    }
    const key = selection.alias ?? selection.name;
    const fieldPath = [...path, key];
    const value = resolveField(source, selection, ctx, fieldPath);
    if (value === undefined) {
      ctx.errors.push({
        message: `Cannot query field "${selection.name}" on type "${typeName}".`,
        path: fieldPath,
      });
      out[key] = null;
      continue;
    }
    if (value === null) {
      out[key] = null;
      continue;
    }
    if (selection.selections.length === 0) {
      out[key] = value;
      continue;
    }
    if (Array.isArray(value)) {
      out[key] = value.map((item, index) =>
        isRecord(item) ? executeSelections(selection.selections, item as Source, ctx, [...fieldPath, index]) : item,
      );
      continue;
    }
    if (isRecord(value)) {
      out[key] = executeSelections(selection.selections, value as Source, ctx, fieldPath);
      continue;
    }
    out[key] = value;
  }
  return out;
}

function executeOperation(operation: OperationNode, document: DocumentNode, store: Store, variables: Record<string, unknown>): Record<string, unknown> {
  const fragments = new Map<string, FragmentNode>();
  for (const definition of document.definitions) {
    if (definition.kind === 'fragment') fragments.set(definition.name, definition);
  }
  const ctx: ExecCtx = { store, variables: { ...variables }, fragments, errors: [] };
  for (const def of operation.variableDefinitions) {
    if (!(def.name in ctx.variables) && def.defaultValue !== null) {
      ctx.variables[def.name] = evalValue(def.defaultValue, ctx);
    }
  }
  const root: Source =
    operation.operation === 'mutation' ? { __typename: 'Mutation' } : { __typename: 'Query' };
  const data = executeSelections(operation.selections, root, ctx, []);
  const result: Record<string, unknown> = { data };
  if (ctx.errors.length > 0) result.errors = ctx.errors;
  return result;
}

/** Run one GraphQL request body (a single operation or a batch array). */
export function handleGraphQL(store: Store, body: unknown): { status: number; body: unknown } {
  const requests = Array.isArray(body) ? body : [body];
  const results: Array<Record<string, unknown>> = [];
  for (const request of requests) {
    if (!isRecord(request) || typeof request.query !== 'string') {
      results.push({ data: null, errors: [{ message: 'a GraphQL query string is required' }] });
      continue;
    }
    let document: DocumentNode;
    try {
      document = new Parser(lex(request.query)).parseDocument();
    } catch (err) {
      results.push({ data: null, errors: [{ message: err instanceof Error ? err.message : 'parse error' }] });
      continue;
    }
    const operations = document.definitions.filter((def): def is OperationNode => def.kind === 'operation');
    const operationName = typeof request.operationName === 'string' ? request.operationName : null;
    const operation =
      operationName !== null
        ? operations.find((candidate) => candidate.name === operationName)
        : operations[0];
    if (operation === undefined) {
      results.push({ data: null, errors: [{ message: 'no operation found in the document' }] });
      continue;
    }
    const variables = isRecord(request.variables) ? (request.variables as Record<string, unknown>) : {};
    results.push(executeOperation(operation, document, store, variables));
  }
  return { status: 200, body: Array.isArray(body) ? results : results[0]! };
}
