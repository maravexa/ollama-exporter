package config

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
)

// node is a parsed YAML element. Exactly one of scalar, mapVal, or listVal
// is non-nil for any constructed node.
type node struct {
	scalar  *string
	mapVal  []mapEntry // ordered keys
	listVal []*node
}

type mapEntry struct {
	key   string
	value *node
}

func (n *node) isMap() bool  { return n.mapVal != nil }
func (n *node) isList() bool { return n.listVal != nil }

// asString returns the scalar value, or empty string if n is not a scalar.
func (n *node) asString() string {
	if n == nil || n.scalar == nil {
		return ""
	}
	return *n.scalar
}

// parseYAMLTree builds a typed tree from a minimal YAML subset:
//
//   - 2-space indentation only.
//   - Scalars (`key: value`), map keys (`key:`), list items (`- value` or
//     `- key: value` to start an inline object).
//   - Inline comments after ` #`, full-line `#` comments, and document
//     separator `---` are stripped/skipped.
//   - Inline lists, anchors, multi-doc, and flow-style maps are NOT
//     supported. (We don't need them and they'd quietly hide errors.)
//
// This parser exists because the exporter has a deliberate
// minimal-dependency policy: pulling go-yaml v3 doubles the binary's
// transitive surface for negligible feature gain over our schema.
func parseYAMLTree(data []byte) (*node, error) {
	type frame struct {
		indent int
		n      *node
	}

	root := &node{mapVal: []mapEntry{}}
	stack := []frame{{indent: -2, n: root}}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	lineNo := 0

	for scanner.Scan() {
		lineNo++
		raw := scanner.Text()

		if ci := strings.Index(raw, " #"); ci >= 0 {
			raw = raw[:ci]
		}
		if strings.HasPrefix(strings.TrimSpace(raw), "#") {
			continue
		}
		trimmed := strings.TrimRight(raw, " \t")
		if strings.TrimSpace(trimmed) == "" || strings.TrimSpace(trimmed) == "---" {
			continue
		}

		indent := 0
		for indent < len(trimmed) && trimmed[indent] == ' ' {
			indent++
		}
		if indent%2 != 0 {
			return nil, fmt.Errorf("line %d: indent must be a multiple of 2 spaces", lineNo)
		}
		body := trimmed[indent:]

		// Pop stack until top frame is the parent of this indent.
		for len(stack) > 1 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		parent := &stack[len(stack)-1]

		// List item.
		if strings.HasPrefix(body, "- ") || body == "-" {
			itemBody := ""
			if body != "-" {
				itemBody = strings.TrimSpace(body[2:])
			}
			if !parent.n.isList() {
				if parent.n.isMap() {
					return nil, fmt.Errorf("line %d: list item under non-list parent", lineNo)
				}
				parent.n.listVal = []*node{}
			}

			itemNode := &node{}
			if itemBody == "" {
				itemNode.mapVal = []mapEntry{}
			} else if k, v, ok := splitKV(itemBody); ok {
				itemNode.mapVal = []mapEntry{{key: k, value: scalarFromString(v)}}
				stack = append(stack, frame{indent: indent, n: itemNode})
			} else {
				itemNode.scalar = stringPtr(unquoteYAML(itemBody))
			}
			parent.n.listVal = append(parent.n.listVal, itemNode)
			continue
		}

		// Map entry.
		k, v, ok := splitKV(body)
		if !ok {
			return nil, fmt.Errorf("line %d: cannot parse %q", lineNo, body)
		}
		if !parent.n.isMap() {
			if parent.n.isList() {
				return nil, fmt.Errorf("line %d: map key under list parent (missing '- '?)", lineNo)
			}
			parent.n.mapVal = []mapEntry{}
		}

		var child *node
		if v == "" {
			// Map key opens a sub-block — type unknown until next line.
			child = &node{}
		} else {
			child = scalarFromString(v)
		}
		parent.n.mapVal = append(parent.n.mapVal, mapEntry{key: k, value: child})
		if v == "" {
			stack = append(stack, frame{indent: indent, n: child})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return root, nil
}

// splitKV splits "key: value" on the first colon. value may be empty.
// Returns ok=false if the line has no colon.
func splitKV(s string) (string, string, bool) {
	ci := strings.Index(s, ":")
	if ci < 0 {
		return "", "", false
	}
	return strings.TrimSpace(s[:ci]), strings.TrimSpace(s[ci+1:]), true
}

func scalarFromString(v string) *node {
	s := unquoteYAML(v)
	return &node{scalar: &s}
}

func stringPtr(s string) *string { return &s }

// unquoteYAML removes surrounding "..." or '...' quotes.
func unquoteYAML(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
