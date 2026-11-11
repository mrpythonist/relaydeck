// Package jsonpath evaluates small dotted paths against a JSON payload.
//
// It is used for destination JSON-path filters and header injection
// templates. The syntax is deliberately small: dot-separated field names
// with optional array indexes, for example:
//
//	type
//	data.object.status
//	items[0].id
//	$.type            (a leading "$." is accepted and ignored)
//
// A filter expression is "path=expected", for example
// "data.object.status=succeeded". The event matches when the path
// resolves and the value, rendered as a string, equals the expected
// value. A path that does not resolve, a non-JSON payload, or a
// malformed expression never matches.
//
// Templates use {{path}} placeholders, for example "X-Event-Type:
// {{.type}}". A placeholder whose path does not resolve renders as an
// empty string.
package jsonpath

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Lookup resolves path against a JSON payload. It returns the value and
// whether the path resolved. Numbers come back as json.Number so callers
// can render them without float formatting surprises.
func Lookup(payload []byte, path string) (any, bool) {
	path = strings.TrimSpace(path)
	path = strings.TrimPrefix(path, "$.")
	path = strings.TrimPrefix(path, "$")
	// Templates write {{.type}}; a leading dot is the same as no prefix.
	path = strings.TrimPrefix(path, ".")
	if path == "" {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, false
	}
	cur := root
	for _, seg := range strings.Split(path, ".") {
		if seg == "" {
			return nil, false
		}
		// Split off a trailing [n] index, if any.
		name := seg
		index := -1
		if i := strings.Index(seg, "["); i >= 0 {
			if !strings.HasSuffix(seg, "]") {
				return nil, false
			}
			name = seg[:i]
			n, err := strconv.Atoi(seg[i+1 : len(seg)-1])
			if err != nil || n < 0 {
				return nil, false
			}
			index = n
		}
		if name != "" {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil, false
			}
			v, ok := m[name]
			if !ok {
				return nil, false
			}
			cur = v
		}
		if index >= 0 {
			arr, ok := cur.([]any)
			if !ok || index >= len(arr) {
				return nil, false
			}
			cur = arr[index]
		}
	}
	return cur, true
}

// stringify renders a looked-up value the way filters and templates
// compare it: strings as-is, numbers and booleans as JSON, null as "null".
func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// Match reports whether payload satisfies the filter expression
// "path=expected". Anything malformed or unresolvable returns false.
func Match(payload []byte, expr string) bool {
	i := strings.Index(expr, "=")
	if i <= 0 {
		return false
	}
	path := strings.TrimSpace(expr[:i])
	expected := strings.TrimSpace(expr[i+1:])
	if path == "" {
		return false
	}
	v, ok := Lookup(payload, path)
	if !ok {
		return false
	}
	return stringify(v) == expected
}

// RenderTemplate replaces {{path}} placeholders in template with values
// looked up from payload. Placeholders that do not resolve render as an
// empty string. A "{{" with no closing "}}" is left as-is.
func RenderTemplate(payload []byte, template string) string {
	var b strings.Builder
	rest := template
	for {
		start := strings.Index(rest, "{{")
		if start < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:start])
		end := strings.Index(rest[start+2:], "}}")
		if end < 0 {
			b.WriteString(rest[start:])
			break
		}
		path := strings.TrimSpace(rest[start+2 : start+2+end])
		if v, ok := Lookup(payload, path); ok {
			b.WriteString(stringify(v))
		}
		rest = rest[start+2+end+2:]
	}
	return b.String()
}
