package host_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tycheg/reasonr-engine/internal/adapters/auth"
	"github.com/tycheg/reasonr-engine/internal/config"
	"github.com/tycheg/reasonr-engine/internal/host"
)

// TestBuildSigner: integrations.auth — the signing key from a file or an
// environment variable, kid, issuer, public keys also in JWKS; without the
// section a key is created only in test mode.
func TestBuildSigner(t *testing.T) {
	dir := t.TempDir()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	priv := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	next, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	pubDER, _ := x509.MarshalPKIXPublicKey(&next.PublicKey)
	pubFile := filepath.Join(dir, "next.pem")
	if err := os.WriteFile(pubFile, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENGINE_KEY", priv)
	t.Setenv("PUBLIC_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})))
	node := func(yaml string) *config.Node {
		t.Helper()
		c, err := config.Decode([]byte("integrations:\n  auth: " + yaml + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		return c.IntegrationsAuth
	}
	s, err := host.BuildSigner(node(`{ key_env: ENGINE_KEY, kid: k1, issuer: eng, also: [{ key_file: "`+pubFile+`", kid: k2 }] }`), false)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := auth.ParseJWKS(s.JWKS())
	if err != nil || s.Issuer != "eng" || len(keys) != 2 || !keys["k1"].(*ecdsa.PublicKey).Equal(&key.PublicKey) ||
		!keys["k2"].(*ecdsa.PublicKey).Equal(&next.PublicKey) {
		t.Fatalf("signer: issuer %q, keys %v, %v", s.Issuer, keys, err)
	}
	if s, err := host.BuildSigner(nil, false); s != nil || err != nil {
		t.Fatalf("without integrations.auth outside --test: %v %v", s, err)
	}
	if s, err := host.BuildSigner(nil, true); s == nil || err != nil || s.Issuer != host.DefaultIssuer {
		t.Fatalf("without integrations.auth in --test: %v %v", s, err)
	}
	for yaml, want := range map[string]string{
		`{ kid: k1 }`:                               "set key_file or key_env",
		`{ key_env: NOPE }`:                         "NOPE is empty",
		`{ key_env: PUBLIC_KEY }`:                   "private signing key",
		`{ key_env: ENGINE_KEY, key_file: x }`:      "not both",
		`{ key_file: "` + dir + `/nope.pem" }`:      "nope.pem",
		`{ key_env: ENGINE_KEY, also: x }`:          "expected a list",
		`{ key_env: ENGINE_KEY, also: [{ x: 1 }] }`: `unknown key "x"`,
	} {
		if _, err := host.BuildSigner(node(yaml), false); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", yaml, err, want)
		}
	}
}
