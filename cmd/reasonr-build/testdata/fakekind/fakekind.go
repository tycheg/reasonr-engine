// Package fakekind is an engine kind plugin for the reasonrd build test: the
// test double of the stateless kind (kindtest.UseKind), so that a binary
// built from the engine alone runs a model.
package fakekind

import "github.com/tycheg/reasonr-engine/kind/kindtest"

func init() { kindtest.UseKind() }
