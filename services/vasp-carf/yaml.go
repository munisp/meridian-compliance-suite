package main

import (
	"fmt"
	"strconv"
	"strings"
)

// ---------- minimal YAML-subset parser (offline-resilient; no external deps) ----------
// Supports: nested block maps, block lists ("- "), scalars (null/bool/int/float/
// quoted/plain strings), inline flow maps {a: b} and flow lists [a, b], comments.
// Sufficient for the rp-* pack grammar in SPEC §1.4.

type yamlLine struct {
	indent  int
	content string
}

func ParseYAML(doc string) (any, error) {
	var lines []yamlLine
	for _, raw := range strings.Split(doc, "\n") {
		if strings.TrimSpace(raw) == "" || strings.HasPrefix(strings.TrimSpace(raw), "#") {
			continue
		}
		noComment := stripComment(raw)
		if strings.TrimSpace(noComment) == "" {
			continue
		}
		indent := len(noComment) - len(strings.TrimLeft(noComment, " "))
		lines = append(lines, yamlLine{indent, strings.TrimSpace(noComment)})
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("empty document")
	}
	v, n, err := parseBlock(lines, 0, lines[0].indent)
	if err != nil {
		return nil, err
	}
	if n != len(lines) {
		return nil, fmt.Errorf("trailing content at line %d", n)
	}
	return v, nil
}

func stripComment(s string) string {
	inS, inD := false, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			if !inD {
				inS = !inS
			}
		case '"':
			if !inS {
				inD = !inD
			}
		case '#':
			if !inS && !inD && (i == 0 || s[i-1] == ' ') {
				return s[:i]
			}
		}
	}
	return s
}

func parseBlock(lines []yamlLine, i, indent int) (any, int, error) {
	if i >= len(lines) {
		return nil, i, nil
	}
	if strings.HasPrefix(lines[i].content, "- ") || lines[i].content == "-" {
		return parseList(lines, i, indent)
	}
	return parseMap(lines, i, indent)
}

func parseMap(lines []yamlLine, i, indent int) (any, int, error) {
	m := map[string]any{}
	for i < len(lines) {
		ln := lines[i]
		if ln.indent < indent {
			break
		}
		if ln.indent > indent {
			return nil, i, fmt.Errorf("unexpected indent at %q", ln.content)
		}
		if strings.HasPrefix(ln.content, "- ") {
			break
		}
		key, val, ok := splitKV(ln.content)
		if !ok {
			return nil, i, fmt.Errorf("bad map line %q", ln.content)
		}
		i++
		if val != "" {
			v, err := parseScalar(val)
			if err != nil {
				return nil, i, err
			}
			m[key] = v
		} else {
			// nested block or null
			if i < len(lines) && lines[i].indent > indent {
				v, ni, err := parseBlock(lines, i, lines[i].indent)
				if err != nil {
					return nil, ni, err
				}
				m[key] = v
				i = ni
			} else if i < len(lines) && lines[i].indent == indent && (strings.HasPrefix(lines[i].content, "- ") || lines[i].content == "-") {
				v, ni, err := parseList(lines, i, indent)
				if err != nil {
					return nil, ni, err
				}
				m[key] = v
				i = ni
			} else {
				m[key] = nil
			}
		}
	}
	return m, i, nil
}

func parseList(lines []yamlLine, i, indent int) (any, int, error) {
	var out []any
	for i < len(lines) {
		ln := lines[i]
		if ln.indent < indent || (!strings.HasPrefix(ln.content, "- ") && ln.content != "-") {
			break
		}
		if ln.indent > indent {
			return nil, i, fmt.Errorf("unexpected indent in list at %q", ln.content)
		}
		item := strings.TrimSpace(strings.TrimPrefix(ln.content, "-"))
		i++
		if item == "" {
			if i < len(lines) && lines[i].indent > indent {
				v, ni, err := parseBlock(lines, i, lines[i].indent)
				if err != nil {
					return nil, ni, err
				}
				out = append(out, v)
				i = ni
			} else {
				out = append(out, nil)
			}
			continue
		}
		if key, val, ok := splitKV(item); ok && !strings.HasPrefix(item, "{") && !strings.HasPrefix(item, "[") {
			// inline map start: treat rest as map entries at deeper indent
			m := map[string]any{}
			if val != "" {
				v, err := parseScalar(val)
				if err != nil {
					return nil, i, err
				}
				m[key] = v
			} else if i < len(lines) && lines[i].indent > indent {
				v, ni, err := parseBlock(lines, i, lines[i].indent)
				if err != nil {
					return nil, ni, err
				}
				m[key] = v
				i = ni
			} else {
				m[key] = nil
			}
			// consume following deeper-indented map lines
			for i < len(lines) && lines[i].indent > indent && !strings.HasPrefix(lines[i].content, "- ") {
				sub := lines[i]
				k2, v2, ok2 := splitKV(sub.content)
				if !ok2 {
					return nil, i, fmt.Errorf("bad map line %q", sub.content)
				}
				i++
				if v2 != "" {
					v, err := parseScalar(v2)
					if err != nil {
						return nil, i, err
					}
					m[k2] = v
				} else if i < len(lines) && lines[i].indent > sub.indent {
					v, ni, err := parseBlock(lines, i, lines[i].indent)
					if err != nil {
						return nil, ni, err
					}
					m[k2] = v
					i = ni
				} else {
					m[k2] = nil
				}
			}
			out = append(out, m)
			continue
		}
		v, err := parseScalar(item)
		if err != nil {
			return nil, i, err
		}
		out = append(out, v)
	}
	return out, i, nil
}

func splitKV(s string) (key, val string, ok bool) {
	idx := strings.Index(s, ": ")
	if idx < 0 {
		if strings.HasSuffix(s, ":") {
			return strings.TrimSpace(s[:len(s)-1]), "", true
		}
		return "", "", false
	}
	return strings.TrimSpace(s[:idx]), strings.TrimSpace(s[idx+2:]), true
}

func parseScalar(s string) (any, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "null" || s == "~":
		return nil, nil
	case s == "true":
		return true, nil
	case s == "false":
		return false, nil
	case strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}"):
		m := map[string]any{}
		body := strings.TrimSpace(s[1 : len(s)-1])
		if body == "" {
			return m, nil
		}
		for _, part := range splitFlow(body) {
			k, v, ok := splitKV(part)
			if !ok {
				return nil, fmt.Errorf("bad flow map entry %q", part)
			}
			vv, err := parseScalar(v)
			if err != nil {
				return nil, err
			}
			m[k] = vv
		}
		return m, nil
	case strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]"):
		var out []any
		body := strings.TrimSpace(s[1 : len(s)-1])
		if body == "" {
			return out, nil
		}
		for _, part := range splitFlow(body) {
			vv, err := parseScalar(strings.TrimSpace(part))
			if err != nil {
				return nil, err
			}
			out = append(out, vv)
		}
		return out, nil
	case strings.HasPrefix(s, "\"") && strings.HasSuffix(s, "\"") && len(s) >= 2:
		return s[1 : len(s)-1], nil
	case strings.HasPrefix(s, "'") && strings.HasSuffix(s, "'") && len(s) >= 2:
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'"), nil
	}
	if iv, err := strconv.ParseInt(s, 10, 64); err == nil {
		return iv, nil
	}
	if fv, err := strconv.ParseFloat(s, 64); err == nil {
		return fv, nil
	}
	return s, nil
}

func splitFlow(body string) []string {
	var parts []string
	depth := 0
	start := 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(body[start:i]))
				start = i + 1
			}
		}
	}
	parts = append(parts, strings.TrimSpace(body[start:]))
	return parts
}
