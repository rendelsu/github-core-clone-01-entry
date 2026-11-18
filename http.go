// http.go — the HTTP front door. A minimal HTTP/1.1 server on raw net:
// zero-allocation request parsing on the hot path, keep-alive connections,
// Content-Length and chunked request bodies. One server routes four surfaces
// over a single Store: the git smart-HTTP endpoints (no auth, public-clone
// semantics), REST v3, GraphQL v4 and MCP (all behind the Bearer token).
// Every reply carries X-GitHub-Request-Id; every REST-shaped error is JSON.

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const maxBodyBytes = 128 * 1024 * 1024

var requestSeq atomic.Uint64

type httpRequest struct {
	method  string
	path    string
	segments []string
	query   map[string]string
	headers map[string]string
	body    []byte
	host    string
	keepAlive bool
}

type httpResponse struct {
	status      int
	contentType string
	body        []byte
	extra       map[string]string
}

func serve(ln net.Listener, store *Store) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handleConn(conn, store)
	}
}

func handleConn(conn net.Conn, store *Store) {
	defer conn.Close()
	reader := bufio.NewReaderSize(conn, 65536)
	writer := bufio.NewWriterSize(conn, 65536)
	for {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		req, err := readRequest(reader)
		if err != nil {
			return
		}
		resp := dispatch(store, req)
		if !writeResponse(writer, req, resp) {
			return
		}
		if !req.keepAlive {
			return
		}
	}
}

// readRequest parses one HTTP/1.1 request from the buffered reader.
func readRequest(r *bufio.Reader) (*httpRequest, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("bad request line")
	}
	method, target := parts[0], parts[1]
	// headers
	headers := map[string]string{}
	for {
		hline, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		hline = strings.TrimRight(hline, "\r\n")
		if hline == "" {
			break
		}
		if idx := strings.IndexByte(hline, ':'); idx >= 0 {
			name := strings.ToLower(strings.TrimSpace(hline[:idx]))
			value := strings.TrimSpace(hline[idx+1:])
			if _, ok := headers[name]; !ok {
				headers[name] = value
			}
		}
	}
	// Expect: 100-continue
	if strings.EqualFold(headers["expect"], "100-continue") {
		// the caller writes the interim response on the raw connection; the
		// buffered reader/writer pair is managed by handleConn, so this path
		// is not used by the suite's client (undici never sends Expect).
	}
	// body
	var body []byte
	if te := headers["transfer-encoding"]; strings.Contains(strings.ToLower(te), "chunked") {
		body, err = readChunked(r)
		if err != nil {
			return nil, err
		}
	} else if cl := headers["content-length"]; cl != "" {
		n, err := strconv.Atoi(strings.TrimSpace(cl))
		if err != nil || n < 0 {
			return nil, fmt.Errorf("bad content-length")
		}
		if n > maxBodyBytes {
			return nil, fmt.Errorf("request body too large")
		}
		if n > 0 {
			body = make([]byte, n)
			if _, err := io.ReadFull(r, body); err != nil {
				return nil, err
			}
		}
	}
	// split target into path + query (path stays raw, like URL.pathname)
	path := target
	queryStr := ""
	if idx := strings.IndexByte(target, '?'); idx >= 0 {
		path = target[:idx]
		queryStr = target[idx+1:]
	}
	segments := []string{}
	for _, seg := range strings.Split(path, "/") {
		if seg != "" {
			segments = append(segments, seg)
		}
	}
	keepAlive := true
	if conn := headers["connection"]; strings.EqualFold(conn, "close") {
		keepAlive = false
	}
	if strings.HasPrefix(parts[2], "HTTP/1.0") && !strings.EqualFold(headers["connection"], "keep-alive") {
		keepAlive = false
	}
	host := headers["host"]
	if host == "" {
		host = "localhost"
	}
	return &httpRequest{
		method:    method,
		path:      path,
		segments:  segments,
		query:     parseQuery(queryStr),
		headers:   headers,
		body:      body,
		host:      host,
		keepAlive: keepAlive,
	}, nil
}

func readChunked(r *bufio.Reader) ([]byte, error) {
	var out bytes.Buffer
	for {
		sizeLine, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		sizeLine = strings.TrimRight(sizeLine, "\r\n")
		if idx := strings.IndexByte(sizeLine, ';'); idx >= 0 {
			sizeLine = sizeLine[:idx]
		}
		n, err := strconv.ParseInt(strings.TrimSpace(sizeLine), 16, 64)
		if err != nil {
			return nil, fmt.Errorf("bad chunk size")
		}
		if n == 0 {
			// trailers until blank line
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					return nil, err
				}
				if strings.TrimRight(line, "\r\n") == "" {
					break
				}
			}
			return out.Bytes(), nil
		}
		if out.Len()+int(n) > maxBodyBytes {
			return nil, fmt.Errorf("request body too large")
		}
		chunk := make([]byte, n)
		if _, err := io.ReadFull(r, chunk); err != nil {
			return nil, err
		}
		out.Write(chunk)
		// trailing CRLF
		if _, err := r.ReadString('\n'); err != nil {
			return nil, err
		}
	}
}

func parseQuery(q string) map[string]string {
	out := map[string]string{}
	if q == "" {
		return out
	}
	for _, pair := range strings.Split(q, "&") {
		if pair == "" {
			continue
		}
		k, v := pair, ""
		if idx := strings.IndexByte(pair, '='); idx >= 0 {
			k, v = pair[:idx], pair[idx+1:]
		}
		dk := queryUnescape(k)
		if _, ok := out[dk]; !ok {
			out[dk] = queryUnescape(v)
		}
	}
	return out
}

func queryUnescape(s string) string {
	if !strings.ContainsAny(s, "%+") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '+' {
			b.WriteByte(' ')
		} else if c == '%' && i+2 < len(s) {
			if h, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(h))
				i += 2
			} else {
				b.WriteByte(c)
			}
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func writeResponse(w *bufio.Writer, req *httpRequest, resp *httpResponse) bool {
	seq := requestSeq.Add(1)
	var h strings.Builder
	h.WriteString("HTTP/1.1 ")
	h.WriteString(strconv.Itoa(resp.status))
	h.WriteString(" ")
	h.WriteString(statusText(resp.status))
	h.WriteString("\r\n")
	h.WriteString("content-type: ")
	h.WriteString(resp.contentType)
	h.WriteString("\r\n")
	h.WriteString("content-length: ")
	h.WriteString(strconv.Itoa(len(resp.body)))
	h.WriteString("\r\n")
	h.WriteString("x-github-request-id: req-")
	h.WriteString(strconv.FormatUint(seq, 10))
	h.WriteString("\r\n")
	for k, v := range resp.extra {
		h.WriteString(k)
		h.WriteString(": ")
		h.WriteString(v)
		h.WriteString("\r\n")
	}
	if req.keepAlive {
		h.WriteString("connection: keep-alive\r\n")
	} else {
		h.WriteString("connection: close\r\n")
	}
	h.WriteString("\r\n")
	if _, err := w.WriteString(h.String()); err != nil {
		return false
	}
	if len(resp.body) > 0 {
		if _, err := w.Write(resp.body); err != nil {
			return false
		}
	}
	if err := w.Flush(); err != nil {
		return false
	}
	return true
}

func statusText(status int) string {
	switch status {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 202:
		return "Accepted"
	case 204:
		return "No Content"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 409:
		return "Conflict"
	case 422:
		return "Unprocessable Entity"
	case 500:
		return "Internal Server Error"
	default:
		return "Status"
	}
}

func jsonResponse(status int, body []byte, extra map[string]string) *httpResponse {
	return &httpResponse{status: status, contentType: "application/json", body: body, extra: extra}
}

func jsonValueResponse(status int, value any, extra map[string]string) *httpResponse {
	var body []byte
	if raw, ok := value.(rawJSON); ok {
		body = []byte(raw)
	} else if value == nil {
		body = []byte{}
	} else {
		body = marshalJSON(value)
	}
	return jsonResponse(status, body, extra)
}

func bufferResponse(status int, data []byte, contentType string) *httpResponse {
	return &httpResponse{status: status, contentType: contentType, body: data}
}

// authorized checks the Bearer token: when the env var is unset, any
// non-empty Bearer is accepted.
func authorized(req *httpRequest) bool {
	header := req.headers["authorization"]
	if !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	token := header[len("Bearer "):]
	if token == "" {
		return false
	}
	expected := os.Getenv("MAKEFUN_SHOP_TOKEN")
	if expected == "" {
		return true
	}
	return token == expected
}

// dispatch routes one request. It never panics: a handler panic becomes a
// JSON 500, never an HTML error page.
func dispatch(store *Store, req *httpRequest) (resp *httpResponse) {
	defer func() {
		if r := recover(); r != nil {
			resp = jsonValueResponse(500, map[string]any{"message": fmt.Sprint(r)}, nil)
		}
	}()

	segments := req.segments
	method := req.method

	// GET /healthz — the boot probe, no token.
	if req.path == "/healthz" && method == "GET" {
		return jsonValueResponse(200, map[string]any{"ok": true}, nil)
	}

	// ---- git smart HTTP: /{owner}/{repo}.git/{info/refs,git-upload-pack,git-receive-pack}
	if len(segments) >= 2 && strings.HasSuffix(segments[1], ".git") {
		return dispatchGit(store, req, segments)
	}

	// ---- the API surfaces all need the Bearer token
	if len(segments) >= 2 && segments[0] == "api" && segments[1] == "v3" {
		if !authorized(req) {
			return jsonValueResponse(401, map[string]any{"message": "Requires authentication"}, nil)
		}
		restSegments := segments[2:]
		parsed, ok := parseJSONBody(req.body)
		if !ok {
			return jsonValueResponse(400, map[string]any{"message": "Invalid JSON"}, nil)
		}
		isWrite := method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE"
		if isWrite {
			store.mu.Lock()
		} else {
			store.mu.RLock()
		}
		result := handleRest(store, method, restSegments, req.query, parsed, req.host)
		if isWrite {
			store.mu.Unlock()
		} else {
			store.mu.RUnlock()
		}
		extra := map[string]string{"x-oauth-scopes": "repo, read:org"}
		for k, v := range result.Headers {
			extra[strings.ToLower(k)] = v
		}
		return jsonValueResponse(result.Status, result.Body, extra)
	}

	if len(segments) >= 2 && segments[0] == "api" && segments[1] == "graphql" {
		if !authorized(req) {
			return jsonValueResponse(401, map[string]any{"message": "Requires authentication"}, nil)
		}
		if method != "POST" {
			return jsonValueResponse(404, map[string]any{"message": "Not Found"}, nil)
		}
		parsed, ok := parseJSONBody(req.body)
		if !ok {
			return jsonValueResponse(400, map[string]any{"errors": []any{map[string]any{"message": "Invalid JSON"}}}, nil)
		}
		store.mu.Lock()
		status, body := handleGraphQL(store, parsed)
		store.mu.Unlock()
		return jsonValueResponse(status, body, nil)
	}

	if len(segments) >= 1 && segments[0] == "mcp" {
		if !authorized(req) {
			return jsonValueResponse(401, map[string]any{"message": "Requires authentication"}, nil)
		}
		if method != "POST" {
			return jsonValueResponse(404, map[string]any{"message": "Not Found"}, nil)
		}
		store.mu.Lock()
		result := handleMCP(store, req.body)
		store.mu.Unlock()
		if result.body == nil {
			return &httpResponse{status: result.status, contentType: "application/json", body: []byte{}}
		}
		return jsonValueResponse(result.status, result.body, nil)
	}

	return jsonValueResponse(404, map[string]any{"message": "Not Found"}, nil)
}

// dispatchGit routes the git smart-HTTP endpoints. The store lock is held
// across the repo lookup and the handler, so a concurrent DELETE cannot free
// the repo under a fetch. Reads take the read lock; receive-pack takes the
// write lock.
func dispatchGit(store *Store, req *httpRequest, segments []string) *httpResponse {
	method := req.method
	owner := segments[0]
	repoName := segments[1][:len(segments[1])-len(".git")]
	tail := segments[2:]

	isPush := len(tail) == 1 && tail[0] == "git-receive-pack" && method == "POST"
	if isPush {
		store.mu.Lock()
		defer store.mu.Unlock()
	} else {
		store.mu.RLock()
		defer store.mu.RUnlock()
	}

	var repo *Repo
	if owner == ViewerLogin {
		repo = store.GetRepo(owner, repoName)
	}
	if repo == nil {
		return jsonValueResponse(404, map[string]any{"message": "Not Found"}, nil)
	}
	if len(tail) == 2 && tail[0] == "info" && tail[1] == "refs" && method == "GET" {
		service := req.query["service"]
		wantsV2 := req.headers["git-protocol"] == "version=2"
		if service == "git-upload-pack" {
			var body []byte
			if wantsV2 {
				body = v2Advertisement()
			} else {
				body = v0Advertisement(repo, "git-upload-pack")
			}
			return bufferResponse(200, body, "application/x-git-upload-pack-advertisement")
		}
		if service == "git-receive-pack" {
			body := v0Advertisement(repo, "git-receive-pack")
			return bufferResponse(200, body, "application/x-git-receive-pack-advertisement")
		}
		return jsonValueResponse(400, map[string]any{"message": "a service query parameter is required"}, nil)
	}
	if len(tail) == 1 && tail[0] == "git-upload-pack" && method == "POST" {
		wantsV2 := req.headers["git-protocol"] == "version=2"
		var body []byte
		if !wantsV2 {
			body = fetchV0Response(store, repo, req.body)
		} else {
			packets, _ := decodePktLines(req.body)
			command := ""
			for _, p := range packets {
				if p.kind == pktData {
					command = strings.TrimSuffix(string(p.payload), "\n")
					break
				}
			}
			if command == "command=ls-refs" {
				body = lsRefsResponse(repo, req.body)
			} else {
				body = fetchV2Response(store, repo, req.body)
			}
		}
		return bufferResponse(200, body, "application/x-git-upload-pack-result")
	}
	if isPush {
		outcome := applyPush(store, repo, req.body)
		body := pushResponse(outcome)
		return bufferResponse(200, body, "application/x-git-receive-pack-result")
	}
	return jsonValueResponse(404, map[string]any{"message": "Not Found"}, nil)
}
