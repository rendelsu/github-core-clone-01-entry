// store.go — the forge's whole state, in memory. One process holds one store;
// every trial starts a fresh process, so nothing here is ever persisted or
// read back from disk. Ids are sequential counters and timestamps come from a
// logical clock (a fixed epoch plus a sequence), so two runs that see the same
// requests build byte-identical state.
//
// Concurrency: the HTTP server serves requests on many goroutines. All store
// state is guarded by mu (RWMutex); read handlers take a read lock, mutating
// handlers take the write lock. Per-object caches (deflate/commit/tree) are
// guarded by their own mutex because reads populate them.

package main

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The token owner's login: the scenario's fixed viewer (see wire.ts buildScenario).
const (
	ViewerLogin   = "adaiversen"
	ViewerID      = 1000
	ViewerEmail   = "adaiversen@example.invalid"
	ViewerIdent   = ViewerLogin + " <" + ViewerEmail + ">"
	StrangerLogin = "olgabauer"
	DefaultBranch = "main"
)

// The fixed epoch every generated timestamp derives from (2025-01-01T00:00:00Z).
const EpochSec = 1735689600

// The fixed epoch commit objects created through the API carry.
const CommitEpochSec = 1700000000

func isoTime(seq int64) string {
	return time.Unix(EpochSec+seq, 0).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

type Label struct {
	ID          int
	Name        string
	Color       string
	Description string
}

type Comment struct {
	ID   int
	Body string
	Seq  int64
}

type Issue struct {
	Number   int
	ID       int
	Title    string
	Body     string
	State    string // "open" | "closed"
	Labels   []string
	Comments []*Comment
	Seq      int64
	// cached is the serialized REST JSON for this issue, or nil when stale.
	// It is only written while holding the store write lock.
	cached []byte
}

type Pull struct {
	Number        int
	ID            int
	Title         string
	Body          string
	State         string // "open" | "closed"
	Merged        bool
	MergedAt      string // "" means null
	MergeCommitSha string // "" means null
	HeadRef       string
	BaseRef       string
	HeadSha       string
	BaseSha       string
	Seq           int64
}

type Repo struct {
	ID            int
	Name          string
	Owner         string
	Priv          bool
	Description   string
	DefaultBranch string
	Seq           int64
	// ref name -> commit/tag oid.
	Refs map[string]string
	// oid -> object.
	Objects map[string]GitObject
	// oid -> cached zlib stream of the object's bytes (objects never change).
	deflateCache map[string][]byte
	// oid -> parsed commit (cache; commits never change).
	commitCache map[string]CommitInfo
	// oid -> parsed tree entries (cache; trees never change).
	treeCache map[string][]TreeEntry
	Labels    map[string]*Label
	LabelSeq  int
	Issues    map[int]*Issue
	Pulls     map[int]*Pull
	// The shared issue + pull-request counter (GitHub numbers both from one counter).
	NextNumber int
	CommentSeq int
	// Logical clock for commits this forge creates through the API.
	CommitSeq int
	// cached repo JSON, or nil when stale.
	cached []byte
}

type Store struct {
	mu        sync.RWMutex
	cacheMu   sync.Mutex
	repos     map[string]*Repo
	repoIDSeq int
	issueIDSeq int
	pullIDSeq int
	globalSeq int64
}

func NewStore() *Store {
	return &Store{repos: map[string]*Repo{}}
}

func (s *Store) repoKey(owner, name string) string { return owner + "/" + name }

func (s *Store) CreateRepo(name string, priv bool, description string) *Repo {
	key := s.repoKey(ViewerLogin, name)
	if _, ok := s.repos[key]; ok {
		return nil
	}
	s.repoIDSeq++
	s.globalSeq++
	repo := &Repo{
		ID:            s.repoIDSeq,
		Name:          name,
		Owner:         ViewerLogin,
		Priv:          priv,
		Description:   description,
		DefaultBranch: DefaultBranch,
		Seq:           s.globalSeq,
		Refs:          map[string]string{},
		Objects:       map[string]GitObject{},
		deflateCache:  map[string][]byte{},
		commitCache:   map[string]CommitInfo{},
		treeCache:     map[string][]TreeEntry{},
		Labels:        map[string]*Label{},
		Issues:        map[int]*Issue{},
		Pulls:         map[int]*Pull{},
		NextNumber:    1,
	}
	s.repos[key] = repo
	return repo
}

// GetRepo returns a repository under the viewer; any other owner is a 404
// (never a 403).
func (s *Store) GetRepo(owner, name string) *Repo {
	if owner != ViewerLogin {
		return nil
	}
	return s.repos[s.repoKey(owner, name)]
}

func (s *Store) DeleteRepo(repo *Repo) {
	delete(s.repos, s.repoKey(repo.Owner, repo.Name))
}

// ListRepos returns every repository the viewer owns, in creation order.
func (s *Store) ListRepos() []*Repo {
	out := make([]*Repo, 0, len(s.repos))
	for _, r := range s.repos {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// ReposByID returns all repositories keyed by their numeric id (for
// global-id lookups).
func (s *Store) ReposByID() map[int]*Repo {
	out := map[int]*Repo{}
	for _, r := range s.repos {
		out[r.ID] = r
	}
	return out
}

// -------------------------------------------------------------------------
// objects
// -------------------------------------------------------------------------

// PutObject stores an object (idempotent) and returns its id.
func (s *Store) PutObject(repo *Repo, typ ObjType, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := repo.Objects[oid]; !ok {
		repo.Objects[oid] = GitObject{Type: typ, Data: data}
	}
	return oid
}

func (s *Store) GetObject(repo *Repo, oid string) (GitObject, bool) {
	o, ok := repo.Objects[oid]
	return o, ok
}

// CommitInfo returns a commit's parsed headers, cached (commits are immutable).
func (s *Store) CommitInfo(repo *Repo, oid string) (CommitInfo, bool) {
	s.cacheMu.Lock()
	if cached, ok := repo.commitCache[oid]; ok {
		s.cacheMu.Unlock()
		return cached, true
	}
	s.cacheMu.Unlock()
	object, ok := repo.Objects[oid]
	if !ok || object.Type != ObjCommit {
		return CommitInfo{}, false
	}
	parsed := parseCommit(object.Data)
	s.cacheMu.Lock()
	repo.commitCache[oid] = parsed
	s.cacheMu.Unlock()
	return parsed, true
}

// TreeEntries returns a tree's entries, cached (trees are immutable).
func (s *Store) TreeEntries(repo *Repo, oid string) ([]TreeEntry, bool) {
	s.cacheMu.Lock()
	if cached, ok := repo.treeCache[oid]; ok {
		s.cacheMu.Unlock()
		return cached, true
	}
	s.cacheMu.Unlock()
	object, ok := repo.Objects[oid]
	if !ok || object.Type != ObjTree {
		return nil, false
	}
	parsed, err := parseTree(object.Data)
	if err != nil {
		return nil, false
	}
	s.cacheMu.Lock()
	repo.treeCache[oid] = parsed
	s.cacheMu.Unlock()
	return parsed, true
}

// Deflated returns a deflated copy of an object's bytes, cached (objects are
// immutable).
func (s *Store) Deflated(repo *Repo, oid string) []byte {
	s.cacheMu.Lock()
	if cached, ok := repo.deflateCache[oid]; ok {
		s.cacheMu.Unlock()
		return cached
	}
	s.cacheMu.Unlock()
	object, ok := repo.Objects[oid]
	if !ok {
		return nil
	}
	d := deflate(object.Data)
	s.cacheMu.Lock()
	repo.deflateCache[oid] = d
	s.cacheMu.Unlock()
	return d
}

// -------------------------------------------------------------------------
// reachability
// -------------------------------------------------------------------------

// Reachable returns every object reachable from roots. depth limits the
// commit generations (1 = the roots only, no parents); trees and blobs of an
// included commit are always included. A tag root includes the tag and its
// target at the same depth. Commits in cut are included with their trees,
// but their parents are not followed: that is how a shallow client's have
// lines behave, because the client holds the boundary commit but none of its
// history.
func (s *Store) Reachable(repo *Repo, roots []string, depth int, cut map[string]bool) map[string]bool {
	seen := map[string]bool{}
	type item struct {
		oid  string
		dist int
	}
	var queue []item
	for _, root := range roots {
		if IsOID(root) {
			queue = append(queue, item{root, 0})
		}
	}
	for len(queue) > 0 {
		it := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[it.oid] {
			continue
		}
		object, ok := repo.Objects[it.oid]
		if !ok {
			continue
		}
		seen[it.oid] = true
		switch object.Type {
		case ObjCommit:
			info, ok := s.CommitInfo(repo, it.oid)
			if !ok {
				continue
			}
			// the commit's own tree always travels with it, whatever the depth
			queue = append(queue, item{info.Tree, it.dist})
			if it.dist+1 >= depth {
				continue
			}
			if cut[it.oid] {
				continue
			}
			for _, parent := range info.Parents {
				queue = append(queue, item{parent, it.dist + 1})
			}
		case ObjTag:
			tag := parseTag(object.Data)
			queue = append(queue, item{tag.Object, it.dist})
		case ObjTree:
			entries, ok := s.TreeEntries(repo, it.oid)
			if !ok {
				continue
			}
			for _, e := range entries {
				if e.Mode == "160000" {
					continue // a gitlink names a commit the repo does not hold
				}
				queue = append(queue, item{e.OID, it.dist})
			}
		}
	}
	return seen
}

const infiniteDepth = int(^uint(0) >> 1)

// CommitDistances returns the commits reachable from roots at each distance
// (0 = the roots), with no depth limit. Used for shallow-boundary
// bookkeeping. maxDist caps the walk: only entries at distance <= maxDist
// are recorded. A merge commit inside the walked region makes first-discovery
// distances order-dependent past the cutoff, so in that case the full walk is
// run instead (exact either way).
func (s *Store) CommitDistances(repo *Repo, roots []string, maxDist int) map[string]int {
	run := func(limit int) (map[string]int, bool) {
		dist := map[string]int{}
		var queue []string
		sawMerge := false
		for _, root := range roots {
			_, has := dist[root]
			if !IsOID(root) || has {
				continue
			}
			object, ok := repo.Objects[root]
			if !ok {
				continue
			}
			if object.Type == ObjTag {
				tag := parseTag(object.Data)
				if _, has := dist[tag.Object]; !has {
					queue = append(queue, tag.Object)
				}
				continue
			}
			if object.Type != ObjCommit {
				continue
			}
			dist[root] = 0
			queue = append(queue, root)
		}
		for len(queue) > 0 {
			oid := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			current := dist[oid]
			if current >= limit {
				continue
			}
			info, ok := s.CommitInfo(repo, oid)
			if !ok {
				continue
			}
			if len(info.Parents) > 1 {
				sawMerge = true
			}
			for _, parent := range info.Parents {
				if _, has := dist[parent]; !has {
					if _, exists := repo.Objects[parent]; exists {
						dist[parent] = current + 1
						queue = append(queue, parent)
					}
				}
			}
		}
		return dist, sawMerge
	}
	bounded, sawMerge := run(maxDist)
	if maxDist == infiniteDepth || !sawMerge {
		return bounded
	}
	full, _ := run(infiniteDepth)
	return full
}

// IsAncestor is true when ancestor is an ancestor of descendant (or the same
// commit).
func (s *Store) IsAncestor(repo *Repo, ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := map[string]bool{}
	queue := []string{descendant}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		info, ok := s.CommitInfo(repo, oid)
		if !ok {
			continue
		}
		for _, parent := range info.Parents {
			if parent == ancestor {
				return true
			}
			queue = append(queue, parent)
		}
	}
	return false
}

// MergeBase returns the merge-base of two commits: the first commit
// reachable from both.
func (s *Store) MergeBase(repo *Repo, a, b string) (string, bool) {
	ancestorsA := map[string]bool{}
	queueA := []string{a}
	for len(queueA) > 0 {
		oid := queueA[len(queueA)-1]
		queueA = queueA[:len(queueA)-1]
		if ancestorsA[oid] {
			continue
		}
		ancestorsA[oid] = true
		info, ok := s.CommitInfo(repo, oid)
		if !ok {
			continue
		}
		queueA = append(queueA, info.Parents...)
	}
	queueB := []string{b}
	seenB := map[string]bool{}
	for len(queueB) > 0 {
		oid := queueB[len(queueB)-1]
		queueB = queueB[:len(queueB)-1]
		if seenB[oid] {
			continue
		}
		seenB[oid] = true
		if ancestorsA[oid] {
			return oid, true
		}
		info, ok := s.CommitInfo(repo, oid)
		if !ok {
			continue
		}
		queueB = append(queueB, info.Parents...)
	}
	return "", false
}

// -------------------------------------------------------------------------
// trees as flat path maps
// -------------------------------------------------------------------------

type flatEntry struct {
	OID  string
	Mode string
}

// FlattenTree flattens a tree into path -> {oid, mode}, recursing into
// directories.
func (s *Store) FlattenTree(repo *Repo, treeOID string) map[string]flatEntry {
	flat := map[string]flatEntry{}
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		entries, ok := s.TreeEntries(repo, oid)
		if !ok {
			return
		}
		for _, e := range entries {
			path := e.Name
			if prefix != "" {
				path = prefix + "/" + e.Name
			}
			if e.Mode == "40000" {
				walk(e.OID, path)
			} else {
				flat[path] = flatEntry{OID: e.OID, Mode: e.Mode}
			}
		}
	}
	walk(treeOID, "")
	return flat
}

// BuildTreeFromFlat builds (and stores) nested trees from a flat path map;
// returns the root tree id.
func (s *Store) BuildTreeFromFlat(repo *Repo, flat map[string]flatEntry) string {
	type node struct {
		files map[string]flatEntry
		dirs  map[string]*node
	}
	mk := func() *node { return &node{files: map[string]flatEntry{}, dirs: map[string]*node{}} }
	root := mk()
	for path, value := range flat {
		segments := strings.Split(path, "/")
		n := root
		for i := 0; i < len(segments)-1; i++ {
			name := segments[i]
			child, ok := n.dirs[name]
			if !ok {
				child = mk()
				n.dirs[name] = child
			}
			n = child
		}
		n.files[segments[len(segments)-1]] = value
	}
	var build func(n *node) string
	build = func(n *node) string {
		entries := []TreeEntry{}
		for name, value := range n.files {
			entries = append(entries, TreeEntry{Mode: value.Mode, Name: name, OID: value.OID})
		}
		for name, child := range n.dirs {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: build(child)})
		}
		return s.PutObject(repo, ObjTree, buildTree(entries))
	}
	return build(root)
}

// CreateCommit creates a commit object on parents with tree and stores it.
func (s *Store) CreateCommit(repo *Repo, tree string, parents []string, message string) string {
	repo.CommitSeq++
	timestamp := int64(CommitEpochSec) + int64(repo.CommitSeq)
	data := buildCommit(tree, parents, ViewerIdent, ViewerIdent, timestamp, message)
	return s.PutObject(repo, ObjCommit, data)
}

// -------------------------------------------------------------------------
// diffs and merges
// -------------------------------------------------------------------------

type diffCount struct {
	Additions int
	Deletions int
}

// LineDiff is a line diff: additions and deletions between two texts (small
// contents).
func LineDiff(oldText, newText string) diffCount {
	var oldLines, newLines []string
	if oldText != "" {
		oldLines = strings.Split(oldText, "\n")
	}
	if newText != "" {
		newLines = strings.Split(newText, "\n")
	}
	rows := len(oldLines) + 1
	cols := len(newLines) + 1
	table := make([]uint32, rows*cols)
	for i := 1; i < rows; i++ {
		for j := 1; j < cols; j++ {
			if oldLines[i-1] == newLines[j-1] {
				table[i*cols+j] = table[(i-1)*cols+(j-1)] + 1
			} else {
				a := table[(i-1)*cols+j]
				b := table[i*cols+(j-1)]
				if a > b {
					table[i*cols+j] = a
				} else {
					table[i*cols+j] = b
				}
			}
		}
	}
	common := table[len(oldLines)*cols+len(newLines)]
	return diffCount{Additions: len(newLines) - int(common), Deletions: len(oldLines) - int(common)}
}

type treeDiff struct {
	Path   string
	Status string // "added" | "removed" | "modified"
	OldOID string // "" means null
	NewOID string // "" means null
}

// DiffTrees returns the changed paths between two trees: added, removed or
// modified.
func (s *Store) DiffTrees(repo *Repo, baseTree, headTree string) []treeDiff {
	base := s.FlattenTree(repo, baseTree)
	head := s.FlattenTree(repo, headTree)
	out := []treeDiff{}
	for path, value := range head {
		old, ok := base[path]
		if !ok {
			out = append(out, treeDiff{Path: path, Status: "added", NewOID: value.OID})
		} else if old.OID != value.OID {
			out = append(out, treeDiff{Path: path, Status: "modified", OldOID: old.OID, NewOID: value.OID})
		}
	}
	for path, value := range base {
		if _, ok := head[path]; !ok {
			out = append(out, treeDiff{Path: path, Status: "removed", OldOID: value.OID})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// ThreeWayMerge is a three-way merge of headTip into baseTip over their
// merge-base. It returns the merged flat tree, or conflict=true when both
// sides changed the same path differently.
func (s *Store) ThreeWayMerge(repo *Repo, baseTip, headTip string) (map[string]flatEntry, bool) {
	baseInfo, ok1 := s.CommitInfo(repo, baseTip)
	headInfo, ok2 := s.CommitInfo(repo, headTip)
	if !ok1 || !ok2 {
		return nil, true
	}
	mergeBase, ok := s.MergeBase(repo, baseTip, headTip)
	if !ok {
		return nil, true
	}
	baseBaseInfo, ok := s.CommitInfo(repo, mergeBase)
	if !ok {
		return nil, true
	}
	baseFlat := s.FlattenTree(repo, baseInfo.Tree)
	headFlat := s.FlattenTree(repo, headInfo.Tree)
	mergeFlat := s.FlattenTree(repo, baseBaseInfo.Tree)
	result := map[string]flatEntry{}
	for k, v := range baseFlat {
		result[k] = v
	}
	paths := map[string]bool{}
	for k := range mergeFlat {
		paths[k] = true
	}
	for k := range headFlat {
		paths[k] = true
	}
	for k := range baseFlat {
		paths[k] = true
	}
	for path := range paths {
		ancestor, hasAncestor := mergeFlat[path]
		head, hasHead := headFlat[path]
		current, hasCurrent := baseFlat[path]
		ancestorOID := ""
		if hasAncestor {
			ancestorOID = ancestor.OID
		}
		headOID := ""
		if hasHead {
			headOID = head.OID
		}
		currentOID := ""
		if hasCurrent {
			currentOID = current.OID
		}
		headChanged := headOID != ancestorOID
		currentChanged := currentOID != ancestorOID
		if !headChanged {
			continue // the head did not touch this path: keep the base side
		}
		if !currentChanged {
			// only the head changed it: take the head's version (or its deletion)
			if !hasHead {
				delete(result, path)
			} else {
				result[path] = head
			}
			continue
		}
		// both sides changed it: fine when they agree, a conflict otherwise
		if headOID == currentOID {
			continue
		}
		return nil, true
	}
	return result, false
}

// -------------------------------------------------------------------------
// issues, pull requests, labels, comments
// -------------------------------------------------------------------------

func (s *Store) CreateLabel(repo *Repo, name, color string) *Label {
	if _, ok := repo.Labels[name]; ok {
		return nil
	}
	repo.LabelSeq++
	label := &Label{ID: repo.LabelSeq, Name: name, Color: color, Description: ""}
	repo.Labels[name] = label
	return label
}

func (s *Store) CreateIssue(repo *Repo, title, body string, labels []string) *Issue {
	number := repo.NextNumber
	repo.NextNumber++
	s.issueIDSeq++
	s.globalSeq++
	issue := &Issue{
		Number: number,
		ID:     s.issueIDSeq,
		Title:  title,
		Body:   body,
		State:  "open",
		Labels: append([]string{}, labels...),
		Seq:    s.globalSeq,
	}
	repo.Issues[number] = issue
	return issue
}

func (s *Store) CreateComment(repo *Repo, issue *Issue, body string) *Comment {
	repo.CommentSeq++
	s.globalSeq++
	comment := &Comment{ID: repo.CommentSeq, Body: body, Seq: s.globalSeq}
	issue.Comments = append(issue.Comments, comment)
	issue.cached = nil
	return comment
}

func (s *Store) CreatePull(repo *Repo, headRef, baseRef, headSha, baseSha, title, body string) *Pull {
	number := repo.NextNumber
	repo.NextNumber++
	s.pullIDSeq++
	s.globalSeq++
	pull := &Pull{
		Number:  number,
		ID:      s.pullIDSeq,
		Title:   title,
		Body:    body,
		State:   "open",
		HeadRef: headRef,
		BaseRef: baseRef,
		HeadSha: headSha,
		BaseSha: baseSha,
		Seq:     s.globalSeq,
	}
	repo.Pulls[number] = pull
	return pull
}

// PullTips returns a pull request's current head/base tips (branches move;
// the PR follows them).
func (s *Store) PullTips(repo *Repo, pull *Pull) (headSha, baseSha string) {
	headSha = pull.HeadSha
	if v, ok := repo.Refs["refs/heads/"+pull.HeadRef]; ok {
		headSha = v
	}
	baseSha = pull.BaseSha
	if v, ok := repo.Refs["refs/heads/"+pull.BaseRef]; ok {
		baseSha = v
	}
	return headSha, baseSha
}

// PullMergeable returns whether a pull request merges cleanly: MERGEABLE,
// CONFLICTING or UNKNOWN.
func (s *Store) PullMergeable(repo *Repo, pull *Pull) string {
	if pull.State != "open" {
		return "UNKNOWN"
	}
	headSha, baseSha := s.PullTips(repo, pull)
	if headSha == "" || baseSha == "" {
		return "UNKNOWN"
	}
	if s.IsAncestor(repo, headSha, baseSha) {
		return "MERGEABLE"
	}
	if _, conflict := s.ThreeWayMerge(repo, baseSha, headSha); conflict {
		return "CONFLICTING"
	}
	return "MERGEABLE"
}

type pullFile struct {
	Filename  string
	Status    string
	Additions int
	Deletions int
	Changes   int
}

// PullFiles returns the files a pull request changes (merge-base tree vs head
// tree).
func (s *Store) PullFiles(repo *Repo, pull *Pull) []pullFile {
	headSha, baseSha := s.PullTips(repo, pull)
	if headSha == "" || baseSha == "" {
		return nil
	}
	headInfo, ok := s.CommitInfo(repo, headSha)
	if !ok {
		return nil
	}
	mergeBase, ok := s.MergeBase(repo, baseSha, headSha)
	if !ok {
		return nil
	}
	baseInfo, ok := s.CommitInfo(repo, mergeBase)
	if !ok {
		return nil
	}
	diffs := s.DiffTrees(repo, baseInfo.Tree, headInfo.Tree)
	out := []pullFile{}
	for _, d := range diffs {
		additions, deletions := 0, 0
		oldText := ""
		if d.OldOID != "" {
			if o, ok := repo.Objects[d.OldOID]; ok {
				oldText = string(o.Data)
			}
		}
		newText := ""
		if d.NewOID != "" {
			if o, ok := repo.Objects[d.NewOID]; ok {
				newText = string(o.Data)
			}
		}
		switch d.Status {
		case "added":
			if newText != "" {
				additions = len(strings.Split(newText, "\n"))
			}
		case "removed":
			if oldText != "" {
				deletions = len(strings.Split(oldText, "\n"))
			}
		default:
			c := LineDiff(oldText, newText)
			additions, deletions = c.Additions, c.Deletions
			if additions == 0 && deletions == 0 {
				additions = 1 // a changed file always adds at least one line
			}
		}
		out = append(out, pullFile{Filename: d.Path, Status: d.Status, Additions: additions, Deletions: deletions, Changes: additions + deletions})
	}
	return out
}

type mergeResult struct {
	OK     bool
	Sha    string
	Reason string // "conflict" | "unknown" | "closed"
}

// SquashMerge squash-merges an open pull request onto its base branch.
func (s *Store) SquashMerge(repo *Repo, pull *Pull) mergeResult {
	if pull.State != "open" {
		return mergeResult{Reason: "closed"}
	}
	headSha, baseSha := s.PullTips(repo, pull)
	if headSha == "" || baseSha == "" {
		return mergeResult{Reason: "unknown"}
	}
	if s.IsAncestor(repo, headSha, baseSha) {
		// already contained in the base: nothing to merge, report the base tip
		return mergeResult{OK: true, Sha: baseSha}
	}
	merged, conflict := s.ThreeWayMerge(repo, baseSha, headSha)
	if conflict || merged == nil {
		return mergeResult{Reason: "conflict"}
	}
	tree := s.BuildTreeFromFlat(repo, merged)
	sha := s.CreateCommit(repo, tree, []string{baseSha}, pull.Title)
	repo.Refs["refs/heads/"+pull.BaseRef] = sha
	pull.State = "closed"
	pull.Merged = true
	s.globalSeq++
	pull.MergedAt = isoTime(s.globalSeq)
	pull.MergeCommitSha = sha
	return mergeResult{OK: true, Sha: sha}
}

// atoi parses a decimal string like strconv.Atoi but returns -1 on failure.
func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return n
}
