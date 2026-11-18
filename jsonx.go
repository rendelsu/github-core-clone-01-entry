// jsonx.go — JSON helpers. All output uses an encoder with HTML escaping
// off, matching JSON.stringify's treatment of <, > and &.

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
)

func marshalJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	// Encode appends a trailing newline; drop it.
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b
}

// parseJSONBody decodes a request body. An empty body decodes as null, like
// the reference implementation; malformed JSON fails.
func parseJSONBody(raw []byte) (any, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	return normalizeNumbers(v), true
}

// normalizeNumbers converts json.Number values to float64 so handlers see
// the same numeric type JSON.parse produces in the reference implementation.
func normalizeNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return 0.0
		}
		return f
	case map[string]any:
		for k, child := range t {
			t[k] = normalizeNumbers(child)
		}
		return t
	case []any:
		for i, child := range t {
			t[i] = normalizeNumbers(child)
		}
		return t
	default:
		return v
	}
}

func isRecord(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

func asString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func asFloat(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

func globalID(kind string, parts ...any) string {
	s := kind
	for _, p := range parts {
		s += ":"
		switch v := p.(type) {
		case string:
			s += v
		case int:
			s += itoa(v)
		case int64:
			s += itoa64(v)
		}
	}
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
