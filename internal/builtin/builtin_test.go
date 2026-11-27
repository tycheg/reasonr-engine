package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tycheg/reasonr-engine/internal/adapters/telemetry"
	"github.com/tycheg/reasonr-engine/internal/config"
	"github.com/tycheg/reasonr-engine/internal/registry"
)

func TestLocalKMS(t *testing.T) {
	key := "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	file := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(file, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REASONR_TEST_MASTER", key)
	f, err := registry.KMS("local")
	if err != nil {
		t.Fatal(err)
	}
	d := registry.Deps{Logger: telemetry.Discard(), Test: true}
	var wrapped []byte
	for _, src := range []string{"{ type: local, key: " + key + " }", "{ type: local, key_env: REASONR_TEST_MASTER }", "{ type: local, key_file: " + file + " }"} {
		n, err := config.Parse([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		k, err := f(n, d)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if wrapped == nil {
			wrapped, _ = k.Wrap(context.Background(), "t", []byte("k"))
		}
		if p, err := k.Unwrap(context.Background(), "t", wrapped); err != nil || string(p) != "k" {
			t.Fatalf("%s: different keys: %v", src, err)
		}
	}
	for src, want := range map[string]string{
		"{ type: local }":             "set key",
		"{ type: local, key: abc }":   "must be 32 bytes",
		"{ type: local, key: \"!\" }": "base64",
		"{ type: local, keys: abc }":  "unknown key",
	} {
		n, _ := config.Parse([]byte(src))
		if _, err := f(n, d); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v; want %q", src, err, want)
		}
	}
}
