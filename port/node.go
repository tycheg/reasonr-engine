// The settings node: a section of engine.yaml an adapter factory gets.
// The contract does not depend on external libraries, so YAML is read by
// its own parser of the subset the configuration needs: block maps and
// sequences, one-line flow {…} and […], strings in double and single
// quotes, # comments. No anchors, multiple documents, multi-line scalars
// or tabs.

package port

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// NodeKind is the kind of a settings node.
type NodeKind uint8

const (
	NodeNull NodeKind = iota
	NodeScalar
	NodeMap
	NodeList
)

// Node is a YAML node. A map keeps its keys in file order.
type Node struct {
	Kind   NodeKind
	Value  string // a scalar
	Keys   []string
	Values []*Node // the values of map keys or the list items
	Line   int
}

// NewNodeMap returns a map of key, string value pairs.
func NewNodeMap(kv ...string) *Node {
	n := &Node{Kind: NodeMap}
	for i := 0; i+1 < len(kv); i += 2 {
		n.Set(kv[i], &Node{Kind: NodeScalar, Value: kv[i+1]})
	}
	return n
}

// Set sets the value of a map key.
func (n *Node) Set(key string, v *Node) {
	for i, k := range n.Keys {
		if k == key {
			n.Values[i] = v
			return
		}
	}
	n.Keys, n.Values = append(n.Keys, key), append(n.Values, v)
}

// Delete removes a map key.
func (n *Node) Delete(key string) {
	for i, k := range n.Keys {
		if k == key {
			n.Keys = append(n.Keys[:i:i], n.Keys[i+1:]...)
			n.Values = append(n.Values[:i:i], n.Values[i+1:]...)
			return
		}
	}
}

// Get returns the value of a map key or nil.
func (n *Node) Get(key string) *Node {
	if n == nil || n.Kind != NodeMap {
		return nil
	}
	for i, k := range n.Keys {
		if k == key {
			return n.Values[i]
		}
	}
	return nil
}

// NodeError is a settings error with a line number.
type NodeError struct {
	Line int
	Msg  string
}

func (e *NodeError) Error() string {
	if e.Line == 0 {
		return "engine.yaml: " + e.Msg
	}
	return fmt.Sprintf("engine.yaml:%d: %s", e.Line, e.Msg)
}

func errAt(line int, format string, args ...any) error {
	return &NodeError{Line: line, Msg: fmt.Sprintf(format, args...)}
}

// String returns the string value of a key; no key — def.
func (n *Node) String(key, def string) (string, error) {
	v := n.Get(key)
	switch {
	case v == nil || v.Kind == NodeNull:
		return def, nil
	case v.Kind != NodeScalar:
		return "", errAt(v.Line, "%s: expected a string", key)
	}
	return v.Value, nil
}

// Int returns the integer value of a key; no key — def.
func (n *Node) Int(key string, def int) (int, error) {
	s, err := n.String(key, "")
	if err != nil || s == "" {
		return def, err
	}
	x, err := strconv.Atoi(s)
	if err != nil {
		return 0, errAt(n.Get(key).Line, "%s: expected an integer, not %q", key, s)
	}
	return x, nil
}

// Bool returns the true/false of a key; no key — def.
func (n *Node) Bool(key string, def bool) (bool, error) {
	s, err := n.String(key, "")
	if err != nil || s == "" {
		return def, err
	}
	switch s {
	case "true", "yes", "on":
		return true, nil
	case "false", "no", "off":
		return false, nil
	}
	return false, errAt(n.Get(key).Line, "%s: expected true or false, not %q", key, s)
}

// Duration returns the duration of a key (2s, 24h, 500ms); no key — def.
func (n *Node) Duration(key string, def time.Duration) (time.Duration, error) {
	s, err := n.String(key, "")
	if err != nil || s == "" {
		return def, err
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, errAt(n.Get(key).Line, "%s: expected a duration, not %q", key, s)
	}
	return d, nil
}

// Strings returns the string list of a key (or one string as a list).
func (n *Node) Strings(key string) ([]string, error) {
	v := n.Get(key)
	switch {
	case v == nil || v.Kind == NodeNull:
		return nil, nil
	case v.Kind == NodeScalar:
		return []string{v.Value}, nil
	case v.Kind != NodeList:
		return nil, errAt(v.Line, "%s: expected a list of strings", key)
	}
	out := make([]string, len(v.Values))
	for i, x := range v.Values {
		if x.Kind != NodeScalar {
			return nil, errAt(x.Line, "%s: expected a list of strings", key)
		}
		out[i] = x.Value
	}
	return out, nil
}

// Only checks that a map has no keys besides the listed ones: a typo in the
// configuration is an error, not a silently ignored setting.
func (n *Node) Only(keys ...string) error {
	if n == nil || n.Kind != NodeMap {
		return nil
	}
next:
	for i, k := range n.Keys {
		for _, x := range keys {
			if k == x {
				continue next
			}
		}
		return errAt(n.Values[i].Line, "unknown key %q (known: %s)", k, strings.Join(keys, ", "))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Parsing.

type line struct {
	no     int
	indent int
	text   string
}

// ParseNode parses a YAML document.
func ParseNode(data []byte) (*Node, error) {
	var lines []line
	for i, raw := range strings.Split(string(data), "\n") {
		raw = strings.TrimRight(raw, "\r")
		text := stripComment(raw)
		if strings.TrimSpace(text) == "" {
			continue
		}
		indent := len(text) - len(strings.TrimLeft(text, " "))
		if strings.HasPrefix(strings.TrimLeft(text, " "), "\t") {
			return nil, errAt(i+1, "a tab in the indent")
		}
		lines = append(lines, line{no: i + 1, indent: indent, text: strings.TrimSpace(text)})
	}
	if len(lines) == 0 {
		return &Node{Kind: NodeMap}, nil
	}
	p := &parser{lines: lines}
	n, err := p.block(lines[0].indent)
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.lines) {
		return nil, errAt(p.lines[p.pos].no, "wrong indent")
	}
	return n, nil
}

// stripComment removes a # comment outside quotes (# at the start or after
// a space).
func stripComment(s string) string {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' {
				i++
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#' && (i == 0 || s[i-1] == ' '):
			return s[:i]
		}
	}
	return s
}

type parser struct {
	lines []line
	pos   int
}

// block parses a block of lines with the indent indent: a list, a map or a
// single scalar.
func (p *parser) block(indent int) (*Node, error) {
	l := p.lines[p.pos]
	if l.text == "-" || strings.HasPrefix(l.text, "- ") {
		return p.list(indent)
	}
	if _, _, ok := splitKey(l.text); ok {
		return p.mapping(indent)
	}
	p.pos++
	return parseValue(l.text, l.no)
}

func (p *parser) list(indent int) (*Node, error) {
	n := &Node{Kind: NodeList, Line: p.lines[p.pos].no}
	for p.pos < len(p.lines) {
		l := &p.lines[p.pos]
		if l.indent < indent {
			break
		}
		isItem := l.text == "-" || strings.HasPrefix(l.text, "- ")
		if l.indent > indent {
			return nil, errAt(l.no, `expected a list item "- …"`)
		}
		if !isItem {
			break // the list under a key with the same indent has ended
		}
		if l.text == "-" {
			p.pos++
			item, err := p.nested(indent, l.no)
			if err != nil {
				return nil, err
			}
			n.Values = append(n.Values, item)
			continue
		}
		// "- key: value": the item starts on the same line, and the line
		// becomes the first line of the nested block.
		rest := strings.TrimLeft(l.text[1:], " ")
		l.indent += len(l.text) - len(rest)
		l.text = rest
		item, err := p.block(l.indent)
		if err != nil {
			return nil, err
		}
		n.Values = append(n.Values, item)
	}
	return n, nil
}

func (p *parser) mapping(indent int) (*Node, error) {
	n := &Node{Kind: NodeMap, Line: p.lines[p.pos].no}
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, errAt(l.no, "wrong indent")
		}
		key, value, ok := splitKey(l.text)
		if !ok {
			return nil, errAt(l.no, `expected "key: value"`)
		}
		for _, k := range n.Keys {
			if k == key {
				return nil, errAt(l.no, "key %q is repeated", key)
			}
		}
		p.pos++
		var v *Node
		var err error
		if value == "" {
			v, err = p.nested(indent, l.no)
		} else {
			v, err = parseValue(value, l.no)
		}
		if err != nil {
			return nil, err
		}
		n.Keys = append(n.Keys, key)
		n.Values = append(n.Values, v)
	}
	return n, nil
}

// nested parses the nested block after "key:" or "-"; a list in a map may
// have the same indent as the key.
func (p *parser) nested(indent, no int) (*Node, error) {
	if p.pos >= len(p.lines) {
		return &Node{Kind: NodeNull, Line: no}, nil
	}
	next := p.lines[p.pos]
	isItem := next.text == "-" || strings.HasPrefix(next.text, "- ")
	if next.indent > indent || next.indent == indent && isItem {
		return p.block(next.indent)
	}
	return &Node{Kind: NodeNull, Line: no}, nil
}

// splitKey splits "key: value" at the first ": " or ":" at the end outside
// quotes and brackets.
func splitKey(s string) (key, value string, ok bool) {
	if s == "" || s[0] == '{' || s[0] == '[' {
		return "", "", false
	}
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == ':' && (i+1 == len(s) || s[i+1] == ' '):
			k := strings.TrimSpace(s[:i])
			if uq, err := unquote(k); err == nil {
				k = uq
			}
			return k, strings.TrimSpace(s[i+1:]), k != ""
		}
	}
	return "", "", false
}

// parseValue parses the value on a line: a flow {…}, […] or a scalar.
func parseValue(s string, no int) (*Node, error) {
	f := &flow{s: s, no: no}
	n, err := f.value()
	if err != nil {
		return nil, err
	}
	f.ws()
	if f.pos < len(f.s) {
		return nil, errAt(no, "extra text after the value: %q", f.s[f.pos:])
	}
	return n, nil
}

type flow struct {
	s   string
	pos int
	no  int
}

func (f *flow) ws() {
	for f.pos < len(f.s) && f.s[f.pos] == ' ' {
		f.pos++
	}
}

func (f *flow) value() (*Node, error) {
	f.ws()
	if f.pos >= len(f.s) {
		return &Node{Kind: NodeNull, Line: f.no}, nil
	}
	switch f.s[f.pos] {
	case '{':
		return f.collection('}')
	case '[':
		return f.collection(']')
	}
	return f.scalar(false)
}

// scalar reads a scalar; inFlow — inside {…} or […], where , ] } end it.
func (f *flow) scalar(inFlow bool) (*Node, error) {
	f.ws()
	start := f.pos
	if f.pos < len(f.s) && (f.s[f.pos] == '"' || f.s[f.pos] == '\'') {
		q := f.s[f.pos]
		f.pos++
		for f.pos < len(f.s) {
			c := f.s[f.pos]
			if c == q && q == '\'' && f.pos+1 < len(f.s) && f.s[f.pos+1] == '\'' {
				f.pos += 2 // '' inside '…'
				continue
			}
			if c == q {
				break
			}
			if q == '"' && c == '\\' {
				f.pos++
			}
			f.pos++
		}
		if f.pos >= len(f.s) {
			return nil, errAt(f.no, "unclosed string")
		}
		f.pos++
		v, err := unquote(f.s[start:f.pos])
		if err != nil {
			return nil, errAt(f.no, "%v", err)
		}
		return &Node{Kind: NodeScalar, Value: v, Line: f.no}, nil
	}
	for f.pos < len(f.s) {
		c := f.s[f.pos]
		if inFlow && (c == ',' || c == ']' || c == '}' || c == ':' && (f.pos+1 == len(f.s) || f.s[f.pos+1] == ' ')) {
			break
		}
		f.pos++
	}
	v := strings.TrimSpace(f.s[start:f.pos])
	if v == "null" || v == "~" || v == "" {
		return &Node{Kind: NodeNull, Line: f.no}, nil
	}
	return &Node{Kind: NodeScalar, Value: v, Line: f.no}, nil
}

func (f *flow) collection(end byte) (*Node, error) {
	n := &Node{Kind: NodeList, Line: f.no}
	if end == '}' {
		n.Kind = NodeMap
	}
	f.pos++
	for {
		f.ws()
		if f.pos < len(f.s) && f.s[f.pos] == end {
			f.pos++
			return n, nil
		}
		if n.Kind == NodeMap {
			k, err := f.scalar(true)
			if err != nil {
				return nil, err
			}
			f.ws()
			if k.Kind != NodeScalar || f.pos >= len(f.s) || f.s[f.pos] != ':' {
				return nil, errAt(f.no, `expected "key: value" in {…}`)
			}
			f.pos++
			n.Keys = append(n.Keys, k.Value)
		}
		v, err := f.item()
		if err != nil {
			return nil, err
		}
		n.Values = append(n.Values, v)
		f.ws()
		switch {
		case f.pos < len(f.s) && f.s[f.pos] == ',':
			f.pos++
		case f.pos < len(f.s) && f.s[f.pos] == end:
		default:
			return nil, errAt(f.no, `expected "," or "%c"`, end)
		}
	}
}

func (f *flow) item() (*Node, error) {
	f.ws()
	if f.pos < len(f.s) && (f.s[f.pos] == '{' || f.s[f.pos] == '[') {
		end := byte('}')
		if f.s[f.pos] == '[' {
			end = ']'
		}
		return f.collection(end)
	}
	return f.scalar(true)
}

// unquote removes the quotes: "…" with Go escapes, '…' with ” → '.
func unquote(s string) (string, error) {
	switch {
	case len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"':
		return strconv.Unquote(s)
	case len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'':
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'"), nil
	case len(s) > 0 && (s[0] == '"' || s[0] == '\''):
		return "", fmt.Errorf("unclosed string %s", s)
	}
	return s, nil
}
