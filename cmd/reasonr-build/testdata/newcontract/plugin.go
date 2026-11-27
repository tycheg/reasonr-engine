// Package newcontract is a plugin of another version of the contract for
// the builder's test: the builder refuses it.
package newcontract

import "github.com/tycheg/reasonr-engine/port/registry"

func init() { registry.RegisterCommand("newcontract", nil) }
