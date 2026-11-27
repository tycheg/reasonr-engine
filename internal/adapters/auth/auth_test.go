package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/conformance"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

func catalog(t *testing.T) kind.Catalog {
	p := kernel.NewPrograms()
	p.Set(port.DefaultTenant, conformance.Machine(t))
	p.Set("acme", conformance.Machine(t))
	return p
}

const keeper = "00000000-0000-4000-8000-000000000001"

var now = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func claims(extra map[string]any) map[string]any {
	c := map[string]any{"sub": keeper, "actor_type": "Keeper", "roles": []any{"admin"}, "exp": now.Add(time.Hour).Unix()}
	for k, v := range extra {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

func TestJWT(t *testing.T) {
	secret := []byte("a secret")
	j := &JWT{Keys: StaticKey{secret}, Claims: Claims{Tenant: "tid", ActorType: "actor_type"}, Now: func() time.Time { return now },
		Audience: "api", Catalog: catalog(t)}
	a := reasonr.NewArena()
	ok, _ := Sign(claims(map[string]any{"tid": "acme", "aud": []any{"x", "api"}}), secret)
	tenant, c, err := j.Authenticate(context.Background(), ParseAuthorization("Bearer "+ok), a)
	if err != nil || tenant != "acme" || c.Kind != reasonr.CallerUser || len(c.Roles) != 1 || c.Roles[0] != "admin" ||
		a.Format(c.Subject) != "T3.Id(00000000-0000-4000-8000-000000000001)" {
		t.Fatalf("tenant %q, caller %+v %s, %v", tenant, c, a.Format(c.Subject), err)
	}
	noActor, _ := Sign(claims(map[string]any{"aud": "api", "actor_type": nil, "roles": "a b"}), secret)
	tenant, c, err = j.Authenticate(context.Background(), ParseAuthorization("Bearer "+noActor), a)
	if err != nil || tenant != port.DefaultTenant || c.Subject.Kind != reasonr.KindNone || len(c.Roles) != 2 {
		t.Fatalf("without an actor: %q %+v %v", tenant, c, err)
	}
	other, _ := Sign(claims(map[string]any{"aud": "api"}), []byte("someone else's"))
	for name, tok := range map[string]string{
		"another signature": other,
		"expired":           must(Sign(claims(map[string]any{"aud": "api", "exp": now.Add(-time.Minute).Unix()}), secret)),
		"too early":         must(Sign(claims(map[string]any{"aud": "api", "nbf": now.Add(time.Minute).Unix()}), secret)),
		"wrong audience":    must(Sign(claims(map[string]any{"aud": "other"}), secret)),
		"not an actor":      must(Sign(claims(map[string]any{"aud": "api", "actor_type": "Box"}), secret)),
		"wrong id":          must(Sign(claims(map[string]any{"aud": "api", "sub": "x"}), secret)),
		"garbage":           "a.b",
		"alg none":          must(SignWith("none", "", claims(map[string]any{"aud": "api"}), func([]byte) ([]byte, error) { return nil, nil })),
	} {
		if _, _, err := j.Authenticate(context.Background(), ports.Credentials{Scheme: "Bearer", Token: tok}, a); !errors.Is(err, ports.ErrUnauthenticated) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func must(s string, err error) string {
	if err != nil {
		panic(err)
	}
	return s
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// TestJWKS: RS256 and ES256 by a key set with a cache; an unknown kid
// rereads the set at most once per MinRefresh.
func TestJWKS(t *testing.T) {
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	ek, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	var fetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		fmt.Fprintf(w, `{"keys":[{"kty":"RSA","kid":"r1","n":%q,"e":"AQAB"},{"kty":"EC","kid":"e1","crv":"P-256","x":%q,"y":%q},{"kty":"oct","kid":"s"}]}`,
			b64(rk.N.Bytes()), b64(ek.X.FillBytes(make([]byte, 32))), b64(ek.Y.FillBytes(make([]byte, 32))))
	}))
	defer srv.Close()
	j := &JWT{Keys: &JWKS{URL: srv.URL, MinRefresh: time.Hour}, Claims: Claims{ActorType: "actor_type"}, Now: func() time.Time { return now }, Catalog: catalog(t)}
	rs := must(SignWith("RS256", "r1", claims(nil), func(signed []byte) ([]byte, error) {
		d := sha256.Sum256(signed)
		return rsa.SignPKCS1v15(rand.Reader, rk, crypto.SHA256, d[:])
	}))
	es := must(SignWith("ES256", "e1", claims(nil), func(signed []byte) ([]byte, error) {
		d := sha256.Sum256(signed)
		r, s, err := ecdsa.Sign(rand.Reader, ek, d[:])
		return append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...), err
	}))
	a := reasonr.NewArena()
	for name, tok := range map[string]string{"RS256": rs, "ES256": es} {
		if _, c, err := j.Authenticate(context.Background(), ports.Credentials{Scheme: "Bearer", Token: tok}, a); err != nil || c.Subject.Kind != reasonr.KindID {
			t.Errorf("%s: %v", name, err)
		}
	}
	unknown := must(SignWith("RS256", "r2", claims(nil), func([]byte) ([]byte, error) { return make([]byte, 256), nil }))
	for range 3 {
		if _, _, err := j.Authenticate(context.Background(), ports.Credentials{Scheme: "Bearer", Token: unknown}, a); err == nil {
			t.Fatal("unknown kid: no error")
		}
	}
	if n := fetches.Load(); n != 1 {
		t.Errorf("the set was read %d times, want 1", n)
	}
}

func TestKeysAndTest(t *testing.T) {
	cat := catalog(t)
	keys := &APIKeys{Catalog: cat}
	keys.Add("k-1", Identity{Tenant: "acme", Roles: []string{"admin"}})
	chain := Chain{"ApiKey": keys, "Test": Test{Catalog: cat}}
	a := reasonr.NewArena()
	tenant, c, err := chain.Authenticate(context.Background(), ParseAuthorization("ApiKey k-1"), a)
	if err != nil || tenant != "acme" || c.Roles[0] != "admin" {
		t.Fatalf("key: %q %+v %v", tenant, c, err)
	}
	if _, _, err := chain.Authenticate(context.Background(), ParseAuthorization("ApiKey k-2"), a); !errors.Is(err, ports.ErrUnauthenticated) {
		t.Errorf("unknown key: %v", err)
	}
	tenant, c, err = chain.Authenticate(context.Background(), ParseAuthorization("Test actor=Keeper:"+keeper+", role=admin tenant=acme"), a)
	if err != nil || tenant != "acme" || c.Actor == 0 || len(c.Roles) != 1 {
		t.Fatalf("test: %q %+v %v", tenant, c, err)
	}
	for _, h := range []string{"Bearer x", "Test actor=Keeper", "Test who=me", "Test actor=Nobody:1"} {
		if _, _, err := chain.Authenticate(context.Background(), ParseAuthorization(h), a); !errors.Is(err, ports.ErrUnauthenticated) {
			t.Errorf("%s: %v", h, err)
		}
	}
}

// schemas is a catalog of ready schemas.
type schemas map[port.Tenant]*reasonr.Schema

func (s schemas) Schema(t port.Tenant) (*reasonr.Schema, error) {
	if sc := s[t]; sc != nil {
		return sc, nil
	}
	return nil, fmt.Errorf("%w: %s", port.ErrUnknownTenant, t)
}

// TestAnyTenant: an external system token for every tenant (tenant.any)
// takes the tenant from the header if all its roles are roles of the
// external systems of that tenant's model; the caller has only roles, no actor.
func TestAnyTenant(t *testing.T) {
	sc := &reasonr.Schema{Types: []reasonr.TypeInfo{
		{Ref: 1, Name: "Matching", Kind: reasonr.TypeExternal, Role: "matching"},
		{Ref: 2, Name: "Payments", Kind: reasonr.TypeExternal, Role: "payments"},
		{Ref: 3, Name: "Audit", Kind: reasonr.TypeExternal},
	}}
	ext := &reasonr.Schema{Types: []reasonr.TypeInfo{{Ref: 1, Name: "Matching", Kind: reasonr.TypeExternal, Role: "matching"}}}
	secret := []byte("a secret")
	j := &JWT{Keys: StaticKey{secret}, Claims: Claims{Tenant: "tid", AnyTenant: "*", FixedActor: "Keeper"}, Now: func() time.Time { return now },
		Catalog: schemas{"acme": sc, "beta": ext}}
	tok := func(tid string, roles ...any) string {
		return must(Sign(claims(map[string]any{"tid": tid, "roles": roles, "actor_type": nil}), secret))
	}
	auth := func(token string, tenant port.Tenant) (port.Tenant, reasonr.Caller, error) {
		return j.Authenticate(context.Background(), ports.Credentials{Scheme: "Bearer", Token: token, Tenant: tenant}, reasonr.NewArena())
	}
	for _, c := range []struct {
		tenant port.Tenant
		roles  []any
	}{{"acme", []any{"matching", "payments"}}, {"beta", []any{"matching"}}} {
		got, caller, err := auth(tok("*", c.roles...), c.tenant)
		if err != nil || got != c.tenant || len(caller.Roles) != len(c.roles) || caller.Subject.Kind != reasonr.KindNone || caller.Actor != 0 {
			t.Fatalf("%s: tenant %q, caller %+v, %v", c.tenant, got, caller, err)
		}
	}
	for name, c := range map[string]struct {
		token  string
		tenant port.Tenant
		want   error
	}{
		"without the header":               {tok("*", "matching"), "", ports.ErrUnauthenticated},
		"unknown tenant":                   {tok("*", "matching"), "nope", ports.ErrUnauthenticated},
		"without roles":                    {tok("*"), "acme", ports.ErrForbidden},
		"a role not of an external system": {tok("*", "matching", "admin"), "acme", ports.ErrForbidden},
		"a system without a role":          {tok("*", "Audit"), "acme", ports.ErrForbidden},
		"a system role of another model":   {tok("*", "payments"), "beta", ports.ErrForbidden},
	} {
		if _, _, err := auth(c.token, c.tenant); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	// An ordinary token ignores the header.
	plain := must(Sign(claims(map[string]any{"tid": "acme", "sub": nil, "actor_type": nil}), secret))
	if got, _, err := auth(plain, "beta"); err != nil || got != "acme" {
		t.Fatalf("a token of tenant acme with header beta: %q, %v", got, err)
	}
	// Without tenant.any "*" is an ordinary tenant name.
	j.Claims.AnyTenant, j.Claims.FixedActor = "", ""
	if got, _, err := auth(tok("*", "matching"), "acme"); err != nil || got != "*" {
		t.Fatalf("without tenant.any: %q, %v", got, err)
	}
}

// TestSigner: the engine's JWT is checked with its JWKS (the default kid
// is the key thumbprint); JWKS also holds the Also keys; keys are read from PEM.
func TestSigner(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	next, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	s := &Signer{Key: key, Issuer: "engine", Also: []PublicKey{{Kid: "next", Key: &next.PublicKey}}, Now: func() time.Time { return now }}
	keys, err := ParseJWKS(s.JWKS())
	if err != nil || len(keys) != 2 || keys["next"] == nil || keys[Thumbprint(&key.PublicKey)] == nil {
		t.Fatalf("JWKS: %d keys, %v", len(keys), err)
	}
	tok, err := s.Token("Matching", "acme", "k-1")
	if err != nil {
		t.Fatal(err)
	}
	j := &JWT{Keys: &JWKS{URL: serve(t, s.JWKS())}, Issuer: "engine", Audience: "Matching", Now: func() time.Time { return now.Add(4 * time.Minute) }}
	claims, err := j.Verify(context.Background(), tok)
	if err != nil || claims["tenant"] != "acme" || claims["jti"] != "k-1" {
		t.Fatalf("claims %v, %v", claims, err)
	}
	j.Now = func() time.Time { return now.Add(6 * time.Minute) }
	if _, err := j.Verify(context.Background(), tok); !errors.Is(err, ports.ErrUnauthenticated) {
		t.Fatalf("a token older than 5 min is accepted: %v", err)
	}
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(key)
	sec1, _ := x509.MarshalECPrivateKey(key)
	pub, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	for typ, der := range map[string][]byte{"PRIVATE KEY": pkcs8, "EC PRIVATE KEY": sec1, "PUBLIC KEY": pub} {
		priv, p, err := ParseECKey(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}))
		if err != nil || !p.Equal(&key.PublicKey) || (priv == nil) != (typ == "PUBLIC KEY") {
			t.Errorf("%s: %v", typ, err)
		}
	}
	rk, _ := rsa.GenerateKey(rand.Reader, 1024)
	rsaDER, _ := x509.MarshalPKCS8PrivateKey(rk)
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	p384DER, _ := x509.MarshalECPrivateKey(p384)
	for name, data := range map[string][]byte{
		"not PEM": []byte("key"),
		"RSA":     pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rsaDER}),
		"P-384":   pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: p384DER}),
	} {
		if _, _, err := ParseECKey(data); err == nil {
			t.Errorf("%s: the key is accepted", name)
		}
	}
}

func serve(t *testing.T, body []byte) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	t.Cleanup(srv.Close)
	return srv.URL
}
