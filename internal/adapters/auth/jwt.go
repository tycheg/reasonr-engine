package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// KeySource gives signature verification keys: []byte for HS256,
// *rsa.PublicKey for RS256, *ecdsa.PublicKey for ES256.
type KeySource interface {
	Key(ctx context.Context, kid, alg string) (any, error)
}

// StaticKey is one key for any kid.
type StaticKey struct{ K any }

func (s StaticKey) Key(context.Context, string, string) (any, error) { return s.K, nil }

// Claims say how token claims map to the tenant and the caller. Empty
// claim names mean the defaults.
type Claims struct {
	Tenant        string      // the claim with the tenant; "" — always DefaultTenant
	DefaultTenant port.Tenant // the tenant if there is no claim; "" — port.DefaultTenant
	Actor         string      // the claim with the actor id; sub by default
	ActorType     string      // the claim with the actor type; "" — FixedActor
	FixedActor    string      // the actor type for every token; "" — the actor only from ActorType
	Roles         string      // the claim with the roles (an array or a space-separated string); roles by default
	// AnyTenant is the value of the Tenant claim of an external system token
	// for every tenant (tenant.any, usually "*"); "" — no such tokens.
	AnyTenant string
}

// JWT authenticates by a JWT in the Authorization: Bearer header.
type JWT struct {
	Keys     KeySource
	Claims   Claims
	Issuer   string        // "" — not checked
	Audience string        // "" — not checked
	Leeway   time.Duration // the clock leeway for exp and nbf
	Now      func() time.Time
	Catalog  kind.Catalog
}

var _ ports.Authenticator = (*JWT)(nil)

// Authenticate checks the token's signature and times and builds the caller.
func (j *JWT) Authenticate(ctx context.Context, cred ports.Credentials, a *reasonr.Arena) (port.Tenant, reasonr.Caller, error) {
	claims, err := j.Verify(ctx, cred.Token)
	if err != nil {
		return "", reasonr.Caller{}, err
	}
	id := j.identity(claims)
	if t, _ := claims[j.Claims.Tenant].(string); j.Claims.AnyTenant != "" && j.Claims.Tenant != "" && t == j.Claims.AnyTenant {
		if err := j.anyTenant(id, cred.Tenant); err != nil {
			return "", reasonr.Caller{}, err
		}
	}
	c, err := id.Caller(j.Catalog, a)
	return id.Tenant, c, err
}

// anyTenant chooses the tenant of an external system token for every tenant
// (RFC §18): the integration service names the tenant with the
// X-Reasonr-Tenant header, returning it from the engine's request. Only a
// token whose roles are all roles of the external systems of that tenant's
// model may do so; the caller has only these roles, no actor.
func (j *JWT) anyTenant(id *Identity, tenant port.Tenant) error {
	if tenant == "" {
		return unauthenticated("a token for every tenant: name the tenant with the X-Reasonr-Tenant header")
	}
	s, err := j.Catalog.Schema(tenant)
	if err != nil {
		return unauthenticated("%v", err)
	}
	if len(id.Roles) == 0 {
		return forbidden("a token for every tenant without roles: it is only for external systems")
	}
	systems := map[string]bool{}
	for i := range s.Types {
		if ti := &s.Types[i]; ti.Kind == reasonr.TypeExternal && ti.Role != "" {
			systems[ti.Role] = true
		}
	}
	for _, r := range id.Roles {
		if !systems[s.Role(r)] {
			return forbidden("a token for every tenant: role %q is not a role of an external system of the model of tenant %s", r, tenant)
		}
	}
	id.Tenant, id.ActorType, id.ActorID = tenant, "", ""
	return nil
}

func forbidden(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ports.ErrForbidden, fmt.Sprintf(format, args...))
}

// identity maps the claims to the tenant, the actor and the roles.
func (j *JWT) identity(claims map[string]any) *Identity {
	cl := j.Claims
	id := &Identity{Tenant: cl.DefaultTenant}
	if id.Tenant == "" {
		id.Tenant = port.DefaultTenant
	}
	if s, ok := claims[cl.Tenant].(string); ok && cl.Tenant != "" && s != "" {
		id.Tenant = port.Tenant(s)
	}
	id.ActorType = cl.FixedActor
	if s, ok := claims[cl.ActorType].(string); ok && cl.ActorType != "" {
		id.ActorType = s
	}
	actor := cl.Actor
	if actor == "" {
		actor = "sub"
	}
	if s, ok := claims[actor].(string); ok && id.ActorType != "" {
		id.ActorID = s
	} else {
		id.ActorType = ""
	}
	roles := cl.Roles
	if roles == "" {
		roles = "roles"
	}
	switch r := claimPath(claims, roles).(type) {
	case string:
		id.Roles = strings.Fields(r)
	case []any:
		for _, x := range r {
			if s, ok := x.(string); ok {
				id.Roles = append(id.Roles, s)
			}
		}
	}
	return id
}

// Verify checks the signature, exp, nbf, iss and aud and returns the claims.
func (j *JWT) Verify(ctx context.Context, token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, unauthenticated("the token is not a JWT")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &header); err != nil {
		return nil, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, unauthenticated("the signature is not base64url")
	}
	key, err := j.Keys.Key(ctx, header.Kid, header.Alg)
	if err != nil {
		return nil, unauthenticated("key %q: %v", header.Kid, err)
	}
	if err := verify(header.Alg, key, []byte(parts[0]+"."+parts[1]), sig); err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := decodeSegment(parts[1], &claims); err != nil {
		return nil, err
	}
	now := time.Now()
	if j.Now != nil {
		now = j.Now()
	}
	if exp, ok := numeric(claims["exp"]); ok && now.After(time.Unix(exp, 0).Add(j.Leeway)) {
		return nil, unauthenticated("the token has expired")
	}
	if nbf, ok := numeric(claims["nbf"]); ok && now.Add(j.Leeway).Before(time.Unix(nbf, 0)) {
		return nil, unauthenticated("the token is not valid yet")
	}
	if j.Issuer != "" && claims["iss"] != j.Issuer {
		return nil, unauthenticated("wrong issuer")
	}
	if j.Audience != "" && !hasAudience(claims["aud"], j.Audience) {
		return nil, unauthenticated("the token is not for this service")
	}
	return claims, nil
}

func decodeSegment(seg string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return unauthenticated("a token part is not base64url")
	}
	if err := json.Unmarshal(b, v); err != nil {
		return unauthenticated("a token part is not JSON")
	}
	return nil
}

func numeric(v any) (int64, bool) {
	f, ok := v.(float64)
	return int64(f), ok
}

func hasAudience(aud any, want string) bool {
	switch a := aud.(type) {
	case string:
		return a == want
	case []any:
		for _, x := range a {
			if x == want {
				return true
			}
		}
	}
	return false
}

// verify checks the signature sig of the data signed with the algorithm alg.
func verify(alg string, key any, signed, sig []byte) error {
	digest := sha256.Sum256(signed)
	switch alg {
	case "HS256":
		secret, ok := key.([]byte)
		if !ok {
			return unauthenticated("the key does not fit HS256")
		}
		mac := hmac.New(sha256.New, secret)
		mac.Write(signed)
		if subtle.ConstantTimeCompare(mac.Sum(nil), sig) != 1 {
			return unauthenticated("wrong signature")
		}
		return nil
	case "RS256":
		pub, ok := key.(*rsa.PublicKey)
		if !ok {
			return unauthenticated("the key does not fit RS256")
		}
		if rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig) != nil {
			return unauthenticated("wrong signature")
		}
		return nil
	case "ES256":
		pub, ok := key.(*ecdsa.PublicKey)
		if !ok || len(sig) != 64 {
			return unauthenticated("the key or the signature does not fit ES256")
		}
		r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
		if !ecdsa.Verify(pub, digest[:], r, s) {
			return unauthenticated("wrong signature")
		}
		return nil
	}
	return unauthenticated("algorithm %q is not supported", alg)
}

// Sign signs the claims with an HS256 key — for tests and tools.
func Sign(claims map[string]any, secret []byte) (string, error) {
	return SignWith("HS256", "", claims, func(signed []byte) ([]byte, error) {
		mac := hmac.New(sha256.New, secret)
		mac.Write(signed)
		return mac.Sum(nil), nil
	})
}

// SignWith assembles a token with the header alg and kid and the signature sign.
func SignWith(alg, kid string, claims map[string]any, sign func(signed []byte) ([]byte, error)) (string, error) {
	h := map[string]string{"alg": alg, "typ": "JWT"}
	if kid != "" {
		h["kid"] = kid
	}
	hb, _ := json.Marshal(h)
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("auth: claims: %w", err)
	}
	signed := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	sig, err := sign([]byte(signed))
	if err != nil {
		return "", err
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// claimPath returns a claim by a dotted path: realm_access.roles in
// Keycloak is the roles field of the realm_access object.
func claimPath(claims map[string]any, path string) any {
	if v, ok := claims[path]; ok {
		return v
	}
	var cur any = claims
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}
