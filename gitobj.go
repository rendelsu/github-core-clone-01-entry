// gitobj.go — the git object model: SHA-1 ids, tree/commit/tag bytes, pkt-line
// framing and packfiles (plain objects only, no deltas). Pure functions, no
// I/O, no clock: every byte a function builds is a pure function of its inputs.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

const ZeroOID = "0000000000000000000000000000000000000000"

type ObjType string

const (
	ObjCommit ObjType = "commit"
	ObjTree   ObjType = "tree"
	ObjBlob   ObjType = "blob"
	ObjTag    ObjType = "tag"
)

type GitObject struct {
	Type ObjType
	Data []byte
}

func IsOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

// hashObject is git's object id: SHA-1 over `<type> <size>\0` + the bytes.
func hashObject(typ ObjType, data []byte) string {
	h := sha1.New()
	h.Write([]byte(string(typ) + " " + strconv.Itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------------------
// pkt-line
// ---------------------------------------------------------------------------

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

func pktLine(payload []byte) []byte {
	out := make([]byte, 0, len(payload)+4)
	head := fmt.Sprintf("%04x", len(payload)+4)
	out = append(out, head...)
	out = append(out, payload...)
	return out
}

func pktLineStr(s string) []byte { return pktLine([]byte(s)) }

type pktKind int

const (
	pktData pktKind = iota
	pktFlushKind
	pktDelimKind
	pktEndKind
)

type pkt struct {
	kind    pktKind
	payload []byte
}

func isHex4(b []byte) bool {
	if len(b) != 4 {
		return false
	}
	for _, c := range b {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// decodePktLines decodes pkt-lines from the head of buf. It stops at the
// first four bytes that are not a hex length (raw pack bytes follow) or at
// the buffer's end. Returns the packets and the unconsumed tail.
func decodePktLines(buf []byte) ([]pkt, []byte) {
	var packets []pkt
	off := 0
	for off+4 <= len(buf) {
		head := buf[off : off+4]
		if !isHex4(head) {
			break
		}
		n, err := strconv.ParseInt(string(head), 16, 32)
		if err != nil {
			break
		}
		length := int(n)
		switch length {
		case 0:
			packets = append(packets, pkt{kind: pktFlushKind})
			off += 4
			continue
		case 1:
			packets = append(packets, pkt{kind: pktDelimKind})
			off += 4
			continue
		case 2:
			packets = append(packets, pkt{kind: pktEndKind})
			off += 4
			continue
		}
		if length < 4 || off+length > len(buf) {
			break
		}
		packets = append(packets, pkt{kind: pktData, payload: buf[off+4 : off+length]})
		off += length
	}
	return packets, buf[off:]
}

// encodePktLines encodes a mixed sequence: byte slices/strings are data
// packets, pktFlush/pktDelim are the control packets.
func encodePktLines(items [][]byte) []byte {
	var out []byte
	for _, item := range items {
		out = append(out, item...)
	}
	return out
}

func encodeItems(items []any) []byte {
	var out []byte
	for _, item := range items {
		switch v := item.(type) {
		case string:
			if v == "flush" {
				out = append(out, pktFlush...)
			} else if v == "delim" {
				out = append(out, pktDelim...)
			} else {
				out = append(out, pktLineStr(v)...)
			}
		case []byte:
			out = append(out, pktLine(v)...)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// trees
// ---------------------------------------------------------------------------

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// parseTree parses a tree object into its entries, in stored order.
func parseTree(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	off := 0
	for off < len(data) {
		sp := bytes.IndexByte(data[off:], ' ')
		if sp < 0 {
			return nil, fmt.Errorf("tree entry has no mode separator")
		}
		sp += off
		nul := bytes.IndexByte(data[sp+1:], 0)
		if nul < 0 {
			return nil, fmt.Errorf("tree entry has no name terminator")
		}
		nul += sp + 1
		if nul+21 > len(data) {
			return nil, fmt.Errorf("tree entry has a truncated object id")
		}
		entries = append(entries, TreeEntry{
			Mode: string(data[off:sp]),
			Name: string(data[sp+1 : nul]),
			OID:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		off = nul + 21
	}
	return entries, nil
}

// compareTreeNames is git's tree-entry sort: byte order of the name, a
// directory compared as if it ended in '/'.
func compareTreeNames(a, b TreeEntry) int {
	an := a.Name
	if a.Mode == "40000" {
		an += "/"
	}
	bn := b.Name
	if b.Mode == "40000" {
		bn += "/"
	}
	return strings.Compare(an, bn)
}

// buildTree builds a tree object from entries; entries are sorted the way
// git sorts them.
func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return compareTreeNames(sorted[i], sorted[j]) < 0 })
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// commits and tags
// ---------------------------------------------------------------------------

type CommitInfo struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Timestamp int64
	Message   string
}

// parseCommit parses a commit object. The message is the bytes after the
// blank line, verbatim.
func parseCommit(data []byte) CommitInfo {
	blank := bytes.Index(data, []byte("\n\n"))
	headEnd := len(data)
	message := ""
	if blank >= 0 {
		headEnd = blank
		message = string(data[blank+2:])
	}
	var info CommitInfo
	for _, line := range strings.Split(string(data[:headEnd]), "\n") {
		if line == "" {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		key, value := line, ""
		if sp >= 0 {
			key, value = line[:sp], line[sp+1:]
		}
		switch key {
		case "tree":
			info.Tree = value
		case "parent":
			info.Parents = append(info.Parents, value)
		case "author":
			info.Author = value
		case "committer":
			info.Committer = value
		}
	}
	// timestamp from the author line: " ... <ts> +0000"
	if i := strings.LastIndex(info.Author, " "); i >= 0 {
		rest := info.Author[:i]
		if j := strings.LastIndex(rest, " "); j >= 0 {
			if ts, err := strconv.ParseInt(rest[j+1:], 10, 64); err == nil {
				info.Timestamp = ts
			}
		}
	}
	info.Message = message
	return info
}

// buildCommit builds a commit object's bytes. The message is stored verbatim
// (no newline added).
func buildCommit(tree string, parents []string, author, committer string, timestamp int64, message string) []byte {
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author " + author + " " + strconv.FormatInt(timestamp, 10) + " +0000\n")
	b.WriteString("committer " + committer + " " + strconv.FormatInt(timestamp, 10) + " +0000\n")
	b.WriteString("\n")
	b.WriteString(message)
	return []byte(b.String())
}

type TagInfo struct {
	Object  string
	Type    ObjType
	Tag     string
	Tagger  string
	Message string
}

// parseTag parses an annotated tag object.
func parseTag(data []byte) TagInfo {
	blank := bytes.Index(data, []byte("\n\n"))
	headEnd := len(data)
	message := ""
	if blank >= 0 {
		headEnd = blank
		message = string(data[blank+2:])
	}
	headers := map[string]string{}
	for _, line := range strings.Split(string(data[:headEnd]), "\n") {
		if line == "" {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		headers[line[:sp]] = line[sp+1:]
	}
	typ := ObjType(headers["type"])
	if typ == "" {
		typ = ObjCommit
	}
	return TagInfo{
		Object:  headers["object"],
		Type:    typ,
		Tag:     headers["tag"],
		Tagger:  headers["tagger"],
		Message: message,
	}
}

// ---------------------------------------------------------------------------
// packfiles (plain objects, no deltas)
// ---------------------------------------------------------------------------

var typeCode = map[ObjType]int{ObjCommit: 1, ObjTree: 2, ObjBlob: 3, ObjTag: 4}
var codeType = map[int]ObjType{1: ObjCommit, 2: ObjTree, 3: ObjBlob, 4: ObjTag}

func writePackObjectHeader(typ ObjType, size int) []byte {
	code := typeCode[typ]
	var out []byte
	remaining := size
	first := (code << 4) | (remaining & 0x0f)
	remaining /= 16
	for remaining > 0 {
		out = append(out, byte(first|0x80))
		first = remaining & 0x7f
		remaining /= 128
	}
	out = append(out, byte(first))
	return out
}

func deflate(data []byte) []byte {
	var buf bytes.Buffer
	w, _ := zlib.NewWriterLevel(&buf, zlib.BestSpeed)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

// buildPack builds a packfile from plain objects. A cached zlib stream per
// object may be supplied (objects are immutable, so the stream never
// changes). The trailer is SHA-1 over everything before it.
func buildPack(objects []packObject) []byte {
	var body bytes.Buffer
	body.WriteString("PACK")
	var hdr [8]byte
	hdr[0] = 0
	hdr[1] = 0
	hdr[2] = 0
	hdr[3] = 2
	body.Write(hdr[:4])
	body.WriteByte(byte(len(objects) >> 24))
	body.WriteByte(byte(len(objects) >> 16))
	body.WriteByte(byte(len(objects) >> 8))
	body.WriteByte(byte(len(objects)))
	for _, o := range objects {
		body.Write(writePackObjectHeader(o.Type, len(o.Data)))
		if o.Deflated != nil {
			body.Write(o.Deflated)
		} else {
			body.Write(deflate(o.Data))
		}
	}
	raw := body.Bytes()
	sum := sha1.Sum(raw)
	return append(raw, sum[:]...)
}

type packObject struct {
	Type     ObjType
	Data     []byte
	Deflated []byte
}

// readPack reads a packfile of plain objects. Delta entries are refused (the
// suite's client never sends them; a delta here is an unpack error, not
// silent corruption).
func readPack(pack []byte) ([]GitObject, error) {
	if len(pack) < 32 {
		return nil, fmt.Errorf("pack is shorter than its trailer")
	}
	if string(pack[:4]) != "PACK" {
		return nil, fmt.Errorf("pack does not start with PACK")
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	trailerStart := len(pack) - 20
	objects := make([]GitObject, 0, count)
	off := 12
	for index := 0; index < count; index++ {
		if off >= trailerStart {
			return nil, fmt.Errorf("pack is truncated")
		}
		first := int(pack[off])
		size := first & 0x0f
		shift := uint(4)
		off++
		b := first
		for b&0x80 != 0 {
			if off >= trailerStart {
				return nil, fmt.Errorf("pack entry size varint is truncated")
			}
			b = int(pack[off])
			off++
			size += (b & 0x7f) << shift
			shift += 7
			if shift > 63 {
				return nil, fmt.Errorf("pack entry size varint is too long")
			}
		}
		typeCode := (first >> 4) & 0x07
		if typeCode == 6 || typeCode == 7 {
			return nil, fmt.Errorf("delta pack entries are not supported")
		}
		typ, ok := codeType[typeCode]
		if !ok {
			return nil, fmt.Errorf("pack entry has unknown type code %d", typeCode)
		}
		// Inflate the zlib stream starting at off; Go's zlib reader stops
		// exactly at the stream's end over a bytes.Reader (verified).
		r := bytes.NewReader(pack[off:trailerStart])
		zr, err := zlib.NewReader(r)
		if err != nil {
			return nil, err
		}
		inflated, err := io.ReadAll(zr)
		zr.Close()
		if err != nil {
			return nil, err
		}
		if len(inflated) != size {
			return nil, fmt.Errorf("pack entry declares %d bytes but inflated to %d", size, len(inflated))
		}
		consumed := (trailerStart - off) - r.Len()
		off += consumed
		objects = append(objects, GitObject{Type: typ, Data: inflated})
	}
	if off != trailerStart {
		return nil, fmt.Errorf("pack has stray bytes before its trailer")
	}
	trailer := hex.EncodeToString(pack[trailerStart:])
	computed := sha1Hex(pack[:trailerStart])
	if trailer != computed {
		return nil, fmt.Errorf("pack trailer does not match its bytes")
	}
	return objects, nil
}
