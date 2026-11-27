// Package fakeadapter is a third-party adapter for the reasonrd build test:
// a KMS of type: fake that sees the engine only through port and registry,
// as every adapter does.
package fakeadapter

import (
	"bytes"
	"context"
	"errors"

	"github.com/tycheg/reasonr-engine/port"
	"github.com/tycheg/reasonr-engine/port/registry"
)

func init() {
	registry.RegisterKMS("fake", func(n *port.Node, _ registry.Deps) (port.KMS, error) {
		return kms{}, n.Only("type")
	})
}

// kms "wraps" a key by prefixing it with its context: a test, not a secret.
type kms struct{}

func (kms) Wrap(_ context.Context, keyContext string, plain []byte) ([]byte, error) {
	return append([]byte(keyContext+":"), plain...), nil
}

func (kms) Unwrap(_ context.Context, keyContext string, wrapped []byte) ([]byte, error) {
	plain, ok := bytes.CutPrefix(wrapped, []byte(keyContext+":"))
	if !ok {
		return nil, errors.New("fake kms: another key context")
	}
	return plain, nil
}
