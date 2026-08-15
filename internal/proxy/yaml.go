package proxy

import (
	"fmt"
	"strconv"
	"strings"
)

// --- minimal YAML parser (just enough for our config) ---

// parseYAML returns a YAML document as nested map[string]any / []any.
// Supports scalars (string, int, bool), sequences (`- ...`), and maps
// (key: value). Indentation groups children. Comments (`#`) and blank
// lines are ignored. Strings may be unquoted or quoted with " or '.
func ParseYAML(src string) (map[string]any, error) {
	lines := prepYAMLLines(src)
	root := map[string]any{}
	if err := parseYAMLLevel(lines, 0, 0, root); err != nil {
		return nil, err
	}
	return root, nil
}

func prepYAMLLines(src string) []string {
	out := []string{}
	for _, raw := range strings.Split(src, "\n") {
		// strip comments outside of quoted strings (rough; works for our config)
		s := stripYAMLComment(raw)
		// trim trailing whitespace
		s = strings.TrimRight(s, " \t\r")
		out = append(out, s)
	}
	return out
}

func stripYAMLComment(line string) string {
	inS, inD := false, false
	for i, r := range line {
		switch r {
		case '\'':
			if !inD {
				inS = !inS
			}
		case '"':
			if !inS {
				inD = !inD
			}
		case '#':
			if !inS && !inD && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
				return line[:i]
			}
		}
	}
	return line
}

func indentOf(s string) int {
	for i, r := range s {
		if r != ' ' && r != '\t' {
			return i
		}
	}
	return len(s)
}

func parseYAMLLevel(lines []string, start, baseIndent int, out map[string]any) error {
	i := start
	for i < len(lines) {
		raw := lines[i]
		if strings.TrimSpace(raw) == "" {
			i++
			continue
		}
		ind := indentOf(raw)
		if ind < baseIndent {
			return nil // caller handles the parent
		}
		if ind > baseIndent {
			return fmt.Errorf("line %d: unexpected indent", i+1)
		}
		body := raw[ind:]
		// list item
		if strings.HasPrefix(body, "- ") || body == "-" {
			// sequences are returned separately; callers should use a []any
			return fmt.Errorf("line %d: sequences must be passed via parseYAMLSeq", i+1)
		}
		// key: value
		idx := strings.Index(body, ":")
		if idx < 0 {
			return fmt.Errorf("line %d: missing ':'", i+1)
		}
		key := strings.TrimSpace(body[:idx])
		val := strings.TrimSpace(body[idx+1:])
		// sequence under this key
		if val == "" {
			// either a nested map or a sequence
			// Peek: find next non-empty line; if indent > baseIndent+0 → child
			// of this key. If next non-empty line starts with "- ", it's a
			// sequence; otherwise a map.
			j := i + 1
			for j < len(lines) && strings.TrimSpace(lines[j]) == "" {
				j++
			}
			if j >= len(lines) {
				out[key] = map[string]any{}
				i = j
				continue
			}
			nextIndent := indentOf(lines[j])
			if nextIndent <= baseIndent {
				out[key] = map[string]any{}
				i = j
				continue
			}
			trimmed := strings.TrimSpace(lines[j])
			if strings.HasPrefix(trimmed, "- ") || trimmed == "-" {
				seq, consumed := parseYAMLSeq(lines, j, nextIndent)
				out[key] = seq
				i = consumed
				continue
			}
			child := map[string]any{}
			if err := parseYAMLLevel(lines, j, nextIndent, child); err != nil {
				return err
			}
			out[key] = child
			// advance past consumed child
			k := j
			for k < len(lines) {
				if strings.TrimSpace(lines[k]) == "" {
					k++
					continue
				}
				if indentOf(lines[k]) < nextIndent {
					break
				}
				k++
			}
			i = k
			continue
		}
		// inline scalar
		if val == "|" || val == ">" {
			return fmt.Errorf("line %d: block scalars (|/>) not supported", i+1)
		}
		out[key] = parseYAMLScalar(val)
		i++
	}
	return nil
}

func parseYAMLSeq(lines []string, start, baseIndent int) ([]any, int) {
	seq := []any{}
	i := start
	for i < len(lines) {
		raw := lines[i]
		if strings.TrimSpace(raw) == "" {
			i++
			continue
		}
		ind := indentOf(raw)
		if ind < baseIndent {
			return seq, i
		}
		if ind > baseIndent {
			return seq, i // malformed
		}
		body := strings.TrimSpace(raw[ind:])
		if !strings.HasPrefix(body, "- ") && body != "-" {
			return seq, i
		}
		payload := strings.TrimSpace(strings.TrimPrefix(body, "-"))
		if payload == "" {
			// sequence of maps — parse next indented block as a map for this item
			j := i + 1
			for j < len(lines) && strings.TrimSpace(lines[j]) == "" {
				j++
			}
			if j < len(lines) {
				nextIndent := indentOf(lines[j])
				if nextIndent > baseIndent {
					child := map[string]any{}
					k := j
					for k < len(lines) {
						if strings.TrimSpace(lines[k]) == "" {
							k++
							continue
						}
						if indentOf(lines[k]) < nextIndent {
							break
						}
						k++
					}
					if err := parseYAMLLevel(lines, j, nextIndent, child); err == nil {
						seq = append(seq, child)
					}
					i = k
					continue
				}
			}
			seq = append(seq, map[string]any{})
			i = j
			continue
		}
		// map item: "- key: value" first line
		if strings.Contains(payload, ":") {
			child := map[string]any{}
			idx := strings.Index(payload, ":")
			k := strings.TrimSpace(payload[:idx])
			v := strings.TrimSpace(payload[idx+1:])
			child[k] = parseYAMLScalar(v)
			// peek next lines for additional keys at same indent
			j := i + 1
			for j < len(lines) {
				raw2 := lines[j]
				if strings.TrimSpace(raw2) == "" {
					j++
					continue
				}
				ind2 := indentOf(raw2)
				if ind2 <= baseIndent {
					break
				}
				if ind2 != baseIndent+2 {
					break
				}
				body2 := strings.TrimSpace(raw2[ind2:])
				if strings.HasPrefix(body2, "- ") {
					break
				}
				idx2 := strings.Index(body2, ":")
				if idx2 < 0 {
					break
				}
				k2 := strings.TrimSpace(body2[:idx2])
				v2 := strings.TrimSpace(body2[idx2+1:])
				child[k2] = parseYAMLScalar(v2)
				j++
			}
			seq = append(seq, child)
			i = j
			continue
		}
		seq = append(seq, parseYAMLScalar(payload))
		i++
	}
	return seq, i
}

func parseYAMLScalar(s string) any {
	unq := strings.Trim(s, "\"'")
	if s == "" {
		return ""
	}
	if s == "true" {
		return true
	}
	if s == "false" {
		return false
	}
	if s == "null" || s == "~" {
		return nil
	}
	if i, err := strconv.ParseInt(unq, 10, 64); err == nil && !strings.ContainsAny(unq, ".eE") {
		return i
	}
	if f, err := strconv.ParseFloat(unq, 64); err == nil {
		return f
	}
	return unq
}
