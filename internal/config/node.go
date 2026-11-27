// Package config is the engine configuration engine.yaml (RFC §31.1,
// §30–31): parsing, checking the structure, and the component settings
// nodes that the adapter factories get (port.Node).
package config

import (
	"fmt"

	"github.com/tycheg/reasonr-engine/port"
)

// The settings node is the contract's (port.Node): engine.yaml is a tree
// of them.
type (
	Node  = port.Node
	Kind  = port.NodeKind
	Error = port.NodeError
)

// The node kinds.
const (
	Null   = port.NodeNull
	Scalar = port.NodeScalar
	Map    = port.NodeMap
	List   = port.NodeList
)

// NewMap returns a map of key, string value pairs.
func NewMap(kv ...string) *Node { return port.NewNodeMap(kv...) }

// Parse parses a YAML document.
func Parse(data []byte) (*Node, error) { return port.ParseNode(data) }

func errAt(line int, format string, args ...any) error {
	return &Error{Line: line, Msg: fmt.Sprintf(format, args...)}
}
