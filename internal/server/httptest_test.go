package server

import (
	"testing"

	"github.com/tycheg/reasonr-engine/internal/testmodels"
)

// TestScenariosHTTP: the RFC example tests pass through the HTTP API on
// memory adapters (reasonrd test --driver api).
func TestScenariosHTTP(t *testing.T) {
	for _, mod := range []struct {
		dir     string
		tenants []string
	}{{"taxi", []string{"", "acme"}}, {"bank", []string{""}}} {
		dir := testmodels.Dir(mod.dir)
		base, stop := server(t, "--model", dir, "--test")
		for _, tn := range mod.tenants {
			runHTTP(t, base, dir, tn)
		}
		stop()
	}
}
