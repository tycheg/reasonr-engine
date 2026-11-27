// Package auth holds the transport authenticators (RFC §30–31): JWT
// (HS256, RS256, ES256; static keys or keys from JWKS with a cache), static
// API keys of external systems and the test scheme (only in the server's
// test mode). Each determines the data tenant and the caller: an actor
// (type and id) and roles.
package auth

import (
	"context"
	"fmt"
	"strings"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/codecs"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Chain chooses an authenticator by the credentials scheme: Bearer — JWT,
// ApiKey — keys, Test — the test one. A scheme without an authenticator is
// ports.ErrUnauthenticated.
type Chain map[string]ports.Authenticator

var _ ports.Authenticator = Chain(nil)

// Authenticate passes the credentials to the authenticator of their scheme.
func (c Chain) Authenticate(ctx context.Context, cred ports.Credentials, a *reasonr.Arena) (port.Tenant, reasonr.Caller, error) {
	for scheme, au := range c {
		if strings.EqualFold(scheme, cred.Scheme) {
			return au.Authenticate(ctx, cred, a)
		}
	}
	return "", reasonr.Caller{}, unauthenticated("scheme %q is not supported", cred.Scheme)
}

// ParseAuthorization parses the Authorization header: "Scheme token".
func ParseAuthorization(h string) ports.Credentials {
	scheme, token, _ := strings.Cut(strings.TrimSpace(h), " ")
	return ports.Credentials{Scheme: scheme, Token: strings.TrimSpace(token)}
}

func unauthenticated(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ports.ErrUnauthenticated, fmt.Sprintf(format, args...))
}

// Identity is what the authenticator learned about the caller, before
// resolving it by the tenant's model.
type Identity struct {
	Tenant    port.Tenant
	ActorType string // the actor type name; "" — not an actor
	ActorID   string // the actor id in text form (jsonv.DecodeIDText)
	Roles     []string
}

// Caller builds the caller by the tenant's model from the catalog: the
// actor must be an actor type of the model, its id readable by the type's strategy.
func (id *Identity) Caller(catalog kind.Catalog, a *reasonr.Arena) (reasonr.Caller, error) {
	c := reasonr.Caller{Kind: reasonr.CallerUser, Subject: reasonr.None, Roles: id.Roles, Tenant: string(id.Tenant)}
	s, err := catalog.Schema(id.Tenant)
	if err == nil && len(id.Roles) > 0 {
		// Model roles are shared by the project (admin); a name from
		// authentication is checked against the schema (Schema.Role).
		c.Roles = make([]string, len(id.Roles))
		for i, r := range id.Roles {
			c.Roles[i] = s.Role(r)
		}
	}
	if id.ActorType == "" {
		return c, nil
	}
	if err != nil {
		return c, unauthenticated("%v", err)
	}
	ti := s.Lookup(id.ActorType)
	if ti == nil || ti.Kind != reasonr.TypeActor {
		return c, unauthenticated("%q is not an actor of the model", id.ActorType)
	}
	v, err := codecs.JSONOf(s, codecs.Options{}).DecodeIDText(a, ti.Ref, id.ActorID)
	if err != nil {
		return c, unauthenticated("actor id of %s: %v", ti.Name, err)
	}
	c.Actor, c.Subject = ti.Ref, v
	return c, nil
}
