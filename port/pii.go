package port

import "context"

// KMS holds the personal data master key (RFC §20): it wraps tenant keys.
// The master key itself never leaves the KMS. keyContext binds a wrapped
// key to the tenant: it cannot be unwrapped with another context.
type KMS interface {
	Wrap(ctx context.Context, keyContext string, plain []byte) ([]byte, error)
	Unwrap(ctx context.Context, keyContext string, wrapped []byte) ([]byte, error)
}

// SubjectRef is a personal data subject in the key store: the type name
// and the identifier's canonical JSON.
type SubjectRef struct {
	Type string
	ID   string
}

// WrappedKey is a subject key wrapped with the tenant key. Wrapped == nil —
// the key is destroyed by forget (a tombstone of version Version); versions
// only grow.
type WrappedKey struct {
	Subject SubjectRef
	Version uint32
	Wrapped []byte
}

// KeyVersion is the version of a subject key that decrypted data from the
// outbox (CommittedUnit.Keys): the effect stream stores reencrypt it with
// the same version (RFC §20).
type KeyVersion struct {
	Subject SubjectRef
	Version uint32
}

// KeyStore holds the wrapped tenant and subject keys (RFC §31.2): the main
// store keeps them, so that forget is atomic with the data. Destroying keys
// is part of Commit (UnitOfWork.Forget).
type KeyStore interface {
	// TenantKey returns the wrapped tenant key; nil — no key yet.
	TenantKey(ctx context.Context, tenant Tenant) ([]byte, error)
	// CreateTenantKey stores the tenant key if there is none and returns
	// the stored one (on a race — the winner's key).
	CreateTenantKey(ctx context.Context, tenant Tenant, wrapped []byte) ([]byte, error)
	// SubjectKeys returns the keys and tombstones of the subjects that have
	// them; the order is not defined.
	SubjectKeys(ctx context.Context, tenant Tenant, subjects []SubjectRef) ([]WrappedKey, error)
	// CreateSubjectKey stores a subject key if there is no live key:
	// version 1, after a tombstone — the next one. If a live key exists, it
	// returns it: on a cache miss the encrypter calls CreateSubjectKey right
	// away, without SubjectKeys (one round trip to the store for a new
	// subject). In a transaction a live key must not abort it.
	CreateSubjectKey(ctx context.Context, tenant Tenant, s SubjectRef, wrapped []byte) (WrappedKey, error)
}

// TenantKeyRotator is a key store that can rewrap tenant keys with a new
// master key (reasonrd pii rotate, RFC §20).
type TenantKeyRotator interface {
	// TenantKeys returns the wrapped keys of every tenant.
	TenantKeys(ctx context.Context) (map[Tenant][]byte, error)
	// ReplaceTenantKey replaces a tenant's wrapped key if it is still old;
	// false — the key changed or was deleted.
	ReplaceTenantKey(ctx context.Context, tenant Tenant, old, wrapped []byte) (bool, error)
}
