// githttp.go — the git smart-HTTP surface: ref advertisements (v0 and v2),
// ls-refs, fetch (v2 sections and the v0 sideband form) and receive-pack
// pushes with per-ref report-status. All state lives in the Store; every
// response is built from the repo's refs and objects.

package main

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
)

const agent = "github-core-clone/1.0"

const uploadCaps = "multi_ack thin-pack side-band-64k ofs-delta shallow symref=HEAD:refs/heads/main agent=" + agent
const receiveCaps = "report-status side-band-64k delete-refs ofs-delta agent=" + agent

type refPair struct {
	name string
	oid  string
}

// sortedRefs returns refs in advertisement order: heads first, then tags,
// each sorted.
func sortedRefs(repo *Repo) []refPair {
	var heads, tags []refPair
	for name, oid := range repo.Refs {
		if strings.HasPrefix(name, "refs/heads/") {
			heads = append(heads, refPair{name, oid})
		} else if strings.HasPrefix(name, "refs/tags/") {
			tags = append(tags, refPair{name, oid})
		} else {
			heads = append(heads, refPair{name, oid})
		}
	}
	sort.Slice(heads, func(i, j int) bool { return heads[i].name < heads[j].name })
	sort.Slice(tags, func(i, j int) bool { return tags[i].name < tags[j].name })
	return append(heads, tags...)
}

// peeledOf returns the peeled commit of an annotated tag ref, or "".
func peeledOf(repo *Repo, oid string) string {
	object, ok := repo.Objects[oid]
	if !ok || object.Type != ObjTag {
		return ""
	}
	tag := parseTag(object.Data)
	target, ok := repo.Objects[tag.Object]
	if !ok {
		return ""
	}
	if target.Type == ObjTag {
		return peeledOf(repo, tag.Object)
	}
	return tag.Object
}

// v0Advertisement returns the v0 advertisement body for a service.
func v0Advertisement(repo *Repo, service string) []byte {
	caps := uploadCaps
	if service == "git-receive-pack" {
		caps = receiveCaps
	}
	items := []any{"# service=" + service + "\n"}
	refs := sortedRefs(repo)
	if len(refs) == 0 {
		items = append(items, ZeroOID+" capabilities^{}\x00"+caps+"\n")
	} else {
		headOID := repo.Refs["refs/heads/main"]
		if headOID == "" {
			headOID = refs[0].oid
		}
		items = append(items, headOID+" HEAD\x00"+caps+"\n")
		for _, ref := range refs {
			items = append(items, ref.oid+" "+ref.name+"\n")
			if peeled := peeledOf(repo, ref.oid); peeled != "" {
				items = append(items, peeled+" "+ref.name+"^{}\n")
			}
		}
	}
	items = append(items, "flush")
	return encodeItems(items)
}

// v2Advertisement returns the v2 capability advertisement body.
func v2Advertisement() []byte {
	return encodeItems([]any{
		"version 2\n",
		"agent=" + agent + "\n",
		"ls-refs=unborn\n",
		"fetch=shallow wait-for-done\n",
		"object-format=sha1\n",
		"flush",
	})
}

// requestLines returns the lines of a v2 request body, as text (control
// packets dropped).
func requestLines(body []byte) []string {
	packets, _ := decodePktLines(body)
	var lines []string
	for _, p := range packets {
		if p.kind == pktData {
			lines = append(lines, string(p.payload))
		}
	}
	return lines
}

type lsRefsArgs struct {
	peel     bool
	symrefs  bool
	unborn   bool
	prefixes []string
}

// lsRefsResponse returns the v2 ls-refs answer.
func lsRefsResponse(repo *Repo, body []byte) []byte {
	lines := requestLines(body)
	args := lsRefsArgs{}
	for _, line := range lines {
		text := strings.TrimSuffix(line, "\n")
		switch {
		case text == "peel":
			args.peel = true
		case text == "symrefs":
			args.symrefs = true
		case text == "unborn":
			args.unborn = true
		case strings.HasPrefix(text, "ref-prefix "):
			args.prefixes = append(args.prefixes, text[len("ref-prefix "):])
		}
	}
	var out []any
	keep := func(name string) bool {
		if len(args.prefixes) == 0 {
			return true
		}
		for _, p := range args.prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	headOID, hasHead := repo.Refs["refs/heads/main"]
	if len(args.prefixes) == 0 {
		if !hasHead {
			out = append(out, "unborn HEAD symref-target:refs/heads/main\n")
		} else {
			out = append(out, headOID+" HEAD symref-target:refs/heads/main\n")
		}
	}
	for _, ref := range sortedRefs(repo) {
		if !keep(ref.name) {
			continue
		}
		line := ref.oid + " " + ref.name
		if args.peel {
			if peeled := peeledOf(repo, ref.oid); peeled != "" {
				line += " peeled:" + peeled
			}
		}
		out = append(out, line+"\n")
	}
	out = append(out, "flush")
	return encodeItems(out)
}

type fetchArgs struct {
	wants   []string
	haves   []string
	shallow []string
	depth   int // -1 means unbounded
}

// parseFetchArgs parses a v2 fetch request body (or a v0 want/have body)
// into its arguments.
func parseFetchArgs(body []byte) fetchArgs {
	lines := requestLines(body)
	args := fetchArgs{depth: -1}
	for _, line := range lines {
		text := strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(text, "want "):
			args.wants = append(args.wants, strings.Split(text[5:], " ")[0])
		case strings.HasPrefix(text, "have "):
			args.haves = append(args.haves, text[5:])
		case strings.HasPrefix(text, "shallow "):
			args.shallow = append(args.shallow, text[8:])
		case strings.HasPrefix(text, "deepen "):
			if n, err := strconv.Atoi(text[7:]); err == nil && n > 0 {
				args.depth = n
			}
		}
	}
	return args
}

type fetchPlan struct {
	// send is the objects to send, in deterministic order.
	send []string
	// shallow is the new shallow boundary commits.
	shallow []string
	// unshallow is previously shallow commits that are now fully included.
	unshallow []string
	// ack is an ack line for the negotiation, or "NAK".
	ack string
	// acked is whether a common object was found (an ACK, else a NAK).
	acked bool
}

// planFetch plans a fetch: which objects go out, which shallow lines the
// answer carries, and the negotiation line. send is reachable(wants, depth)
// minus reachable(haves).
func planFetch(store *Store, repo *Repo, args fetchArgs) fetchPlan {
	seen := map[string]bool{}
	var wants []string
	for _, oid := range args.wants {
		if IsOID(oid) {
			if _, ok := repo.Objects[oid]; ok && !seen[oid] {
				seen[oid] = true
				wants = append(wants, oid)
			}
		}
	}
	var haves []string
	for _, oid := range args.haves {
		if IsOID(oid) {
			if _, ok := repo.Objects[oid]; ok {
				haves = append(haves, oid)
			}
		}
	}
	depth := args.depth
	if depth < 0 {
		depth = infiniteDepth
	}
	wantReach := store.Reachable(repo, wants, depth, nil)
	// the client's shallow boundaries cut the have closure: it holds the
	// boundary commit and its tree, but none of that commit's history
	cut := map[string]bool{}
	for _, s := range args.shallow {
		cut[s] = true
	}
	var haveReach map[string]bool
	if len(haves) > 0 {
		haveReach = store.Reachable(repo, haves, infiniteDepth, cut)
	} else {
		haveReach = map[string]bool{}
	}
	var send []string
	for oid := range wantReach {
		if !haveReach[oid] {
			send = append(send, oid)
		}
	}
	sort.Strings(send)
	// The shallow boundary: included commits whose parents fall outside the cut.
	// Only distances up to depth-1 are ever read, so the walk is capped there
	// instead of traversing the whole history (a depth-1 fetch only needs the
	// wants themselves).
	var distances map[string]int
	if args.depth >= 0 {
		distances = store.CommitDistances(repo, wants, depth-1)
	}
	var shallow, unshallow []string
	if distances != nil {
		for oid, dist := range distances {
			if dist != depth-1 {
				continue
			}
			if info, ok := store.CommitInfo(repo, oid); ok && len(info.Parents) > 0 {
				shallow = append(shallow, oid)
			}
		}
		sort.Strings(shallow)
		// a previous boundary is unshallowed only when its parents are now included
		for _, boundary := range args.shallow {
			dist, ok := distances[boundary]
			if !ok {
				continue
			}
			found := false
			for _, s := range shallow {
				if s == boundary {
					found = true
					break
				}
			}
			if found {
				continue
			}
			if dist < depth-1 {
				unshallow = append(unshallow, boundary)
			}
		}
		sort.Strings(unshallow)
	} else {
		// a full (unbounded) fetch unshallows every boundary now included
		for _, boundary := range args.shallow {
			if wantReach[boundary] {
				unshallow = append(unshallow, boundary)
			}
		}
		sort.Strings(unshallow)
	}
	// Negotiation: an ACK names the first have that is an ancestor of a want.
	// The closure is only needed when the client actually sent haves; a fresh
	// clone sends none, and walking the whole history for an empty loop is pure
	// waste (reachable() also visits every tree and blob, not just commits).
	ack := "NAK"
	acked := false
	if len(haves) > 0 {
		wantClosure := store.Reachable(repo, wants, infiniteDepth, nil)
		for _, have := range haves {
			if wantClosure[have] {
				ack = "ACK " + have
				acked = true
				break
			}
		}
	}
	return fetchPlan{send: send, shallow: shallow, unshallow: unshallow, ack: ack, acked: acked}
}

// sidebandPack returns a pack's bytes as sideband band-1 pkt-lines (chunks
// of at most 65515 bytes).
func sidebandPack(pack []byte) [][]byte {
	const chunk = 65515
	var parts [][]byte
	for off := 0; off < len(pack); off += chunk {
		end := off + chunk
		if end > len(pack) {
			end = len(pack)
		}
		slice := pack[off:end]
		framed := make([]byte, 0, len(slice)+1)
		framed = append(framed, 1)
		framed = append(framed, slice...)
		parts = append(parts, pktLine(framed))
	}
	return parts
}

// buildPackFor builds a pack from object ids, using the per-object deflate cache.
func buildPackFor(store *Store, repo *Repo, oids []string) []byte {
	objects := make([]packObject, 0, len(oids))
	for _, oid := range oids {
		object, ok := repo.Objects[oid]
		if !ok {
			continue
		}
		objects = append(objects, packObject{Type: object.Type, Data: object.Data, Deflated: store.Deflated(repo, oid)})
	}
	return buildPack(objects)
}

// fetchV2Response returns the v2 fetch answer: acknowledgments,
// shallow-info, wanted-refs, packfile.
func fetchV2Response(store *Store, repo *Repo, body []byte) []byte {
	args := parseFetchArgs(body)
	plan := planFetch(store, repo, args)
	var parts [][]byte
	ackLines := []any{"acknowledgments\n", plan.ack + "\n"}
	if plan.acked {
		ackLines = append(ackLines, "ready\n")
	}
	ackLines = append(ackLines, "delim")
	parts = append(parts, encodeItems(ackLines))
	if len(plan.shallow) > 0 || len(plan.unshallow) > 0 {
		lines := []any{"shallow-info\n"}
		for _, oid := range plan.shallow {
			lines = append(lines, "shallow "+oid+"\n")
		}
		for _, oid := range plan.unshallow {
			lines = append(lines, "unshallow "+oid+"\n")
		}
		lines = append(lines, "delim")
		parts = append(parts, encodeItems(lines))
	}
	var wantedRefs []string
	for _, want := range args.wants {
		// deterministic ref order
		names := make([]string, 0, len(repo.Refs))
		for name := range repo.Refs {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if repo.Refs[name] == want {
				wantedRefs = append(wantedRefs, want+" "+name+"\n")
			}
		}
	}
	if len(wantedRefs) > 0 {
		items := []any{"wanted-refs\n"}
		for _, r := range wantedRefs {
			items = append(items, r)
		}
		items = append(items, "delim")
		parts = append(parts, encodeItems(items))
	}
	if len(plan.send) > 0 {
		pack := buildPackFor(store, repo, plan.send)
		parts = append(parts, pktLineStr("packfile\n"))
		parts = append(parts, sidebandPack(pack)...)
	}
	parts = append(parts, pktFlush)
	return bytes.Join(parts, nil)
}

// fetchV0Response returns the v0 fetch answer: NAK/ACK lines, shallow
// lines, then the sideband pack.
func fetchV0Response(store *Store, repo *Repo, body []byte) []byte {
	args := parseFetchArgs(body)
	plan := planFetch(store, repo, args)
	var parts [][]byte
	parts = append(parts, pktLineStr(plan.ack+"\n"))
	if plan.acked {
		parts = append(parts, pktLineStr("ready\n"))
	}
	for _, oid := range plan.shallow {
		parts = append(parts, pktLineStr("shallow "+oid+"\n"))
	}
	for _, oid := range plan.unshallow {
		parts = append(parts, pktLineStr("unshallow "+oid+"\n"))
	}
	if len(plan.send) > 0 {
		pack := buildPackFor(store, repo, plan.send)
		parts = append(parts, sidebandPack(pack)...)
	}
	parts = append(parts, pktFlush)
	return bytes.Join(parts, nil)
}

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

type pushOutcome struct {
	// unpack is the unpack line's verdict: "ok" or a human error.
	unpack string
	refs   []pushRefResult
}

type pushRefResult struct {
	ref    string
	ok     bool
	reason string
}

// applyPush applies a receive-pack request: unpack the pack, then validate
// and apply each command in order. A refused ref never moves; the accepted
// ones do.
func applyPush(store *Store, repo *Repo, body []byte) pushOutcome {
	packets, rest := decodePktLines(body)
	var commands []pushCommand
	for _, p := range packets {
		if p.kind != pktData {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			line = line[:nul] // capabilities ride the first command line
		}
		parts := strings.Split(line, " ")
		if len(parts) < 3 {
			continue
		}
		commands = append(commands, pushCommand{oldOID: parts[0], newOID: parts[1], ref: parts[2]})
	}
	// Unpack first: objects land even when a ref is refused.
	if len(rest) > 0 {
		objects, err := readPack(rest)
		if err != nil {
			return pushOutcome{unpack: err.Error()}
		}
		for _, object := range objects {
			store.PutObject(repo, object.Type, object.Data)
		}
	}
	var refs []pushRefResult
	for _, cmd := range commands {
		current, hasCurrent := repo.Refs[cmd.ref]
		if cmd.newOID == ZeroOID {
			// delete
			if !hasCurrent {
				refs = append(refs, pushRefResult{ref: cmd.ref, reason: "no such ref"})
			} else if cmd.oldOID != current {
				refs = append(refs, pushRefResult{ref: cmd.ref, reason: "stale info"})
			} else {
				delete(repo.Refs, cmd.ref)
				refs = append(refs, pushRefResult{ref: cmd.ref, ok: true})
			}
			continue
		}
		if !IsOID(cmd.newOID) {
			refs = append(refs, pushRefResult{ref: cmd.ref, reason: "missing object"})
			continue
		}
		if _, ok := repo.Objects[cmd.newOID]; !ok {
			refs = append(refs, pushRefResult{ref: cmd.ref, reason: "missing object"})
			continue
		}
		if cmd.oldOID == ZeroOID {
			// create
			if hasCurrent {
				refs = append(refs, pushRefResult{ref: cmd.ref, reason: "already exists"})
			} else {
				repo.Refs[cmd.ref] = cmd.newOID
				refs = append(refs, pushRefResult{ref: cmd.ref, ok: true})
			}
			continue
		}
		// update
		if !hasCurrent {
			refs = append(refs, pushRefResult{ref: cmd.ref, reason: "no such ref"})
			continue
		}
		if cmd.oldOID != current {
			refs = append(refs, pushRefResult{ref: cmd.ref, reason: "stale info"})
			continue
		}
		if cmd.newOID == current {
			refs = append(refs, pushRefResult{ref: cmd.ref, ok: true})
			continue
		}
		if strings.HasPrefix(cmd.ref, "refs/tags/") {
			// a tag is not fast-forwarded: an update without force is refused
			refs = append(refs, pushRefResult{ref: cmd.ref, reason: "already exists"})
			continue
		}
		if !store.IsAncestor(repo, cmd.oldOID, cmd.newOID) {
			refs = append(refs, pushRefResult{ref: cmd.ref, reason: "non-fast-forward"})
			continue
		}
		repo.Refs[cmd.ref] = cmd.newOID
		refs = append(refs, pushRefResult{ref: cmd.ref, ok: true})
	}
	return pushOutcome{unpack: "ok", refs: refs}
}

// pushResponse returns the receive-pack answer: the report-status stream
// inside sideband band 1.
func pushResponse(outcome pushOutcome) []byte {
	inner := []any{"unpack " + outcome.unpack + "\n"}
	for _, ref := range outcome.refs {
		if ref.ok {
			inner = append(inner, "ok "+ref.ref+"\n")
		} else {
			inner = append(inner, "ng "+ref.ref+" "+ref.reason+"\n")
		}
	}
	inner = append(inner, "flush")
	report := encodeItems(inner)
	var parts [][]byte
	const chunk = 65515
	for off := 0; off < len(report); off += chunk {
		end := off + chunk
		if end > len(report) {
			end = len(report)
		}
		slice := report[off:end]
		framed := make([]byte, 0, len(slice)+1)
		framed = append(framed, 1)
		framed = append(framed, slice...)
		parts = append(parts, pktLine(framed))
	}
	parts = append(parts, pktFlush)
	return bytes.Join(parts, nil)
}
