// Package dupadapter registers a KMS under the name of fakeadapter's: the
// builder's test loads it as a plugin into a binary that links
// fakeadapter, and the second registration is refused.
package dupadapter

import (
	"errors"

	"github.com/tycheg/reasonr-engine/port"
	"github.com/tycheg/reasonr-engine/port/registry"
)

func init() {
	registry.RegisterKMS("fake", func(*port.Node, registry.Deps) (port.KMS, error) {
		return nil, errors.New("dupadapter: not a KMS")
	})
}
