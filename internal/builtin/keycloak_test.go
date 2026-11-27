package builtin

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/config"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/internal/registry"
	"github.com/tycheg/reasonr-engine/internal/testmodels"
	"github.com/tycheg/reasonr-engine/port"
)

// TestJWTKeycloak: jwt with the Keycloak settings of the auth module's
// README (the realm as the issuer, its JWKS, actor Auth.User from sub, the
// realm_access.roles roles) accepts a realm token (RS256) of the taxi model,
// which embeds the module.
func TestJWTKeycloak(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.RawURLEncoding.EncodeToString
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/realms/taxi/protocol/openid-connect/certs" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	defer srv.Close()
	realm := srv.URL + "/realms/taxi"

	mach, err := reasonr.Build(testmodels.Dir("taxi"), reasonr.Options{Test: true})
	if err != nil {
		t.Fatal(err)
	}
	programs := kernel.NewPrograms()
	programs.Set(port.DefaultTenant, mach)
	n, err := config.Parse([]byte("type: jwt\nissuer: " + realm + "\njwks: " + realm + "/protocol/openid-connect/certs\naudience: taxi-web\n" +
		"actor: { claim: sub, type: Auth.User }\nroles: { claim: realm_access.roles }\n"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := registry.Auth("jwt")
	if err != nil {
		t.Fatal(err)
	}
	_, j, err := f(n, registry.Deps{Catalog: programs})
	if err != nil {
		t.Fatal(err)
	}

	sign := func(claims map[string]any) string {
		head, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "k1"})
		body, _ := json.Marshal(claims)
		in := b64(head) + "." + b64(body)
		sum := sha256.Sum256([]byte(in))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		return in + "." + b64(sig)
	}
	const sub = "0192b6c1-0000-0000-0000-0000000000a1"
	token := sign(map[string]any{
		"iss": realm, "aud": "taxi-web", "sub": sub, "exp": time.Now().Add(time.Hour).Unix(),
		"realm_access": map[string]any{"roles": []any{"authAdmin", "offline_access"}},
	})
	a := reasonr.NewArena()
	_, c, err := j.Authenticate(context.Background(), ports.Credentials{Scheme: "Bearer", Token: token}, a)
	if err != nil {
		t.Fatal(err)
	}
	if ti := mach.Schema().Type(c.Actor); ti == nil || ti.Name != "Auth.User" || a.Format(c.Subject) != fmt.Sprintf("T%d.Id(%s)", c.Actor, sub) {
		t.Errorf("caller: %+v", c)
	}
	if len(c.Roles) != 2 || c.Roles[0] != "authAdmin" || c.Roles[1] != "offline_access" {
		t.Errorf("roles: %v", c.Roles)
	}
	// A token of another realm is not accepted.
	other := sign(map[string]any{"iss": srv.URL + "/realms/other", "sub": sub, "exp": time.Now().Add(time.Hour).Unix()})
	if _, _, err := j.Authenticate(context.Background(), ports.Credentials{Scheme: "Bearer", Token: other}, a); err == nil {
		t.Error("a token of another realm was accepted")
	}
}
