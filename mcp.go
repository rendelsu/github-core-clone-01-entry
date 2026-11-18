// mcp.go — the Model Context Protocol surface over POST /mcp (Streamable
// HTTP, JSON-RPC 2.0). The modern 2026-07-28 discovery handshake is served,
// with the older `initialize` handshake as a fallback. Six tools expose the
// forge to agents; every tool reads and writes the same Store the REST and
// GraphQL surfaces use, so work done through MCP shows up in /api/v3.

package main

import (
	"regexp"
	"strconv"
	"strings"
)

const serverName = "github-core-clone"
const serverVersion = "1.0.0"

type mcpResult struct {
	status int
	// body nil means "no body" (a notification got a bare 202).
	body any
}

type toolDef struct {
	name        string
	description string
	properties  map[string]map[string]string
	required    []string
}

// The tool list is a static constant, so two tools/list calls answer
// byte-identically.
var mcpTools = []toolDef{
	{
		name:        "create_repo",
		description: "Create a new repository owned by the authenticated user.",
		properties: map[string]map[string]string{
			"name": {"type": "string", "description": "The name of the repository to create."},
		},
		required: []string{"name"},
	},
	{
		name:        "get_repo",
		description: "Get a repository by owner and name.",
		properties: map[string]map[string]string{
			"owner": {"type": "string", "description": "The owner of the repository."},
			"repo":  {"type": "string", "description": "The name of the repository."},
		},
		required: []string{"owner", "repo"},
	},
	{
		name:        "create_issue",
		description: "Create an issue in a repository.",
		properties: map[string]map[string]string{
			"owner": {"type": "string", "description": "The owner of the repository."},
			"repo":  {"type": "string", "description": "The name of the repository."},
			"title": {"type": "string", "description": "The title of the issue."},
			"body":  {"type": "string", "description": "The body of the issue."},
		},
		required: []string{"owner", "repo", "title"},
	},
	{
		name:        "list_issues",
		description: "List issues in a repository.",
		properties: map[string]map[string]string{
			"owner": {"type": "string", "description": "The owner of the repository."},
			"repo":  {"type": "string", "description": "The name of the repository."},
			"state": {"type": "string", "description": "Filter by state: open, closed or all."},
		},
		required: []string{"owner", "repo"},
	},
	{
		name:        "create_pull_request",
		description: "Create a pull request from a head branch onto a base branch.",
		properties: map[string]map[string]string{
			"owner": {"type": "string", "description": "The owner of the repository."},
			"repo":  {"type": "string", "description": "The name of the repository."},
			"title": {"type": "string", "description": "The title of the pull request."},
			"head":  {"type": "string", "description": "The head branch name."},
			"base":  {"type": "string", "description": "The base branch name."},
			"body":  {"type": "string", "description": "The body of the pull request."},
		},
		required: []string{"owner", "repo", "title", "head", "base"},
	},
	{
		name:        "merge_pull_request",
		description: "Squash-merge a pull request.",
		properties: map[string]map[string]string{
			"owner":       {"type": "string", "description": "The owner of the repository."},
			"repo":        {"type": "string", "description": "The name of the repository."},
			"pull_number": {"type": "integer", "description": "The pull request number."},
		},
		required: []string{"owner", "repo", "pull_number"},
	},
}

// toolsListJSON is the pre-serialized tools/list payload: two tools/list
// calls answer the same list, byte for byte.
var toolsListJSON = func() []byte {
	tools := make([]any, 0, len(mcpTools))
	for _, t := range mcpTools {
		props := map[string]any{}
		for name, spec := range t.properties {
			props[name] = map[string]any{"type": spec["type"], "description": spec["description"]}
		}
		tools = append(tools, map[string]any{
			"name":        t.name,
			"description": t.description,
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": props,
				"required":   t.required,
			},
		})
	}
	return marshalJSON(map[string]any{"tools": tools})
}()

// mcpArg pulls one argument out of the call, accepting the common spelling
// variants.
func mcpArg(args map[string]any, names ...string) any {
	for _, name := range names {
		if v, ok := args[name]; ok {
			return v
		}
	}
	return nil
}

// resolveMCPRepo resolves owner/repo from the arguments, including a
// full_name fallback.
func resolveMCPRepo(store *Store, args map[string]any) *Repo {
	owner, _ := mcpArg(args, "owner").(string)
	repoName, _ := mcpArg(args, "repo", "repository").(string)
	fullName, _ := mcpArg(args, "full_name", "fullName").(string)
	if (owner == "" || repoName == "") && fullName != "" {
		parts := strings.Split(fullName, "/")
		if len(parts) == 2 {
			if owner == "" {
				owner = parts[0]
			}
			if repoName == "" {
				repoName = parts[1]
			}
		}
	}
	if owner == "" || repoName == "" {
		return nil
	}
	return store.GetRepo(owner, repoName)
}

func mcpTextResult(value any) map[string]any {
	return map[string]any{
		"structuredContent": value,
		"content":           []any{map[string]any{"type": "text", "text": string(marshalJSON(value))}},
	}
}

func mcpErrorResult(message string) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": message}},
		"isError": true,
	}
}

var nonAlnumPrefix = regexp.MustCompile(`^[^a-z0-9]+`)

// callTool runs one tool call; the result is a tools/call result object. A
// result carrying an "error" key with code/message is a JSON-RPC error.
func callTool(store *Store, name string, args map[string]any) map[string]any {
	normalized := strings.ToLower(nonAlnumPrefix.ReplaceAllString(name, ""))
	var tool *toolDef
	for i := range mcpTools {
		if normalized == mcpTools[i].name || strings.HasSuffix(normalized, "__"+mcpTools[i].name) {
			tool = &mcpTools[i]
			break
		}
	}
	if tool == nil {
		return map[string]any{"error": map[string]any{"code": -32602, "message": "unknown tool: " + name}}
	}
	// required arguments, with the documented aliases accepted
	aliases := map[string][]string{
		"repo":        {"repo", "repository"},
		"pull_number": {"pull_number", "pullNumber", "number"},
		"name":        {"name", "repo", "repository"},
	}
	for _, required := range tool.required {
		names := aliases[required]
		if names == nil {
			names = []string{required}
		}
		if mcpArg(args, names...) == nil {
			return mcpErrorResult("missing required argument: " + required)
		}
	}

	switch tool.name {
	case "create_repo":
		repoName, _ := mcpArg(args, "name", "repo", "repository").(string)
		if repoName == "" {
			if full, ok := mcpArg(args, "full_name").(string); ok {
				if parts := strings.Split(full, "/"); len(parts) > 0 {
					repoName = parts[len(parts)-1]
				}
			}
		}
		if repoName == "" {
			return mcpErrorResult("missing required argument: name")
		}
		repo := store.CreateRepo(repoName, false, "")
		if repo == nil {
			return mcpErrorResult("Repository already exists: " + ViewerLogin + "/" + repoName)
		}
		return mcpTextResult(map[string]any{
			"full_name": repo.Owner + "/" + repo.Name, "name": repo.Name, "owner": repo.Owner,
			"private": repo.Priv, "default_branch": repo.DefaultBranch, "description": repo.Description,
		})
	case "get_repo":
		repo := resolveMCPRepo(store, args)
		if repo == nil {
			return mcpErrorResult("Repository not found")
		}
		return mcpTextResult(map[string]any{
			"full_name": repo.Owner + "/" + repo.Name, "name": repo.Name, "owner": repo.Owner,
			"private": repo.Priv, "default_branch": repo.DefaultBranch, "description": repo.Description,
			"created_at": isoTime(repo.Seq),
		})
	case "create_issue":
		repo := resolveMCPRepo(store, args)
		if repo == nil {
			return mcpErrorResult("Repository not found")
		}
		title, _ := mcpArg(args, "title").(string)
		if title == "" {
			return mcpErrorResult("missing required argument: title")
		}
		body, _ := mcpArg(args, "body").(string)
		issue := store.CreateIssue(repo, title, body, []string{})
		return mcpTextResult(map[string]any{
			"number": issue.Number, "id": issue.ID, "title": issue.Title, "body": issue.Body,
			"state": issue.State, "created_at": isoTime(issue.Seq),
		})
	case "list_issues":
		repo := resolveMCPRepo(store, args)
		if repo == nil {
			return mcpErrorResult("Repository not found")
		}
		state, _ := mcpArg(args, "state").(string)
		if state == "" {
			state = "open"
		}
		var list []*Issue
		for _, issue := range repo.Issues {
			list = append(list, issue)
		}
		filtered := list[:0]
		for _, issue := range list {
			if state == "open" && issue.State != "open" {
				continue
			}
			if state == "closed" && issue.State != "closed" {
				continue
			}
			filtered = append(filtered, issue)
		}
		list = filtered
		// sort by creation order (seq ascending)
		for i := 1; i < len(list); i++ {
			for j := i; j > 0 && list[j].Seq < list[j-1].Seq; j-- {
				list[j], list[j-1] = list[j-1], list[j]
			}
		}
		issues := make([]any, 0, len(list))
		for _, issue := range list {
			issues = append(issues, map[string]any{
				"number": issue.Number, "title": issue.Title, "body": issue.Body,
				"state": issue.State, "created_at": isoTime(issue.Seq),
			})
		}
		return mcpTextResult(map[string]any{"issues": issues})
	case "create_pull_request":
		repo := resolveMCPRepo(store, args)
		if repo == nil {
			return mcpErrorResult("Repository not found")
		}
		title, _ := mcpArg(args, "title").(string)
		head, _ := mcpArg(args, "head").(string)
		baseBranch, _ := mcpArg(args, "base").(string)
		if title == "" || head == "" || baseBranch == "" {
			return mcpErrorResult("title, head and base are required")
		}
		headSha, hasHead := repo.Refs["refs/heads/"+head]
		baseSha, hasBase := repo.Refs["refs/heads/"+baseBranch]
		if !hasHead || !hasBase {
			return mcpErrorResult("head and base must name existing branches")
		}
		for _, existing := range repo.Pulls {
			if existing.State == "open" && existing.HeadRef == head && existing.BaseRef == baseBranch {
				return mcpTextResult(map[string]any{"number": existing.Number, "title": existing.Title, "state": existing.State, "head": head, "base": baseBranch})
			}
		}
		body, _ := mcpArg(args, "body").(string)
		pull := store.CreatePull(repo, head, baseBranch, headSha, baseSha, title, body)
		return mcpTextResult(map[string]any{
			"number": pull.Number, "id": pull.ID, "title": pull.Title, "body": pull.Body,
			"state": pull.State, "head": head, "base": baseBranch,
		})
	case "merge_pull_request":
		repo := resolveMCPRepo(store, args)
		if repo == nil {
			return mcpErrorResult("Repository not found")
		}
		raw := mcpArg(args, "pull_number", "pullNumber", "number")
		number := 0
		switch v := raw.(type) {
		case float64:
			number = int(v)
		case string:
			number, _ = strconv.Atoi(v)
		case int:
			number = v
		}
		pull := repo.Pulls[number]
		if pull == nil {
			return mcpErrorResult("Pull request not found: " + itoa(number))
		}
		if pull.State != "open" {
			return mcpErrorResult("Pull request is already closed")
		}
		merged := store.SquashMerge(repo, pull)
		if !merged.OK {
			return mcpErrorResult("Pull request cannot be merged: " + merged.Reason)
		}
		return mcpTextResult(map[string]any{"number": pull.Number, "merged": true, "sha": merged.Sha, "state": pull.State})
	}
	return map[string]any{"error": map[string]any{"code": -32602, "message": "unknown tool: " + name}}
}

func rpcResult(id any, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func rpcError(id any, code int, message string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
}

// handleOneMCP handles one JSON-RPC request object. A nil result means the
// request is a notification answered with a bare 202.
func handleOneMCP(store *Store, request any) *mcpResult {
	rec, ok := request.(map[string]any)
	if !ok {
		return &mcpResult{status: 400, body: rpcError(nil, -32600, "Invalid Request")}
	}
	id, hasID := rec["id"]
	if !hasID {
		id = nil
	}
	isNotification := !hasID
	method, _ := rec["method"].(string)
	params, _ := rec["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}

	switch {
	case method == "server/discover":
		return &mcpResult{status: 200, body: rpcResult(id, map[string]any{
			"supportedVersions": []any{"2026-07-28", "2025-11-25"},
			"serverInfo":        map[string]any{"name": serverName, "version": serverVersion},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": serverName, "version": serverVersion}},
		})}
	case method == "initialize":
		return &mcpResult{status: 200, body: rpcResult(id, map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
		})}
	case method == "notifications/initialized" || strings.HasPrefix(method, "notifications/"):
		// a notification is answered with nothing (202, no body)
		if isNotification {
			return &mcpResult{status: 202, body: nil}
		}
		return &mcpResult{status: 200, body: rpcResult(id, map[string]any{})}
	case method == "ping":
		return &mcpResult{status: 200, body: rpcResult(id, map[string]any{})}
	case method == "tools/list":
		return &mcpResult{status: 200, body: rpcResult(id, rawJSON(toolsListJSON))}
	case method == "tools/call":
		name, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		result := callTool(store, name, args)
		if errObj, ok := result["error"].(map[string]any); ok {
			code, _ := errObj["code"].(int)
			if code == 0 {
				if f, ok := errObj["code"].(float64); ok {
					code = int(f)
				}
			}
			return &mcpResult{status: 200, body: rpcError(id, code, errObj["message"].(string))}
		}
		return &mcpResult{status: 200, body: rpcResult(id, result)}
	case method == "prompts/list":
		return &mcpResult{status: 200, body: rpcResult(id, map[string]any{"prompts": []any{}})}
	case method == "resources/list":
		return &mcpResult{status: 200, body: rpcResult(id, map[string]any{"resources": []any{}})}
	default:
		return &mcpResult{status: 200, body: rpcError(id, -32601, "Method not found: "+method)}
	}
}

var mcpIDRe = regexp.MustCompile(`"id"\s*:\s*("[^"]*"|\d+|null)`)

// handleMCP handles a POST /mcp body (raw bytes; a bad body answers -32700).
func handleMCP(store *Store, rawBody []byte) *mcpResult {
	parsed, ok := parseJSONBody(rawBody)
	if !ok {
		// try to recover the request id from the raw text, else null
		var id any
		if m := mcpIDRe.FindSubmatch(rawBody); m != nil {
			var v any
			if vv, ok2 := parseJSONBody(m[1]); ok2 {
				v = vv
			}
			id = v
		}
		return &mcpResult{status: 400, body: rpcError(id, -32700, "Parse error")}
	}
	if arr, isArr := parsed.([]any); isArr {
		results := make([]any, 0, len(arr))
		for _, item := range arr {
			outcome := handleOneMCP(store, item)
			if outcome != nil && outcome.body != nil {
				results = append(results, outcome.body)
			}
		}
		return &mcpResult{status: 200, body: results}
	}
	outcome := handleOneMCP(store, parsed)
	if outcome == nil {
		return &mcpResult{status: 202, body: nil}
	}
	return outcome
}
