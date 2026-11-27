package server

import "github.com/tycheg/reasonr-engine/kind/kindtest"

// The engine's tests run on the test kind of porttest: the stateless kind is
// another module (reasonr-stateless).
func init() { kindtest.UseKind() }
