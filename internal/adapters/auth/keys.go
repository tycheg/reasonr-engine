package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"strings"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// APIKeys are the static keys of external systems (Authorization: ApiKey
// <key>): each key has a tenant, roles and, if needed, an actor.
type APIKeys struct {
	Catalog kind.Catalog
	keys    []apiKey
}

type apiKey struct {
	hash [32]byte
	id   Identity
}

var _ ports.Authenticator = (*APIKeys)(nil)

// Add adds a key; only its hash is stored.
func (k *APIKeys) Add(key string, id Identity) {
	if id.Tenant == "" {
		id.Tenant = port.DefaultTenant
	}
	k.keys = append(k.keys, apiKey{hash: sha256.Sum256([]byte(key)), id: id})
}

// Authenticate looks a key up by comparing hashes in constant time.
func (k *APIKeys) Authenticate(_ context.Context, cred ports.Credentials, a *reasonr.Arena) (port.Tenant, reasonr.Caller, error) {
	h := sha256.Sum256([]byte(cred.Token))
	var found *Identity
	for i := range k.keys {
		if subtle.ConstantTimeCompare(h[:], k.keys[i].hash[:]) == 1 {
			found = &k.keys[i].id
		}
	}
	if found == nil {
		return "", reasonr.Caller{}, unauthenticated("unknown key")
	}
	c, err := found.Caller(k.Catalog, a)
	return found.Tenant, c, err
}

// Test is the test authentication (only reasonrd --test, RFC §30–31):
// "Authorization: Test actor=Customer:<id>", "Test role=admin";
// tenant=<tenant> is the data tenant, port.DefaultTenant by default. The
// parts are separated by spaces or commas; role repeats.
type Test struct {
	Catalog kind.Catalog
	// Tenants are the tenants where test sign-in is allowed (a production
	// server: tenants with demo: true, RFC §27.5); nil — all (reasonrd --test).
	Tenants map[string]bool
}

var _ ports.Authenticator = Test{}

// Authenticate parses the test credentials.
func (t Test) Authenticate(_ context.Context, cred ports.Credentials, a *reasonr.Arena) (port.Tenant, reasonr.Caller, error) {
	id := Identity{Tenant: port.DefaultTenant}
	for _, part := range strings.FieldsFunc(cred.Token, func(r rune) bool { return r == ' ' || r == ',' }) {
		k, v, ok := strings.Cut(part, "=")
		switch {
		case !ok:
			return "", reasonr.Caller{}, unauthenticated("test credentials: %q without =", part)
		case k == "tenant":
			id.Tenant = port.Tenant(v)
		case k == "role":
			id.Roles = append(id.Roles, v)
		case k == "actor":
			typ, ident, ok := strings.Cut(v, ":")
			if !ok {
				return "", reasonr.Caller{}, unauthenticated("test credentials: actor=Type:id")
			}
			id.ActorType, id.ActorID = typ, ident
		default:
			return "", reasonr.Caller{}, unauthenticated("test credentials: unknown part %q", k)
		}
	}
	if t.Tenants != nil && !t.Tenants[string(id.Tenant)] {
		return "", reasonr.Caller{}, unauthenticated("test sign-in is only for tenants with demo: true")
	}
	c, err := id.Caller(t.Catalog, a)
	return id.Tenant, c, err
}
