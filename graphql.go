// graphql.go — a small, self-contained GraphQL v4 surface: a lexer, a
// recursive-descent parser (operations, variables, fragments, inline
// fragments, aliases, batch bodies) and an executor over the same Store the
// REST surface uses. Unknown fields answer HTTP 200 with an `errors` array
// that names the field, exactly as the real API does.

package main

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// lexer
// ---------------------------------------------------------------------------

type tokenKind int

const (
	tokName tokenKind = iota
	tokInt
	tokFloat
	tokString
	tokPunct
	tokEOF
)

type gqlToken struct {
	kind  tokenKind
	value string
}

func gqlLex(input string) ([]gqlToken, error) {
	var tokens []gqlToken
	i := 0
	n := len(input)
	for i < n {
		ch := input[i]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',' {
			i++
			continue
		}
		if ch == '#' {
			for i < n && input[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '.' && strings.HasPrefix(input[i:], "...") {
			tokens = append(tokens, gqlToken{tokPunct, "..."})
			i += 3
			continue
		}
		if strings.ContainsRune("!$():=@[]{}|", rune(ch)) {
			tokens = append(tokens, gqlToken{tokPunct, string(ch)})
			i++
			continue
		}
		if ch == '"' {
			if strings.HasPrefix(input[i:], `"""`) {
				end := strings.Index(input[i+3:], `"""`)
				var raw string
				if end == -1 {
					raw = input[i+3:]
					i = n
				} else {
					raw = input[i+3 : i+3+end]
					i = i + 3 + end + 3
				}
				tokens = append(tokens, gqlToken{tokString, raw})
				continue
			}
			j := i + 1
			var out strings.Builder
			for j < n && input[j] != '"' {
				if input[j] == '\\' && j+1 < n {
					esc := input[j+1]
					switch {
					case esc == 'n':
						out.WriteByte('\n')
					case esc == 't':
						out.WriteByte('\t')
					case esc == 'r':
						out.WriteByte('\r')
					case esc == 'u' && j+5 < n:
						if code, err := strconv.ParseInt(input[j+2:j+6], 16, 32); err == nil {
							out.WriteRune(rune(code))
						}
						j += 4
					default:
						out.WriteByte(esc)
					}
					j += 2
					continue
				}
				out.WriteByte(input[j])
				j++
			}
			tokens = append(tokens, gqlToken{tokString, out.String()})
			i = j + 1
			continue
		}
		if ch >= '0' && ch <= '9' || ch == '-' && i+1 < n && input[i+1] >= '0' && input[i+1] <= '9' {
			j := i
			if input[j] == '-' {
				j++
			}
			for j < n && input[j] >= '0' && input[j] <= '9' {
				j++
			}
			isFloat := false
			if j < n && input[j] == '.' && j+1 < n && input[j+1] >= '0' && input[j+1] <= '9' {
				isFloat = true
				j++
				for j < n && input[j] >= '0' && input[j] <= '9' {
					j++
				}
			}
			if j < n && (input[j] == 'e' || input[j] == 'E') {
				isFloat = true
				j++
				if j < n && (input[j] == '+' || input[j] == '-') {
					j++
				}
				for j < n && input[j] >= '0' && input[j] <= '9' {
					j++
				}
			}
			kind := tokInt
			if isFloat {
				kind = tokFloat
			}
			tokens = append(tokens, gqlToken{kind, input[i:j]})
			i = j
			continue
		}
		if ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch == '_' {
			j := i
			for j < n && (input[j] >= 'A' && input[j] <= 'Z' || input[j] >= 'a' && input[j] <= 'z' || input[j] >= '0' && input[j] <= '9' || input[j] == '_') {
				j++
			}
			tokens = append(tokens, gqlToken{tokName, input[i:j]})
			i = j
			continue
		}
		return nil, fmt.Errorf("unexpected character %q at %d", string(ch), i)
	}
	tokens = append(tokens, gqlToken{tokEOF, ""})
	return tokens, nil
}

// ---------------------------------------------------------------------------
// parser
// ---------------------------------------------------------------------------

type valueKind int

const (
	valVariable valueKind = iota
	valInt
	valFloat
	valString
	valBoolean
	valNull
	valEnum
	valList
	valObject
)

type valueNode struct {
	kind   valueKind
	num    float64
	str    string
	boolean bool
	values []*valueNode
	fields []objField
	name   string // variable name
}

type objField struct {
	name  string
	value *valueNode
}

type fieldNode struct {
	name       string
	alias      string
	args       []argNode
	selections []selectionNode
}

type argNode struct {
	name  string
	value *valueNode
}

type fragmentSpreadNode struct{ name string }
type inlineFragmentNode struct {
	typeCondition string
	selections    []selectionNode
}

type selectionNode struct {
	kind     string // "field" | "fragmentSpread" | "inlineFragment"
	field    *fieldNode
	spread   *fragmentSpreadNode
	inline   *inlineFragmentNode
}

type operationNode struct {
	operation           string // "query" | "mutation"
	name                string
	variableDefinitions []varDef
	selections          []selectionNode
}

type varDef struct {
	name         string
	defaultValue *valueNode
}

type fragmentNode struct {
	name          string
	typeCondition string
	selections    []selectionNode
}

type documentNode struct {
	definitions []any // *operationNode | *fragmentNode
}

type gqlParser struct {
	pos    int
	tokens []gqlToken
}

func (p *gqlParser) peek() gqlToken  { return p.tokens[p.pos] }
func (p *gqlParser) next() gqlToken { t := p.tokens[p.pos]; p.pos++; return t }

func (p *gqlParser) expectPunct(value string) error {
	t := p.next()
	if t.kind != tokPunct || t.value != value {
		return fmt.Errorf("expected %s, got %s", value, t.value)
	}
	return nil
}

func (p *gqlParser) expectName(value ...string) (string, error) {
	t := p.next()
	if t.kind != tokName || len(value) > 0 && t.value != value[0] {
		return "", fmt.Errorf("expected name %v, got %s", value, t.value)
	}
	return t.value, nil
}

func (p *gqlParser) skipDirectives() error {
	for p.peek().kind == tokPunct && p.peek().value == "@" {
		p.next()
		if _, err := p.expectName(); err != nil {
			return err
		}
		if p.peek().kind == tokPunct && p.peek().value == "(" {
			if _, err := p.parseArguments(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *gqlParser) parseDocument() (*documentNode, error) {
	doc := &documentNode{}
	for p.peek().kind != tokEOF {
		def, err := p.parseDefinition()
		if err != nil {
			return nil, err
		}
		doc.definitions = append(doc.definitions, def)
	}
	return doc, nil
}

func (p *gqlParser) parseDefinition() (any, error) {
	t := p.peek()
	if t.kind == tokPunct && t.value == "{" {
		sels, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		return &operationNode{operation: "query", selections: sels}, nil
	}
	if t.kind == tokName && (t.value == "query" || t.value == "mutation") {
		p.next()
		var name string
		if p.peek().kind == tokName {
			name = p.next().value
		}
		varDefs, err := p.parseVariableDefinitions()
		if err != nil {
			return nil, err
		}
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
		sels, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		return &operationNode{operation: t.value, name: name, variableDefinitions: varDefs, selections: sels}, nil
	}
	if t.kind == tokName && t.value == "fragment" {
		p.next()
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if _, err := p.expectName("on"); err != nil {
			return nil, err
		}
		typeCond, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
		sels, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		return &fragmentNode{name: name, typeCondition: typeCond, selections: sels}, nil
	}
	return nil, fmt.Errorf("unexpected definition %s", t.value)
}

func (p *gqlParser) parseVariableDefinitions() ([]varDef, error) {
	var defs []varDef
	if p.peek().kind == tokPunct && p.peek().value == "(" {
		p.next()
		for !(p.peek().kind == tokPunct && p.peek().value == ")") {
			if err := p.expectPunct("$"); err != nil {
				return nil, err
			}
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			if err := p.parseType(); err != nil {
				return nil, err
			}
			var def *valueNode
			if p.peek().kind == tokPunct && p.peek().value == "=" {
				p.next()
				def, err = p.parseValue(true)
				if err != nil {
					return nil, err
				}
			}
			defs = append(defs, varDef{name: name, defaultValue: def})
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
	}
	return defs, nil
}

func (p *gqlParser) parseType() error {
	if p.peek().kind == tokPunct && p.peek().value == "[" {
		p.next()
		if err := p.parseType(); err != nil {
			return err
		}
		if err := p.expectPunct("]"); err != nil {
			return err
		}
	} else {
		if _, err := p.expectName(); err != nil {
			return err
		}
	}
	if p.peek().kind == tokPunct && p.peek().value == "!" {
		p.next()
	}
	return nil
}

func (p *gqlParser) parseSelectionSet() ([]selectionNode, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var sels []selectionNode
	for !(p.peek().kind == tokPunct && p.peek().value == "}") {
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		sels = append(sels, sel)
	}
	if err := p.expectPunct("}"); err != nil {
		return nil, err
	}
	return sels, nil
}

func (p *gqlParser) parseSelection() (selectionNode, error) {
	if p.peek().kind == tokPunct && p.peek().value == "..." {
		p.next()
		if p.peek().kind == tokName && p.peek().value != "on" {
			name := p.next().value
			if err := p.skipDirectives(); err != nil {
				return selectionNode{}, err
			}
			return selectionNode{kind: "fragmentSpread", spread: &fragmentSpreadNode{name: name}}, nil
		}
		var typeCond string
		if p.peek().kind == tokName && p.peek().value == "on" {
			p.next()
			tc, err := p.expectName()
			if err != nil {
				return selectionNode{}, err
			}
			typeCond = tc
		}
		if err := p.skipDirectives(); err != nil {
			return selectionNode{}, err
		}
		sels, err := p.parseSelectionSet()
		if err != nil {
			return selectionNode{}, err
		}
		return selectionNode{kind: "inlineFragment", inline: &inlineFragmentNode{typeCondition: typeCond, selections: sels}}, nil
	}
	first, err := p.expectName()
	if err != nil {
		return selectionNode{}, err
	}
	alias := ""
	name := first
	if p.peek().kind == tokPunct && p.peek().value == ":" {
		p.next()
		alias = first
		name, err = p.expectName()
		if err != nil {
			return selectionNode{}, err
		}
	}
	args, err := p.parseArguments()
	if err != nil {
		return selectionNode{}, err
	}
	if err := p.skipDirectives(); err != nil {
		return selectionNode{}, err
	}
	var sels []selectionNode
	if p.peek().kind == tokPunct && p.peek().value == "{" {
		sels, err = p.parseSelectionSet()
		if err != nil {
			return selectionNode{}, err
		}
	}
	return selectionNode{kind: "field", field: &fieldNode{name: name, alias: alias, args: args, selections: sels}}, nil
}

func (p *gqlParser) parseArguments() ([]argNode, error) {
	var args []argNode
	if p.peek().kind == tokPunct && p.peek().value == "(" {
		p.next()
		for !(p.peek().kind == tokPunct && p.peek().value == ")") {
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			val, err := p.parseValue(false)
			if err != nil {
				return nil, err
			}
			args = append(args, argNode{name: name, value: val})
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
	}
	return args, nil
}

func (p *gqlParser) parseValue(isConst bool) (*valueNode, error) {
	t := p.peek()
	if t.kind == tokPunct && t.value == "$" {
		if isConst {
			return nil, fmt.Errorf("variable in constant value")
		}
		p.next()
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		return &valueNode{kind: valVariable, name: name}, nil
	}
	if t.kind == tokPunct && t.value == "[" {
		p.next()
		v := &valueNode{kind: valList}
		for !(p.peek().kind == tokPunct && p.peek().value == "]") {
			child, err := p.parseValue(isConst)
			if err != nil {
				return nil, err
			}
			v.values = append(v.values, child)
		}
		if err := p.expectPunct("]"); err != nil {
			return nil, err
		}
		return v, nil
	}
	if t.kind == tokPunct && t.value == "{" {
		p.next()
		v := &valueNode{kind: valObject}
		for !(p.peek().kind == tokPunct && p.peek().value == "}") {
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			child, err := p.parseValue(isConst)
			if err != nil {
				return nil, err
			}
			v.fields = append(v.fields, objField{name: name, value: child})
		}
		if err := p.expectPunct("}"); err != nil {
			return nil, err
		}
		return v, nil
	}
	if t.kind == tokInt {
		p.next()
		n, err := strconv.ParseFloat(t.value, 64)
		if err != nil {
			return nil, err
		}
		return &valueNode{kind: valInt, num: n}, nil
	}
	if t.kind == tokFloat {
		p.next()
		n, err := strconv.ParseFloat(t.value, 64)
		if err != nil {
			return nil, err
		}
		return &valueNode{kind: valFloat, num: n}, nil
	}
	if t.kind == tokString {
		p.next()
		return &valueNode{kind: valString, str: t.value}, nil
	}
	if t.kind == tokName {
		p.next()
		switch t.value {
		case "true":
			return &valueNode{kind: valBoolean, boolean: true}, nil
		case "false":
			return &valueNode{kind: valBoolean, boolean: false}, nil
		case "null":
			return &valueNode{kind: valNull}, nil
		default:
			return &valueNode{kind: valEnum, str: t.value}, nil
		}
	}
	return nil, fmt.Errorf("unexpected value %s", t.value)
}

// ---------------------------------------------------------------------------
// executor
// ---------------------------------------------------------------------------

type gqlError struct {
	Message string        `json:"message"`
	Path    []any         `json:"path,omitempty"`
}

type execCtx struct {
	store     *Store
	variables map[string]any
	fragments map[string]*fragmentNode
	errors    []gqlError
}

// source is a GraphQL object: a map of fields plus __typename and hidden
// back-pointers (__repo, __issue, __pull) the executor reads directly.
type source map[string]any

func (s source) typename() string {
	if t, ok := s["__typename"].(string); ok {
		return t
	}
	return "Query"
}

func decodeGid(id string) (kind string, parts []string, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(id)
		if err != nil {
			return "", nil, false
		}
	}
	text := string(raw)
	segs := strings.Split(text, ":")
	if len(segs) < 2 {
		return "", nil, false
	}
	return segs[0], segs[1:], true
}

func evalValue(node *valueNode, ctx *execCtx) any {
	switch node.kind {
	case valVariable:
		return ctx.variables[node.name]
	case valInt, valFloat:
		return node.num
	case valString:
		return node.str
	case valBoolean:
		return node.boolean
	case valNull:
		return nil
	case valEnum:
		return node.str
	case valList:
		out := make([]any, 0, len(node.values))
		for _, child := range node.values {
			out = append(out, evalValue(child, ctx))
		}
		return out
	case valObject:
		out := map[string]any{}
		for _, f := range node.fields {
			out[f.name] = evalValue(f.value, ctx)
		}
		return out
	}
	return nil
}

func evalArgs(field *fieldNode, ctx *execCtx) map[string]any {
	out := map[string]any{}
	for _, arg := range field.args {
		out[arg.name] = evalValue(arg.value, ctx)
	}
	return out
}

func cursorFor(seq int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte("c:" + itoa64(seq)))
}

func seqAfter(after any) int64 {
	s, ok := after.(string)
	if !ok {
		return 0
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0
	}
	text := string(raw)
	if !strings.HasPrefix(text, "c:") {
		return 0
	}
	n, err := strconv.ParseInt(text[2:], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// connection builds a connection object over a seq-ordered list.
func gqlConnection(items []any, seqs []int64, args map[string]any, mapNode func(item any, seq int64) source, typename string) source {
	first := len(items)
	if f, ok := args["first"].(float64); ok {
		first = int(f)
		if first < 0 {
			first = 0
		}
	}
	afterSeq := seqAfter(args["after"])
	var filtered []int
	for i, seq := range seqs {
		if seq > afterSeq {
			filtered = append(filtered, i)
		}
	}
	end := first
	if end > len(filtered) {
		end = len(filtered)
	}
	page := filtered[:end]
	nodes := make([]any, 0, len(page))
	edges := make([]any, 0, len(page))
	for _, i := range page {
		node := mapNode(items[i], seqs[i])
		nodes = append(nodes, node)
		edges = append(edges, map[string]any{"__typename": "IssueEdge", "node": node, "cursor": cursorFor(seqs[i])})
	}
	var startCursor, endCursor any
	if len(page) > 0 {
		startCursor = cursorFor(seqs[page[0]])
		endCursor = cursorFor(seqs[page[len(page)-1]])
	}
	return source{
		"__typename": typename,
		"nodes":      nodes,
		"edges":      edges,
		"pageInfo": map[string]any{
			"__typename":    "PageInfo",
			"hasNextPage":   len(filtered) > len(page),
			"hasPreviousPage": afterSeq > 0,
			"startCursor":   startCursor,
			"endCursor":     endCursor,
		},
		"totalCount": len(filtered),
	}
}

func issueSource(repo *Repo, issue *Issue) source {
	comments := make([]any, 0, len(issue.Comments))
	seqs := make([]int64, 0, len(issue.Comments))
	for _, c := range issue.Comments {
		comments = append(comments, c)
		seqs = append(seqs, c.Seq)
	}
	commentConn := gqlConnection(comments, seqs, map[string]any{}, func(item any, seq int64) source {
		comment := item.(*Comment)
		return source{
			"__typename": "IssueComment",
			"id":         globalID("C", repo.ID, issue.Number, comment.ID),
			"databaseId": comment.ID,
			"body":       comment.Body,
			"createdAt":  isoTime(comment.Seq),
			"author":     map[string]any{"__typename": "User", "login": ViewerLogin, "id": ViewerID},
		}
	}, "IssueCommentConnection")
	return source{
		"__typename": "Issue",
		"__repo":     repo,
		"__issue":    issue,
		"id":         globalID("I", repo.ID, issue.Number),
		"databaseId": issue.ID,
		"number":     issue.Number,
		"title":      issue.Title,
		"body":       issue.Body,
		"state":      map[bool]string{true: "OPEN", false: "CLOSED"}[issue.State == "open"],
		"createdAt":  isoTime(issue.Seq),
		"updatedAt":  isoTime(issue.Seq),
		"url":        "https://github.com/" + repo.Owner + "/" + repo.Name + "/issues/" + itoa(issue.Number),
		"author":     map[string]any{"__typename": "User", "login": ViewerLogin, "id": ViewerID},
		"repository": repoSource(repo),
		"comments":   commentConn,
	}
}

func pullSource(store *Store, repo *Repo, pull *Pull) source {
	files := store.PullFiles(repo, pull)
	items := make([]any, 0, len(files))
	seqs := make([]int64, 0, len(files))
	for i, f := range files {
		items = append(items, f)
		seqs = append(seqs, int64(i+1))
	}
	fileConn := gqlConnection(items, seqs, map[string]any{}, func(item any, seq int64) source {
		f := item.(pullFile)
		changeType := "MODIFIED"
		if f.Status == "added" {
			changeType = "ADDED"
		}
		return source{
			"__typename": "PullRequestChangedFile",
			"path":       f.Filename,
			"additions":  f.Additions,
			"deletions":  f.Deletions,
			"changeType": changeType,
		}
	}, "PullRequestChangedFileConnection")
	state := "CLOSED"
	if pull.Merged {
		state = "MERGED"
	} else if pull.State == "open" {
		state = "OPEN"
	}
	mergeable := store.PullMergeable(repo, pull)
	if mergeable == "UNKNOWN" {
		mergeable = "CONFLICTING"
	}
	return source{
		"__typename":  "PullRequest",
		"__repo":      repo,
		"__pull":      pull,
		"id":          globalID("P", repo.ID, pull.Number),
		"databaseId":  pull.ID,
		"number":      pull.Number,
		"title":       pull.Title,
		"body":        pull.Body,
		"state":       state,
		"merged":      pull.Merged,
		"mergeable":   mergeable,
		"headRefName": pull.HeadRef,
		"baseRefName": pull.BaseRef,
		"createdAt":   isoTime(pull.Seq),
		"updatedAt":   isoTime(pull.Seq),
		"url":         "https://github.com/" + repo.Owner + "/" + repo.Name + "/pull/" + itoa(pull.Number),
		"author":      map[string]any{"__typename": "User", "login": ViewerLogin, "id": ViewerID},
		"repository":  repoSource(repo),
		"files":       fileConn,
	}
}

func repoSource(repo *Repo) source {
	return source{
		"__typename":       "Repository",
		"__repo":           repo,
		"id":               globalID("R", repo.ID),
		"databaseId":       repo.ID,
		"name":             repo.Name,
		"nameWithOwner":    repo.Owner + "/" + repo.Name,
		"description":      repo.Description,
		"url":              "https://github.com/" + repo.Owner + "/" + repo.Name,
		"owner":            map[string]any{"__typename": "User", "login": repo.Owner, "id": ViewerID},
		"defaultBranchRef": map[string]any{"__typename": "Ref", "name": repo.DefaultBranch, "prefix": "refs/heads/"},
		"viewerPermission": map[bool]string{true: "ADMIN", false: "READ"}[repo.Owner == ViewerLogin],
		"hasIssuesEnabled": true,
		"createdAt":        isoTime(repo.Seq),
		"updatedAt":        isoTime(repo.Seq),
	}
}

// The fields each introspected type exposes (used by __type).
var typeFields = map[string][]string{
	"Issue":                 {"id", "databaseId", "number", "title", "body", "state", "createdAt", "updatedAt", "author", "repository", "url", "comments"},
	"PullRequest":           {"id", "databaseId", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName", "files", "author", "repository", "url", "createdAt", "updatedAt"},
	"Repository":            {"id", "databaseId", "name", "nameWithOwner", "description", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequest", "pullRequests", "url", "createdAt", "updatedAt"},
	"User":                  {"id", "databaseId", "login", "name", "email"},
	"IssueConnection":       {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestConnection": {"nodes", "edges", "pageInfo", "totalCount"},
	"PageInfo":              {"hasNextPage", "hasPreviousPage", "startCursor", "endCursor"},
	"Ref":                   {"name", "prefix"},
}

var enumValues = map[string][]string{
	"SearchType":      {"ISSUE", "REPOSITORY", "USER", "DISCUSSION"},
	"IssueState":      {"OPEN", "CLOSED"},
	"PullRequestState": {"OPEN", "CLOSED", "MERGED"},
}

func typeSource(name string) source {
	if fields, ok := typeFields[name]; ok {
		fieldObjs := make([]any, 0, len(fields))
		for _, f := range fields {
			fieldObjs = append(fieldObjs, map[string]any{
				"__typename": "__Field",
				"name":       f,
				"type":       map[string]any{"__typename": "__Type", "name": "String", "kind": "SCALAR"},
			})
		}
		return source{
			"__typename": "__Type",
			"name":       name,
			"kind":       "OBJECT",
			"fields":     fieldObjs,
			"enumValues": nil,
		}
	}
	if values, ok := enumValues[name]; ok {
		enumObjs := make([]any, 0, len(values))
		for _, v := range values {
			enumObjs = append(enumObjs, map[string]any{"__typename": "__EnumValue", "name": v})
		}
		return source{
			"__typename": "__Type",
			"name":       name,
			"kind":       "ENUM",
			"fields":     nil,
			"enumValues": enumObjs,
		}
	}
	return nil
}

func resolveNode(store *Store, id string) source {
	kind, parts, ok := decodeGid(id)
	if !ok {
		return nil
	}
	atoi := func(s string) int {
		n, _ := strconv.Atoi(s)
		return n
	}
	switch kind {
	case "R":
		repo := store.ReposByID()[atoi(parts[0])]
		if repo == nil {
			return nil
		}
		return repoSource(repo)
	case "I":
		repo := store.ReposByID()[atoi(parts[0])]
		if repo == nil {
			return nil
		}
		issue := repo.Issues[atoi(parts[1])]
		if issue == nil {
			return nil
		}
		return issueSource(repo, issue)
	case "P":
		repo := store.ReposByID()[atoi(parts[0])]
		if repo == nil {
			return nil
		}
		pull := repo.Pulls[atoi(parts[1])]
		if pull == nil {
			return nil
		}
		return pullSource(store, repo, pull)
	case "C":
		repo := store.ReposByID()[atoi(parts[0])]
		if repo == nil {
			return nil
		}
		issue := repo.Issues[atoi(parts[1])]
		if issue == nil {
			return nil
		}
		var comment *Comment
		for _, c := range issue.Comments {
			if c.ID == atoi(parts[2]) {
				comment = c
				break
			}
		}
		if comment == nil {
			return nil
		}
		return source{
			"__typename": "IssueComment",
			"id":         globalID("C", repo.ID, issue.Number, comment.ID),
			"databaseId": comment.ID,
			"body":       comment.Body,
			"createdAt":  isoTime(comment.Seq),
			"author":     map[string]any{"__typename": "User", "login": ViewerLogin, "id": ViewerID},
		}
	}
	return nil
}

// resolveField resolves one field on a source object; nil with ok=false
// means the field does not exist.
func resolveField(src source, field *fieldNode, ctx *execCtx, path []any) (any, bool) {
	typeName := src.typename()
	args := evalArgs(field, ctx)
	store := ctx.store

	// fields carried directly on the source object
	if typeName == "Repository" {
		repo := src["__repo"].(*Repo)
		switch field.name {
		case "issue":
			num := -1
			if f, ok := args["number"].(float64); ok {
				num = int(f)
			}
			issue := repo.Issues[num]
			if issue == nil {
				return nil, true
			}
			return issueSource(repo, issue), true
		case "issues":
			var list []*Issue
			for _, issue := range repo.Issues {
				list = append(list, issue)
			}
			if states, ok := args["states"].([]any); ok {
				wanted := map[string]bool{}
				for _, st := range states {
					wanted[strings.ToLower(fmt.Sprint(st))] = true
				}
				filtered := list[:0]
				for _, issue := range list {
					if wanted[issue.State] {
						filtered = append(filtered, issue)
					}
				}
				list = filtered
			}
			if orderBy, ok := args["orderBy"].(map[string]any); ok {
				direction := fmt.Sprint(orderBy["direction"])
				if direction == "DESC" {
					sort.Slice(list, func(i, j int) bool { return list[i].Seq > list[j].Seq })
				} else {
					sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
				}
			} else {
				sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
			}
			items := make([]any, 0, len(list))
			seqs := make([]int64, 0, len(list))
			for _, issue := range list {
				items = append(items, issue)
				seqs = append(seqs, issue.Seq)
			}
			return gqlConnection(items, seqs, args, func(item any, seq int64) source {
				return issueSource(repo, item.(*Issue))
			}, "IssueConnection"), true
		case "pullRequest":
			num := -1
			if f, ok := args["number"].(float64); ok {
				num = int(f)
			}
			pull := repo.Pulls[num]
			if pull == nil {
				return nil, true
			}
			return pullSource(store, repo, pull), true
		case "pullRequests":
			var list []*Pull
			for _, pull := range repo.Pulls {
				list = append(list, pull)
			}
			if states, ok := args["states"].([]any); ok {
				wanted := map[string]bool{}
				for _, st := range states {
					s := fmt.Sprint(st)
					if s == "MERGED" {
						s = "closed"
					} else {
						s = strings.ToLower(s)
					}
					wanted[s] = true
				}
				filtered := list[:0]
				for _, pull := range list {
					st := pull.State
					if pull.Merged {
						st = "closed"
					}
					if wanted[st] {
						filtered = append(filtered, pull)
					}
				}
				list = filtered
			} else {
				filtered := list[:0]
				for _, pull := range list {
					if pull.State == "open" {
						filtered = append(filtered, pull)
					}
				}
				list = filtered
			}
			sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
			items := make([]any, 0, len(list))
			seqs := make([]int64, 0, len(list))
			for _, pull := range list {
				items = append(items, pull)
				seqs = append(seqs, pull.Seq)
			}
			return gqlConnection(items, seqs, args, func(item any, seq int64) source {
				return pullSource(store, repo, item.(*Pull))
			}, "PullRequestConnection"), true
		}
	}

	if typeName == "Query" {
		switch field.name {
		case "viewer":
			return map[string]any{"__typename": "User", "login": ViewerLogin, "id": ViewerID, "databaseId": ViewerID, "name": ViewerLogin, "email": "adaiversen@example.invalid"}, true
		case "repository":
			repo := store.GetRepo(fmt.Sprint(args["owner"]), fmt.Sprint(args["name"]))
			if repo == nil {
				return nil, true
			}
			return repoSource(repo), true
		case "node":
			if id, ok := args["id"].(string); ok {
				return resolveNode(store, id), true
			}
			return nil, true
		case "__type":
			if name, ok := args["name"].(string); ok {
				return typeSource(name), true
			}
			return nil, true
		case "__schema":
			return map[string]any{"__typename": "__Schema", "types": []any{}}, true
		}
	}

	if typeName == "Mutation" {
		switch field.name {
		case "createIssue":
			input, _ := args["input"].(map[string]any)
			var repo *Repo
			if id, ok := input["repositoryId"].(string); ok {
				if node := resolveNode(store, id); node != nil {
					repo, _ = node["__repo"].(*Repo)
				}
			}
			title, _ := input["title"].(string)
			if repo == nil || title == "" {
				ctx.errors = append(ctx.errors, gqlError{Message: "createIssue: the repositoryId names no repository", Path: path})
				return nil, true
			}
			body, _ := input["body"].(string)
			issue := store.CreateIssue(repo, title, body, []string{})
			return map[string]any{"__typename": "CreateIssuePayload", "issue": issueSource(repo, issue)}, true
		case "closeIssue":
			input, _ := args["input"].(map[string]any)
			var issue *Issue
			var repo *Repo
			if id, ok := input["issueId"].(string); ok {
				if node := resolveNode(store, id); node != nil {
					issue, _ = node["__issue"].(*Issue)
					repo, _ = node["__repo"].(*Repo)
				}
			}
			if issue == nil || repo == nil {
				ctx.errors = append(ctx.errors, gqlError{Message: "closeIssue: the issueId names no issue", Path: path})
				return nil, true
			}
			issue.State = "closed"
			store.cacheMu.Lock()
			issue.cached = nil
			store.cacheMu.Unlock()
			return map[string]any{"__typename": "CloseIssuePayload", "issue": issueSource(repo, issue)}, true
		case "createPullRequest":
			input, _ := args["input"].(map[string]any)
			var repo *Repo
			if id, ok := input["repositoryId"].(string); ok {
				if node := resolveNode(store, id); node != nil {
					repo, _ = node["__repo"].(*Repo)
				}
			}
			head, _ := input["headRefName"].(string)
			baseBranch, _ := input["baseRefName"].(string)
			title, _ := input["title"].(string)
			if repo == nil || head == "" || baseBranch == "" || title == "" {
				ctx.errors = append(ctx.errors, gqlError{Message: "createPullRequest: repositoryId, baseRefName, headRefName and title are required", Path: path})
				return nil, true
			}
			headSha, hasHead := repo.Refs["refs/heads/"+head]
			baseSha, hasBase := repo.Refs["refs/heads/"+baseBranch]
			if !hasHead || !hasBase {
				ctx.errors = append(ctx.errors, gqlError{Message: "createPullRequest: head and base must name existing branches", Path: path})
				return nil, true
			}
			for _, existing := range repo.Pulls {
				if existing.State == "open" && existing.HeadRef == head && existing.BaseRef == baseBranch {
					return map[string]any{"__typename": "CreatePullRequestPayload", "pullRequest": pullSource(store, repo, existing)}, true
				}
			}
			body, _ := input["body"].(string)
			pull := store.CreatePull(repo, head, baseBranch, headSha, baseSha, title, body)
			return map[string]any{"__typename": "CreatePullRequestPayload", "pullRequest": pullSource(store, repo, pull)}, true
		case "mergePullRequest":
			input, _ := args["input"].(map[string]any)
			var pull *Pull
			var repo *Repo
			if id, ok := input["pullRequestId"].(string); ok {
				if node := resolveNode(store, id); node != nil {
					pull, _ = node["__pull"].(*Pull)
					repo, _ = node["__repo"].(*Repo)
				}
			}
			if pull == nil || repo == nil {
				ctx.errors = append(ctx.errors, gqlError{Message: "mergePullRequest: the pullRequestId names no pull request", Path: path})
				return nil, true
			}
			if pull.State == "open" {
				merged := store.SquashMerge(repo, pull)
				if !merged.OK {
					ctx.errors = append(ctx.errors, gqlError{Message: "mergePullRequest: " + merged.Reason, Path: path})
				}
			}
			return map[string]any{"__typename": "MergePullRequestPayload", "pullRequest": pullSource(store, repo, pull)}, true
		case "addComment":
			input, _ := args["input"].(map[string]any)
			var issue *Issue
			var repo *Repo
			if id, ok := input["subjectId"].(string); ok {
				if node := resolveNode(store, id); node != nil {
					issue, _ = node["__issue"].(*Issue)
					repo, _ = node["__repo"].(*Repo)
				}
			}
			body, hasBody := input["body"].(string)
			if issue == nil || repo == nil || !hasBody {
				ctx.errors = append(ctx.errors, gqlError{Message: "addComment: the subjectId names no issue", Path: path})
				return nil, true
			}
			comment := store.CreateComment(repo, issue, body)
			return map[string]any{
				"__typename": "AddCommentPayload",
				"commentEdge": map[string]any{
					"__typename": "IssueCommentEdge",
					"node": map[string]any{
						"__typename": "IssueComment",
						"id":         globalID("C", repo.ID, issue.Number, comment.ID),
						"databaseId": comment.ID,
						"body":       comment.Body,
						"createdAt":  isoTime(comment.Seq),
						"author":     map[string]any{"__typename": "User", "login": ViewerLogin, "id": ViewerID},
					},
				},
			}, true
		}
	}

	// plain properties carried on the source (scalars and pre-built children)
	if field.name != "__typename" {
		if v, ok := src[field.name]; ok {
			return v, true
		}
	}
	return nil, false
}

func executeSelections(selections []selectionNode, src source, ctx *execCtx, path []any) map[string]any {
	out := map[string]any{}
	typeName := src.typename()
	for _, sel := range selections {
		switch sel.kind {
		case "fragmentSpread":
			fragment := ctx.fragments[sel.spread.name]
			if fragment == nil {
				ctx.errors = append(ctx.errors, gqlError{Message: "unknown fragment \"" + sel.spread.name + "\"", Path: path})
				continue
			}
			for k, v := range executeSelections(fragment.selections, src, ctx, path) {
				out[k] = v
			}
			continue
		case "inlineFragment":
			if sel.inline.typeCondition != "" && sel.inline.typeCondition != typeName {
				continue
			}
			for k, v := range executeSelections(sel.inline.selections, src, ctx, path) {
				out[k] = v
			}
			continue
		}
		field := sel.field
		key := field.name
		if field.alias != "" {
			key = field.alias
		}
		fieldPath := append(append([]any{}, path...), key)
		value, ok := resolveField(src, field, ctx, fieldPath)
		if !ok {
			ctx.errors = append(ctx.errors, gqlError{
				Message: "Cannot query field \"" + field.name + "\" on type \"" + typeName + "\".",
				Path:    fieldPath,
			})
			out[key] = nil
			continue
		}
		if value == nil {
			out[key] = nil
			continue
		}
		if len(field.selections) == 0 {
			out[key] = value
			continue
		}
		if arr, isArr := value.([]any); isArr {
			mapped := make([]any, 0, len(arr))
			for i, item := range arr {
				if rec, isRec := item.(map[string]any); isRec {
					mapped = append(mapped, executeSelections(field.selections, source(rec), ctx, append(append([]any{}, fieldPath...), i)))
				} else if srcItem, isSrc := item.(source); isSrc {
					mapped = append(mapped, executeSelections(field.selections, srcItem, ctx, append(append([]any{}, fieldPath...), i)))
				} else {
					mapped = append(mapped, item)
				}
			}
			out[key] = mapped
			continue
		}
		if rec, isRec := value.(map[string]any); isRec {
			out[key] = executeSelections(field.selections, source(rec), ctx, fieldPath)
			continue
		}
		if srcVal, isSrc := value.(source); isSrc {
			out[key] = executeSelections(field.selections, srcVal, ctx, fieldPath)
			continue
		}
		out[key] = value
	}
	return out
}

func executeOperation(op *operationNode, doc *documentNode, store *Store, variables map[string]any) map[string]any {
	fragments := map[string]*fragmentNode{}
	for _, def := range doc.definitions {
		if frag, ok := def.(*fragmentNode); ok {
			fragments[frag.name] = frag
		}
	}
	ctx := &execCtx{store: store, variables: map[string]any{}, fragments: fragments}
	for k, v := range variables {
		ctx.variables[k] = v
	}
	for _, def := range op.variableDefinitions {
		if _, has := ctx.variables[def.name]; !has && def.defaultValue != nil {
			ctx.variables[def.name] = evalValue(def.defaultValue, ctx)
		}
	}
	root := source{"__typename": "Query"}
	if op.operation == "mutation" {
		root = source{"__typename": "Mutation"}
	}
	data := executeSelections(op.selections, root, ctx, []any{})
	result := map[string]any{"data": data}
	if len(ctx.errors) > 0 {
		result["errors"] = ctx.errors
	}
	return result
}

// handleGraphQL runs one GraphQL request body (a single operation or a batch
// array).
func handleGraphQL(store *Store, body any) (int, any) {
	requests, isArray := body.([]any)
	if !isArray {
		requests = []any{body}
	}
	results := make([]any, 0, len(requests))
	for _, request := range requests {
		rec, ok := request.(map[string]any)
		if !ok {
			results = append(results, map[string]any{"data": nil, "errors": []any{map[string]any{"message": "a GraphQL query string is required"}}})
			continue
		}
		query, ok := rec["query"].(string)
		if !ok {
			results = append(results, map[string]any{"data": nil, "errors": []any{map[string]any{"message": "a GraphQL query string is required"}}})
			continue
		}
		tokens, err := gqlLex(query)
		if err != nil {
			results = append(results, map[string]any{"data": nil, "errors": []any{map[string]any{"message": err.Error()}}})
			continue
		}
		doc, err := (&gqlParser{tokens: tokens}).parseDocument()
		if err != nil {
			results = append(results, map[string]any{"data": nil, "errors": []any{map[string]any{"message": err.Error()}}})
			continue
		}
		var operations []*operationNode
		for _, def := range doc.definitions {
			if op, ok := def.(*operationNode); ok {
				operations = append(operations, op)
			}
		}
		operationName, _ := rec["operationName"].(string)
		var operation *operationNode
		if operationName != "" {
			for _, candidate := range operations {
				if candidate.name == operationName {
					operation = candidate
					break
				}
			}
		} else if len(operations) > 0 {
			operation = operations[0]
		}
		if operation == nil {
			results = append(results, map[string]any{"data": nil, "errors": []any{map[string]any{"message": "no operation found in the document"}}})
			continue
		}
		variables, _ := rec["variables"].(map[string]any)
		if variables == nil {
			variables = map[string]any{}
		}
		results = append(results, executeOperation(operation, doc, store, variables))
	}
	if isArray {
		return 200, results
	}
	return 200, results[0]
}
