// rest.go — the GitHub REST v3 surface. Handlers are pure functions over the
// Store: they take the decoded path segments, the query and the parsed JSON
// body, and return {status, body, headers}. The HTTP server owns auth, body
// parsing and serialization.

package main

import (
	"encoding/base64"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

type RestResult struct {
	Status  int
	Body    any
	Headers map[string]string
}

func restOK(body any, status int, headers map[string]string) RestResult {
	return RestResult{Status: status, Body: body, Headers: headers}
}

func restErr(status int, message string) RestResult {
	return RestResult{Status: status, Body: map[string]any{"message": message}}
}

func repoJSON(repo *Repo) map[string]any {
	return map[string]any{
		"id":             repo.ID,
		"node_id":        globalID("R", repo.ID),
		"name":           repo.Name,
		"full_name":      repo.Owner + "/" + repo.Name,
		"private":        repo.Priv,
		"owner":          map[string]any{"login": repo.Owner, "id": ViewerID, "type": "User"},
		"description":    repo.Description,
		"default_branch": repo.DefaultBranch,
		"created_at":     isoTime(repo.Seq),
		"updated_at":     isoTime(repo.Seq),
		"pushed_at":      isoTime(repo.Seq),
		"html_url":       "https://github.com/" + repo.Owner + "/" + repo.Name,
	}
}

func labelJSON(label *Label) map[string]any {
	return map[string]any{
		"id": label.ID, "node_id": globalID("L", label.ID), "name": label.Name,
		"color": label.Color, "description": label.Description,
	}
}

func issueJSON(store *Store, repo *Repo, issue *Issue) map[string]any {
	labels := make([]any, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		if label, ok := repo.Labels[name]; ok {
			labels = append(labels, labelJSON(label))
		} else {
			labels = append(labels, map[string]any{"name": name})
		}
	}
	return map[string]any{
		"id":         issue.ID,
		"node_id":    globalID("I", repo.ID, issue.Number),
		"number":     issue.Number,
		"title":      issue.Title,
		"body":       issue.Body,
		"state":      issue.State,
		"labels":     labels,
		"user":       map[string]any{"login": ViewerLogin, "id": ViewerID, "type": "User"},
		"created_at": isoTime(issue.Seq),
		"updated_at": isoTime(issue.Seq),
		"comments":   len(issue.Comments),
		"html_url":   "https://github.com/" + repo.Owner + "/" + repo.Name + "/issues/" + itoa(issue.Number),
	}
}

// writeJSONString appends a JSON-quoted string to b (escapes ", \, and
// control characters; leaves UTF-8 bytes as-is, like encoding/json).
func writeJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < 0x20 {
				const hex = "0123456789abcdef"
				b.WriteString(`\u00`)
				b.WriteByte(hex[c>>4])
				b.WriteByte(hex[c&0xf])
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
}

// issueJSONBytes serializes one issue byte-identically to issueJSON +
// marshalJSON (keys in encoding/json's sorted order), but without the map
// and the reflection — the create-issue hot path (perf workload "write")
// answers from this.
func issueJSONBytes(store *Store, repo *Repo, issue *Issue) []byte {
	var b strings.Builder
	b.Grow(512)
	b.WriteString(`{"body":`)
	writeJSONString(&b, issue.Body)
	b.WriteString(`,"comments":`)
	b.WriteString(strconv.Itoa(len(issue.Comments)))
	b.WriteString(`,"created_at":`)
	writeJSONString(&b, isoTime(issue.Seq))
	b.WriteString(`,"html_url":`)
	writeJSONString(&b, "https://github.com/"+repo.Owner+"/"+repo.Name+"/issues/"+itoa(issue.Number))
	b.WriteString(`,"id":`)
	b.WriteString(strconv.Itoa(issue.ID))
	b.WriteString(`,"labels":[`)
	for i, name := range issue.Labels {
		if i > 0 {
			b.WriteByte(',')
		}
		if label, ok := repo.Labels[name]; ok {
			b.WriteString(`{"color":`)
			writeJSONString(&b, label.Color)
			b.WriteString(`,"description":`)
			writeJSONString(&b, label.Description)
			b.WriteString(`,"id":`)
			b.WriteString(strconv.Itoa(label.ID))
			b.WriteString(`,"name":`)
			writeJSONString(&b, label.Name)
			b.WriteString(`,"node_id":`)
			writeJSONString(&b, globalID("L", label.ID))
			b.WriteByte('}')
		} else {
			b.WriteString(`{"name":`)
			writeJSONString(&b, name)
			b.WriteByte('}')
		}
	}
	b.WriteString(`],"node_id":`)
	writeJSONString(&b, globalID("I", repo.ID, issue.Number))
	b.WriteString(`,"number":`)
	b.WriteString(strconv.Itoa(issue.Number))
	b.WriteString(`,"state":`)
	writeJSONString(&b, issue.State)
	b.WriteString(`,"title":`)
	writeJSONString(&b, issue.Title)
	b.WriteString(`,"updated_at":`)
	writeJSONString(&b, isoTime(issue.Seq))
	b.WriteString(`,"user":{"id":`)
	b.WriteString(strconv.Itoa(ViewerID))
	b.WriteString(`,"login":`)
	writeJSONString(&b, ViewerLogin)
	b.WriteString(`,"type":"User"}}`)
	return []byte(b.String())
}

// issueJSONCached returns the serialized issue JSON, from the per-issue cache
// when valid. The cache is invalidated whenever the issue changes (PATCH,
// new comment).
func issueJSONCached(store *Store, repo *Repo, issue *Issue) []byte {
	store.cacheMu.Lock()
	cached := issue.cached
	store.cacheMu.Unlock()
	if cached != nil {
		return cached
	}
	body := issueJSONBytes(store, repo, issue)
	store.cacheMu.Lock()
	issue.cached = body
	store.cacheMu.Unlock()
	return body
}

func commentJSON(repo *Repo, issue *Issue, comment *Comment) map[string]any {
	return map[string]any{
		"id":         comment.ID,
		"node_id":    globalID("C", repo.ID, issue.Number, comment.ID),
		"body":       comment.Body,
		"user":       map[string]any{"login": ViewerLogin, "id": ViewerID, "type": "User"},
		"created_at": isoTime(comment.Seq),
		"updated_at": isoTime(comment.Seq),
	}
}

func pullJSON(repo *Repo, pull *Pull) map[string]any {
	headSha := pull.HeadSha
	if v, ok := repo.Refs["refs/heads/"+pull.HeadRef]; ok {
		headSha = v
	}
	baseSha := pull.BaseSha
	if v, ok := repo.Refs["refs/heads/"+pull.BaseRef]; ok {
		baseSha = v
	}
	mergedAt := any(nil)
	if pull.MergedAt != "" {
		mergedAt = pull.MergedAt
	}
	mergeSha := any(nil)
	if pull.MergeCommitSha != "" {
		mergeSha = pull.MergeCommitSha
	}
	return map[string]any{
		"id":      pull.ID,
		"node_id": globalID("P", repo.ID, pull.Number),
		"number":  pull.Number,
		"state":   pull.State,
		"title":   pull.Title,
		"body":    pull.Body,
		"head": map[string]any{
			"ref":   pull.HeadRef,
			"sha":   headSha,
			"label": repo.Owner + ":" + pull.HeadRef,
			"user":  map[string]any{"login": ViewerLogin, "id": ViewerID, "type": "User"},
		},
		"base": map[string]any{
			"ref":   pull.BaseRef,
			"sha":   baseSha,
			"label": repo.Owner + ":" + pull.BaseRef,
			"user":  map[string]any{"login": ViewerLogin, "id": ViewerID, "type": "User"},
		},
		"merged":          pull.Merged,
		"merged_at":       mergedAt,
		"merge_commit_sha": mergeSha,
		"created_at":      isoTime(pull.Seq),
		"updated_at":      isoTime(pull.Seq),
		"html_url":        "https://github.com/" + repo.Owner + "/" + repo.Name + "/pull/" + itoa(pull.Number),
	}
}

// pageParams parses pagination: per_page (default 30, 1..100) and page
// (default 1).
func pageParams(query map[string]string) (perPage, page int) {
	perPage = 30
	if raw, ok := query["per_page"]; ok {
		if n, err := strconv.Atoi(raw); err == nil {
			perPage = n
			if perPage < 1 {
				perPage = 1
			}
			if perPage > 100 {
				perPage = 100
			}
		}
	}
	page = 1
	if raw, ok := query["page"]; ok {
		if n, err := strconv.Atoi(raw); err == nil && n > 1 {
			page = n
		}
	}
	return perPage, page
}

// paginate slices a list and builds the GitHub Link header for the next page.
func paginate(items []any, query map[string]string, baseURL string) ([]any, map[string]string) {
	perPage, page := pageParams(query)
	start := (page - 1) * perPage
	end := start + perPage
	if start > len(items) {
		start = len(items)
	}
	if end > len(items) {
		end = len(items)
	}
	sliced := items[start:end]
	headers := map[string]string{}
	if start+perPage < len(items) {
		lastPage := (len(items) + perPage - 1) / perPage
		next := baseURL + "?per_page=" + itoa(perPage) + "&page=" + itoa(page+1)
		last := baseURL + "?per_page=" + itoa(perPage) + "&page=" + itoa(lastPage)
		headers["link"] = "<" + next + ">; rel=\"next\", <" + last + ">; rel=\"last\""
	}
	return sliced, headers
}

// resolveCommit resolves a ref name or oid to a commit oid, or "".
func resolveCommit(store *Store, repo *Repo, ref string, hasRef bool) string {
	if !hasRef {
		return repo.Refs["refs/heads/"+repo.DefaultBranch]
	}
	if IsOID(ref) {
		if _, ok := repo.Objects[ref]; ok {
			return ref
		}
		return ""
	}
	return repo.Refs["refs/heads/"+ref]
}

// lookupPath walks a tree to a path: the object and its type, or nil when
// absent.
type pathLookup struct {
	typ  string // "blob" | "tree"
	oid  string
	mode string
}

func lookupPath(store *Store, repo *Repo, treeOID string, segments []string) *pathLookup {
	current := treeOID
	for i, seg := range segments {
		entries, ok := store.TreeEntries(repo, current)
		if !ok {
			return nil
		}
		var found *TreeEntry
		for j := range entries {
			if entries[j].Name == seg {
				found = &entries[j]
				break
			}
		}
		if found == nil {
			return nil
		}
		if i == len(segments)-1 {
			if found.Mode == "40000" {
				return &pathLookup{typ: "tree", oid: found.OID, mode: found.Mode}
			}
			return &pathLookup{typ: "blob", oid: found.OID, mode: found.Mode}
		}
		if found.Mode != "40000" {
			return nil
		}
		current = found.OID
	}
	return nil
}

type ident struct {
	name  string
	email string
	date  string
}

func parseIdent(line string) ident {
	// /^(.*) <(.*)> (\d+) [+-]\d{4}$/
	lt := strings.Index(line, " <")
	gt := strings.LastIndex(line, "> ")
	if lt < 0 || gt < 0 || gt <= lt {
		return ident{name: line, date: isoTime(0)}
	}
	rest := line[gt+2:]
	parts := strings.Split(rest, " ")
	if len(parts) != 2 || len(parts[1]) != 5 {
		return ident{name: line, date: isoTime(0)}
	}
	ts, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return ident{name: line, date: isoTime(0)}
	}
	// validate the zone: [+-]dddd
	z := parts[1]
	if z[0] != '+' && z[0] != '-' {
		return ident{name: line, date: isoTime(0)}
	}
	for _, c := range z[1:] {
		if c < '0' || c > '9' {
			return ident{name: line, date: isoTime(0)}
		}
	}
	return ident{name: line[:lt], email: line[lt+2 : gt], date: isoTime(ts - EpochSec)}
}

type contentsResult struct {
	status     int
	commitSha  string
	contentSha string
	created    bool
	message    string
}

// contentsCommit writes (PUT) or removes (DELETE, newContent nil) one file on
// a branch and commits it. Returns the commit and blob ids, or an error
// status.
func contentsCommit(store *Store, repo *Repo, branch *string, path, message string, newContent []byte, hasContent bool, expectedSha string, hasSha bool) contentsResult {
	branchName := repo.DefaultBranch
	if branch != nil {
		branchName = *branch
	}
	tip, hasTip := repo.Refs["refs/heads/"+branchName]
	if !hasTip && branchName != repo.DefaultBranch {
		// a new branch starts at the default branch's tip (or nothing when unborn)
		tip, hasTip = repo.Refs["refs/heads/"+repo.DefaultBranch]
	}
	flat := map[string]flatEntry{}
	if hasTip {
		info, ok := store.CommitInfo(repo, tip)
		if !ok {
			return contentsResult{status: 409, message: "the branch tip is not a commit"}
		}
		flat = store.FlattenTree(repo, info.Tree)
	}
	existing, hasExisting := flat[path]
	var contentSha string
	created := true
	if hasContent {
		contentSha = store.PutObject(repo, ObjBlob, newContent)
		if hasExisting {
			created = false
			if hasSha && !strings.EqualFold(expectedSha, existing.OID) {
				return contentsResult{status: 409, message: "the file has changed since the given sha"}
			}
		}
		flat[path] = flatEntry{OID: contentSha, Mode: "100644"}
	} else {
		if !hasExisting {
			return contentsResult{status: 404, message: "Not Found"}
		}
		if hasSha && !strings.EqualFold(expectedSha, existing.OID) {
			return contentsResult{status: 409, message: "the file has changed since the given sha"}
		}
		delete(flat, path)
	}
	tree := store.BuildTreeFromFlat(repo, flat)
	var parents []string
	if hasTip {
		parents = []string{tip}
	}
	commitSha := store.CreateCommit(repo, tree, parents, message)
	repo.Refs["refs/heads/"+branchName] = commitSha
	return contentsResult{status: 200, commitSha: commitSha, contentSha: contentSha, created: created}
}

// ---------------------------------------------------------------------------
// the dispatcher
// ---------------------------------------------------------------------------

func handleRest(store *Store, method string, segments []string, query map[string]string, body any, host string) RestResult {
	base := "http://" + host
	first := ""
	if len(segments) > 0 {
		first = segments[0]
	}

	// GET /api/v3/ — the API root.
	if len(segments) == 0 || (len(segments) == 1 && first == "") {
		if method != "GET" {
			return restErr(404, "Not Found")
		}
		return restOK(map[string]any{
			"current_user_url":      base + "/api/v3/user",
			"repository_url":        base + "/api/v3/repos/{owner}/{repo}",
			"user_repositories_url": base + "/api/v3/user/repos",
		}, 200, map[string]string{"x-oauth-scopes": "repo, read:org"})
	}

	if first == "meta" {
		if method != "GET" {
			return restErr(404, "Not Found")
		}
		return restOK(map[string]any{"installed_version": "1.0.0"}, 200, nil)
	}

	if first == "user" {
		if len(segments) == 1 {
			if method != "GET" {
				return restErr(404, "Not Found")
			}
			return restOK(map[string]any{
				"login":   ViewerLogin,
				"id":      ViewerID,
				"node_id": globalID("U", ViewerID),
				"type":    "User",
				"name":    ViewerLogin,
				"email":   ViewerEmail,
			}, 200, nil)
		}
		if len(segments) > 1 && segments[1] == "repos" {
			if method == "POST" {
				rec, ok := body.(map[string]any)
				if !ok {
					return restErr(422, "a repository name is required")
				}
				name, _ := rec["name"].(string)
				if name == "" || !validRepoName(name) {
					return restErr(422, "a valid repository name is required")
				}
				priv, _ := rec["private"].(bool)
				description, _ := rec["description"].(string)
				repo := store.CreateRepo(name, priv, description)
				if repo == nil {
					return restErr(422, "Repository already exists")
				}
				return restOK(repoJSON(repo), 201, nil)
			}
			if method == "GET" {
				list := make([]any, 0)
				for _, repo := range store.ListRepos() {
					list = append(list, repoJSON(repo))
				}
				page, headers := paginate(list, query, base+"/api/v3/user/repos")
				return restOK(page, 200, headers)
			}
			return restErr(404, "Not Found")
		}
		return restErr(404, "Not Found")
	}

	if first != "repos" {
		return restErr(404, "Not Found")
	}

	owner := ""
	name := ""
	if len(segments) > 1 {
		owner = segments[1]
	}
	if len(segments) > 2 {
		name = segments[2]
	}
	if owner == "" || name == "" {
		return restErr(404, "Not Found")
	}
	repo := store.GetRepo(owner, name)
	rest := segments[3:]

	// GET / PATCH / DELETE /api/v3/repos/{owner}/{repo}
	if len(rest) == 0 {
		if repo == nil {
			return restErr(404, "Not Found")
		}
		if method == "GET" {
			return restOK(repoJSON(repo), 200, nil)
		}
		if method == "PATCH" {
			rec, ok := body.(map[string]any)
			if !ok {
				return restErr(422, "a JSON object body is required")
			}
			if description, ok := rec["description"].(string); ok {
				repo.Description = description
			}
			if priv, ok := rec["private"].(bool); ok {
				repo.Priv = priv
			}
			repo.cached = nil
			return restOK(repoJSON(repo), 200, nil)
		}
		if method == "DELETE" {
			store.DeleteRepo(repo)
			return RestResult{Status: 204, Body: nil}
		}
		return restErr(404, "Not Found")
	}

	section := rest[0]

	if section == "branches" {
		if repo == nil {
			return restErr(404, "Not Found")
		}
		if len(rest) == 1 {
			if method != "GET" {
				return restErr(404, "Not Found")
			}
			type branchOut struct {
				name string
				oid  string
			}
			var branches []branchOut
			for refName, oid := range repo.Refs {
				if strings.HasPrefix(refName, "refs/heads/") {
					branches = append(branches, branchOut{refName[len("refs/heads/"):], oid})
				}
			}
			sort.Slice(branches, func(i, j int) bool { return branches[i].name < branches[j].name })
			out := make([]any, 0, len(branches))
			for _, b := range branches {
				out = append(out, map[string]any{"name": b.name, "commit": map[string]any{"sha": b.oid}})
			}
			return restOK(out, 200, nil)
		}
		branch := strings.Join(rest[1:], "/")
		oid, ok := repo.Refs["refs/heads/"+branch]
		if !ok {
			return restErr(404, "Not Found")
		}
		if method != "GET" {
			return restErr(404, "Not Found")
		}
		return restOK(map[string]any{"name": branch, "commit": map[string]any{"sha": oid}}, 200, nil)
	}

	if section == "git" && len(rest) > 1 && rest[1] == "refs" {
		if repo == nil {
			return restErr(404, "Not Found")
		}
		if method == "POST" && len(rest) == 2 {
			rec, ok := body.(map[string]any)
			if !ok {
				return restErr(422, "a ref and sha are required")
			}
			ref, _ := rec["ref"].(string)
			sha, _ := rec["sha"].(string)
			if ref == "" || !strings.HasPrefix(ref, "refs/") || sha == "" || !IsOID(sha) {
				return restErr(422, "a ref under refs/ and a 40-hex sha are required")
			}
			if _, exists := repo.Refs[ref]; exists {
				return restErr(422, "Reference already exists")
			}
			object, ok := repo.Objects[sha]
			if !ok {
				return restErr(422, "the sha names no object in this repository")
			}
			repo.Refs[ref] = sha
			return restOK(map[string]any{"ref": ref, "object": map[string]any{"sha": sha, "type": string(object.Type)}}, 201, nil)
		}
		if method == "GET" {
			if len(rest) == 2 {
				names := make([]string, 0, len(repo.Refs))
				for n := range repo.Refs {
					names = append(names, n)
				}
				sort.Strings(names)
				out := make([]any, 0, len(names))
				for _, refName := range names {
					oid := repo.Refs[refName]
					typ := "commit"
					if o, ok := repo.Objects[oid]; ok {
						typ = string(o.Type)
					}
					out = append(out, map[string]any{"ref": refName, "object": map[string]any{"sha": oid, "type": typ}})
				}
				return restOK(out, 200, nil)
			}
			refName := "refs/" + strings.Join(rest[2:], "/")
			oid, ok := repo.Refs[refName]
			if !ok {
				return restErr(404, "Not Found")
			}
			typ := "commit"
			if o, ok := repo.Objects[oid]; ok {
				typ = string(o.Type)
			}
			return restOK(map[string]any{"ref": refName, "object": map[string]any{"sha": oid, "type": typ}}, 200, nil)
		}
		return restErr(404, "Not Found")
	}

	if section == "commits" {
		if repo == nil {
			return restErr(404, "Not Found")
		}
		if method != "GET" {
			return restErr(404, "Not Found")
		}
		sha, hasSha := query["sha"]
		tip := resolveCommit(store, repo, sha, hasSha)
		out := make([]any, 0)
		current := tip
		seen := map[string]bool{}
		for current != "" && !seen[current] && len(out) < 100 {
			seen[current] = true
			info, ok := store.CommitInfo(repo, current)
			if !ok {
				break
			}
			author := parseIdent(info.Author)
			committer := parseIdent(info.Committer)
			parents := make([]any, 0, len(info.Parents))
			for _, p := range info.Parents {
				parents = append(parents, map[string]any{"sha": p})
			}
			out = append(out, map[string]any{
				"sha": current,
				"commit": map[string]any{
					"message": info.Message,
					"author":  map[string]any{"name": author.name, "email": author.email, "date": author.date},
					"committer": map[string]any{"name": committer.name, "email": committer.email, "date": committer.date},
					"tree":    map[string]any{"sha": info.Tree},
				},
				"parents": parents,
			})
			if len(info.Parents) > 0 {
				current = info.Parents[0]
			} else {
				current = ""
			}
		}
		return restOK(out, 200, nil)
	}

	if section == "contents" {
		if repo == nil {
			return restErr(404, "Not Found")
		}
		path := strings.Join(rest[1:], "/")
		if method == "GET" {
			ref, hasRef := query["ref"]
			tip := resolveCommit(store, repo, ref, hasRef)
			if tip == "" {
				return restErr(404, "Not Found")
			}
			info, ok := store.CommitInfo(repo, tip)
			if !ok {
				return restErr(404, "Not Found")
			}
			if path == "" {
				entries, ok := store.TreeEntries(repo, info.Tree)
				if !ok {
					return restErr(404, "Not Found")
				}
				return restOK(dirListing(store, repo, entries, ""), 200, nil)
			}
			segments := strings.Split(path, "/")
			found := lookupPath(store, repo, info.Tree, segments)
			if found == nil {
				return restErr(404, "Not Found")
			}
			if found.typ == "tree" {
				entries, ok := store.TreeEntries(repo, found.oid)
				if !ok {
					return restErr(404, "Not Found")
				}
				return restOK(dirListing(store, repo, entries, path), 200, nil)
			}
			object, ok := repo.Objects[found.oid]
			if !ok {
				return restErr(404, "Not Found")
			}
			return restOK(map[string]any{
				"type":     "file",
				"encoding": "base64",
				"size":     len(object.Data),
				"name":     segments[len(segments)-1],
				"path":     path,
				"content":  base64.StdEncoding.EncodeToString(object.Data),
				"sha":      found.oid,
			}, 200, nil)
		}
		if method == "PUT" {
			rec, ok := body.(map[string]any)
			if !ok {
				return restErr(422, "a JSON object body is required")
			}
			content, contentOK := rec["content"].(string)
			message, messageOK := rec["message"].(string)
			if !contentOK || !messageOK {
				return restErr(422, "message and base64 content are required")
			}
			var branch *string
			if b, ok := rec["branch"].(string); ok {
				branch = &b
			}
			sha, _ := rec["sha"].(string)
			_, hasSha := rec["sha"].(string)
			bytes := decodeBase64Loose(content)
			result := contentsCommit(store, repo, branch, path, message, bytes, true, sha, hasSha)
			if result.status == 409 || result.status == 404 {
				return restErr(result.status, result.message)
			}
			status := 200
			if result.created {
				status = 201
			}
			return restOK(map[string]any{
				"content": map[string]any{
					"name":     lastPathSegment(path),
					"path":     path,
					"sha":      result.contentSha,
					"size":     len(bytes),
					"type":     "file",
					"encoding": "base64",
				},
				"commit": map[string]any{"sha": result.commitSha, "message": message},
			}, status, nil)
		}
		if method == "DELETE" {
			rec, ok := body.(map[string]any)
			if !ok {
				return restErr(422, "a JSON object body is required")
			}
			message, _ := rec["message"].(string)
			var branch *string
			if b, ok := rec["branch"].(string); ok {
				branch = &b
			}
			sha, _ := rec["sha"].(string)
			_, hasSha := rec["sha"].(string)
			result := contentsCommit(store, repo, branch, path, message, nil, false, sha, hasSha)
			if result.status == 409 || result.status == 404 {
				return restErr(result.status, result.message)
			}
			return restOK(map[string]any{"commit": map[string]any{"sha": result.commitSha, "message": message}}, 200, nil)
		}
		return restErr(404, "Not Found")
	}

	if section == "labels" {
		if repo == nil {
			return restErr(404, "Not Found")
		}
		if method != "POST" {
			return restErr(404, "Not Found")
		}
		rec, ok := body.(map[string]any)
		if !ok {
			return restErr(422, "a label name and color are required")
		}
		name, _ := rec["name"].(string)
		color, _ := rec["color"].(string)
		if color == "" {
			color = "ededed"
		}
		if name == "" {
			return restErr(422, "a label name is required")
		}
		label := store.CreateLabel(repo, name, color)
		if label == nil {
			return restErr(422, "Label already exists")
		}
		return restOK(labelJSON(label), 201, nil)
	}

	if section == "issues" {
		if repo == nil {
			return restErr(404, "Not Found")
		}
		if len(rest) == 1 {
			if method == "POST" {
				rec, ok := body.(map[string]any)
				if !ok {
					return restErr(422, "a title is required")
				}
				title, _ := rec["title"].(string)
				if title == "" {
					return restErr(422, "a title is required")
				}
				bodyText, _ := rec["body"].(string)
				var labels []string
				if arr, ok := rec["labels"].([]any); ok {
					for _, item := range arr {
						if s, ok := item.(string); ok {
							labels = append(labels, s)
						}
					}
				}
				for _, label := range labels {
					if _, ok := repo.Labels[label]; !ok {
						return restErr(422, "Label does not exist: "+label)
					}
				}
				issue := store.CreateIssue(repo, title, bodyText, labels)
				return restOK(rawJSON(issueJSONBytes(store, repo, issue)), 201, nil)
			}
			if method == "GET" {
				state := query["state"]
				if state == "" {
					state = "open"
				}
				direction := query["direction"]
				if direction == "" {
					direction = "desc"
				}
				sortBy := query["sort"]
				if sortBy == "" {
					sortBy = "created"
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
				if sortBy == "created" {
					if direction == "asc" {
						sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
					} else {
						sort.Slice(list, func(i, j int) bool { return list[i].Seq > list[j].Seq })
					}
				}
				items := make([]any, 0, len(list))
				for _, issue := range list {
					items = append(items, issueJSON(store, repo, issue))
				}
				page, headers := paginate(items, query, base+"/api/v3/repos/"+owner+"/"+name+"/issues")
				return restOK(page, 200, headers)
			}
			return restErr(404, "Not Found")
		}
		number, err := strconv.Atoi(rest[1])
		if err != nil || number < 1 {
			return restErr(404, "Not Found")
		}
		issue := repo.Issues[number]
		if len(rest) == 2 {
			if issue == nil {
				return restErr(404, "Not Found")
			}
			if method == "GET" {
				return RestResult{Status: 200, Body: rawJSON(issueJSONCached(store, repo, issue))}
			}
			if method == "PATCH" {
				rec, ok := body.(map[string]any)
				if !ok {
					return restErr(422, "a JSON object body is required")
				}
				if state, ok := rec["state"].(string); ok && (state == "open" || state == "closed") {
					issue.State = state
				}
				if title, ok := rec["title"].(string); ok {
					issue.Title = title
				}
				if bodyText, ok := rec["body"].(string); ok {
					issue.Body = bodyText
				}
				store.cacheMu.Lock()
				issue.cached = nil
				store.cacheMu.Unlock()
				return restOK(issueJSON(store, repo, issue), 200, nil)
			}
			return restErr(404, "Not Found")
		}
		if rest[2] == "comments" {
			if issue == nil {
				return restErr(404, "Not Found")
			}
			if method == "POST" {
				rec, ok := body.(map[string]any)
				if !ok {
					return restErr(422, "a comment body is required")
				}
				text, ok := rec["body"].(string)
				if !ok {
					return restErr(422, "a comment body is required")
				}
				comment := store.CreateComment(repo, issue, text)
				return restOK(commentJSON(repo, issue, comment), 201, nil)
			}
			if method == "GET" {
				out := make([]any, 0, len(issue.Comments))
				for _, c := range issue.Comments {
					out = append(out, commentJSON(repo, issue, c))
				}
				return restOK(out, 200, nil)
			}
			return restErr(404, "Not Found")
		}
		return restErr(404, "Not Found")
	}

	if section == "pulls" {
		if repo == nil {
			return restErr(404, "Not Found")
		}
		if len(rest) == 1 {
			if method == "POST" {
				rec, ok := body.(map[string]any)
				if !ok {
					return restErr(422, "head, base and title are required")
				}
				head, _ := rec["head"].(string)
				baseBranch, _ := rec["base"].(string)
				title, _ := rec["title"].(string)
				if head == "" || baseBranch == "" || title == "" {
					return restErr(422, "head, base and title are required")
				}
				bodyText, _ := rec["body"].(string)
				headSha, hasHead := repo.Refs["refs/heads/"+head]
				baseSha, hasBase := repo.Refs["refs/heads/"+baseBranch]
				if !hasHead || !hasBase {
					return restErr(422, "head and base must name existing branches")
				}
				for _, existing := range repo.Pulls {
					if existing.State == "open" && existing.HeadRef == head && existing.BaseRef == baseBranch {
						return restErr(422, "A pull request already exists for this head and base")
					}
				}
				pull := store.CreatePull(repo, head, baseBranch, headSha, baseSha, title, bodyText)
				return restOK(pullJSON(repo, pull), 201, nil)
			}
			if method == "GET" {
				state := query["state"]
				if state == "" {
					state = "open"
				}
				var list []*Pull
				for _, pull := range repo.Pulls {
					list = append(list, pull)
				}
				filtered := list[:0]
				for _, pull := range list {
					if state == "open" && pull.State != "open" {
						continue
					}
					if state == "closed" && pull.State != "closed" {
						continue
					}
					filtered = append(filtered, pull)
				}
				list = filtered
				sort.Slice(list, func(i, j int) bool { return list[i].Seq > list[j].Seq })
				items := make([]any, 0, len(list))
				for _, pull := range list {
					items = append(items, pullJSON(repo, pull))
				}
				page, headers := paginate(items, query, base+"/api/v3/repos/"+owner+"/"+name+"/pulls")
				return restOK(page, 200, headers)
			}
			return restErr(404, "Not Found")
		}
		number, err := strconv.Atoi(rest[1])
		if err != nil || number < 1 {
			return restErr(404, "Not Found")
		}
		pull := repo.Pulls[number]
		if len(rest) == 2 {
			if pull == nil {
				return restErr(404, "Not Found")
			}
			if method != "GET" {
				return restErr(404, "Not Found")
			}
			return restOK(pullJSON(repo, pull), 200, nil)
		}
		if rest[2] == "files" {
			if pull == nil {
				return restErr(404, "Not Found")
			}
			if method != "GET" {
				return restErr(404, "Not Found")
			}
			files := store.PullFiles(repo, pull)
			out := make([]any, 0, len(files))
			for _, f := range files {
				out = append(out, map[string]any{
					"filename": f.Filename, "status": f.Status,
					"additions": f.Additions, "deletions": f.Deletions, "changes": f.Changes,
				})
			}
			return restOK(out, 200, nil)
		}
		if rest[2] == "merge" {
			if pull == nil {
				return restErr(404, "Not Found")
			}
			if method != "PUT" {
				return restErr(404, "Not Found")
			}
			if pull.State != "open" {
				return restErr(405, "Pull request is already closed")
			}
			merged := store.SquashMerge(repo, pull)
			if !merged.OK {
				if merged.Reason == "conflict" {
					return restErr(405, "Merge conflict")
				}
				return restErr(405, "Pull request cannot be merged")
			}
			return restOK(map[string]any{"sha": merged.Sha, "merged": true, "message": "Pull request successfully merged"}, 200, nil)
		}
		return restErr(404, "Not Found")
	}

	return restErr(404, "Not Found")
}

func validRepoName(name string) bool {
	if len(name) == 0 || len(name) > 100 {
		return false
	}
	for i, c := range name {
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			continue
		}
		if i > 0 && (c == '.' || c == '_' || c == '-') {
			continue
		}
		return false
	}
	return true
}

func dirListing(store *Store, repo *Repo, entries []TreeEntry, prefix string) []any {
	out := make([]any, 0, len(entries))
	for _, e := range entries {
		typ := "file"
		if e.Mode == "40000" {
			typ = "dir"
		}
		size := 0
		if typ == "file" {
			if o, ok := repo.Objects[e.OID]; ok {
				size = len(o.Data)
			}
		}
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + e.Name
		}
		out = append(out, map[string]any{
			"type": typ, "name": e.Name, "path": path, "sha": e.OID, "size": size,
		})
	}
	return out
}

func lastPathSegment(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// decodeBase64Loose decodes base64 after stripping whitespace, tolerating
// stray characters the way Buffer.from does.
func decodeBase64Loose(content string) []byte {
	filtered := make([]byte, 0, len(content))
	for i := 0; i < len(content); i++ {
		c := content[i]
		if c == '\r' || c == '\n' || c == '\t' || c == ' ' {
			continue
		}
		filtered = append(filtered, c)
	}
	out, err := base64.StdEncoding.DecodeString(string(filtered))
	if err != nil {
		// tolerate a missing padding or stray characters: strip non-alphabet
		// bytes and retry with padding added.
		clean := make([]byte, 0, len(filtered))
		for _, c := range filtered {
			if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=' {
				clean = append(clean, c)
			}
		}
		for len(clean)%4 != 0 {
			clean = append(clean, '=')
		}
		out, _ = base64.StdEncoding.DecodeString(string(clean))
	}
	return out
}

// rawJSON wraps pre-serialized JSON bytes so the responder writes them as-is.
// It is an alias of json.RawMessage so encoding/json embeds it raw.
type rawJSON = json.RawMessage
