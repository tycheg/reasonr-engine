package server

import (
	"testing"

	"github.com/tycheg/reasonr-engine/internal/servertest"
)

// server runs reasonrd and returns its address and the stop function.
func server(t *testing.T, args ...string) (string, func()) {
	t.Helper()
	return servertest.Start(t, runWith, args...)
}

func call(t *testing.T, base, method, path, who, body string) (int, string) {
	t.Helper()
	return servertest.Call(t, base, method, path, who, body)
}

func runHTTP(t *testing.T, base, dir, tenant string) {
	t.Helper()
	servertest.Scenarios(t, base, dir, tenant)
}
