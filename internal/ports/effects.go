package ports

import "github.com/tycheg/reasonr-engine/port"

// RequestSigner signs the engine's requests to the integration service
// (RFC §18): a JWT the service checks with the engine's public keys
// (GET /.well-known/jwks.json).
type RequestSigner interface {
	// Token is the token of a request with the idempotency key key to the
	// system system in the tenant tenant.
	Token(system string, tenant port.Tenant, key string) (string, error)
	// JWKS are the public keys as a JSON Web Key Set.
	JWKS() []byte
}
