// Package pii encrypts personal data (RFC §20): the key hierarchy "master
// key in the KMS → tenant key → subject key", value encryption with
// AES-256-GCM, blind indexes with HMAC-SHA-256. Stores call the package
// when writing and reading; the model only says whose data it is.
package pii

import (
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/tycheg/reasonr-engine/port"
)

// ErrKeyGone: the resealer (Keyring.Resealer) did not find the key that
// decrypted the value, or that key is already destroyed: the value is
// forgotten and written as none.
var ErrKeyGone = errors.New("pii: the subject key is destroyed — the value is not resealed")

// ErrTampered: a ciphertext does not decrypt with the live key of its
// version — corruption or substitution. The value reads as none, the fault goes to the log.
var ErrTampered = errors.New("pii: the ciphertext fails verification (corruption or substitution)")

// Options are the Keyring settings.
type Options struct {
	// Subjects is the size of the subject key cache: 0 — 100,000, < 0 — no cache.
	Subjects int
	// SubjectTTL is the lifetime of a subject key in the cache: 0 — 5 minutes.
	SubjectTTL time.Duration
	// TenantTTL is the lifetime of a tenant key in the cache: 0 — 1 hour.
	TenantTTL time.Duration
	// Now is the clock for cache lifetimes; nil — time.Now.
	Now func() time.Time
	// Rand is the source of keys and nonces; nil — crypto/rand. An engine
	// kind that repeats units of work on several copies of its state sets a
	// deterministic source: the copies encrypt identically.
	Rand io.Reader
}

// Keyring holds tenant and subject keys with a cache; one per process, safe
// for concurrent use.
type Keyring struct {
	kms   port.KMS
	keys  port.KeyStore
	opts  Options
	cache *lru

	mu      sync.Mutex
	tenants map[port.Tenant]*tenantKeys
}

// tenantKeys is a decrypted tenant key and the keys derived from it.
type tenantKeys struct {
	wrap    cipher.AEAD // wraps subject keys
	blind   []byte      // the root of blind index keys
	expires time.Time

	mu     sync.Mutex
	fields map[string]*sync.Pool // the blind index HMAC by field
}

// Shared is a store that encrypts with Keyring keys and can take another
// store's (RFC §31.2): the event store and asynchronous projections in a
// separate database encrypt with the main store's keys, and its forget
// makes their data unreadable too. UseKeyring is called when building the
// engine, before the first access to the store.
type Shared interface {
	Keyring() *Keyring
	UseKeyring(*Keyring)
}

// NewKeyring creates a Keyring over a KMS and a key store.
func NewKeyring(kms port.KMS, keys port.KeyStore, opts Options) *Keyring {
	if opts.Subjects == 0 {
		opts.Subjects = 100_000
	}
	if opts.SubjectTTL == 0 {
		opts.SubjectTTL = 5 * time.Minute
	}
	if opts.TenantTTL == 0 {
		opts.TenantTTL = time.Hour
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Rand == nil {
		opts.Rand = rand.Reader
	}
	return &Keyring{kms: kms, keys: keys, opts: opts, cache: newLRU(opts.Subjects, opts.SubjectTTL), tenants: map[port.Tenant]*tenantKeys{}}
}

// WithKMS returns a Keyring over the same key store with the master key kms
// and an empty cache: so after rotation (Rotate) one checks that the new
// master key unwraps the tenant keys.
func (k *Keyring) WithKMS(kms port.KMS) *Keyring { return NewKeyring(kms, k.keys, k.opts) }

// KeyVersion is the version of a subject key the unit of work encrypted
// data with: the commit checks it in the transaction (RFC §20).
type KeyVersion struct {
	Subject port.SubjectRef
	Version uint32
	// Created: this sealer created the key. In a store with transactions it
	// is created in the commit transaction: its lock of the key row holds
	// forget until the commit, so such a key needs no check.
	Created bool
}

// Sealer encrypts and decrypts within one unit of work or one read of a
// tenant. It is not safe for concurrent use.
type Sealer struct {
	k      *Keyring
	tenant port.Tenant
	t      *tenantKeys
	used   []KeyVersion
	buf    []byte
	// opened holds the key versions that decrypted values (Opened); for a
	// resealer — also the versions it may encrypt with.
	opened []port.KeyVersion
	reseal bool
}

// Sealer returns the tenant's sealer.
func (k *Keyring) Sealer(tenant port.Tenant) *Sealer { return &Sealer{k: k, tenant: tenant} }

// Resealer returns the tenant's resealer for the effect stream (the event
// journal, asynchronous projections, RFC §31.2): it encrypts a subject's
// value only with the key version from keys (the one that decrypted the
// value from the outbox) or the one it decrypted the previous value with,
// and creates no keys — neither subject nor tenant ones. No key or a
// destroyed one gives ErrKeyGone: a value forgotten by then does not come
// back under a new key. No version check in the transaction is needed: a
// version destroyed concurrently only makes what is written unreadable.
func (k *Keyring) Resealer(tenant port.Tenant, keys []port.KeyVersion) *Sealer {
	return &Sealer{k: k, tenant: tenant, opened: append([]port.KeyVersion(nil), keys...), reseal: true}
}

// Opened returns the key versions Open decrypted values with, without
// repeats: the outbox reader passes them in CommittedUnit.Keys.
func (s *Sealer) Opened() []port.KeyVersion { return s.opened }

// Tenant returns the sealer's tenant.
func (s *Sealer) Tenant() port.Tenant { return s.tenant }

// Ready loads the tenant key, creating it on the first write. A store with
// transactions calls it before the commit transaction: a tenant key
// created in a rolled-back transaction would stay in the cache without a
// row in the store.
func (s *Sealer) Ready(ctx context.Context) error {
	_, err := s.tenantKeys(ctx)
	return err
}

// Used returns the key versions Seal encrypted with, without repeats.
func (s *Sealer) Used() []KeyVersion { return s.used }

// Forgotten drops the keys of forgotten subjects from the cache (after a
// forget commit and on a hub message from other instances).
func (k *Keyring) Forgotten(tenant port.Tenant, subjects []port.SubjectRef) {
	var b []byte
	for _, s := range subjects {
		b = cacheKey(b[:0], tenant, s)
		k.cache.drop(string(b))
	}
}

// DropTenant forgets the tenant key in the cache (the tenant is deleted).
func (k *Keyring) DropTenant(tenant port.Tenant) {
	k.mu.Lock()
	delete(k.tenants, tenant)
	k.mu.Unlock()
}

// tenantKeys returns the tenant keys; if there is no key, it creates one.
func (s *Sealer) tenantKeys(ctx context.Context) (*tenantKeys, error) {
	now := s.k.opts.Now()
	if s.t != nil && now.Before(s.t.expires) {
		return s.t, nil
	}
	s.k.mu.Lock()
	t := s.k.tenants[s.tenant]
	s.k.mu.Unlock()
	if t != nil && now.Before(t.expires) {
		s.t = t
		return t, nil
	}
	wrapped, err := s.k.keys.TenantKey(ctx, s.tenant)
	if err != nil {
		return nil, err
	}
	keyContext := TenantKeyContext(s.tenant)
	if wrapped == nil && s.reseal {
		return nil, ErrKeyGone
	}
	if wrapped == nil {
		w, err := s.k.kms.Wrap(ctx, keyContext, randomKey(s.k.opts.Rand))
		if err != nil {
			return nil, fmt.Errorf("pii: KMS: %w", err)
		}
		if wrapped, err = s.k.keys.CreateTenantKey(ctx, s.tenant, w); err != nil {
			return nil, err
		}
	}
	raw, err := s.k.kms.Unwrap(ctx, keyContext, wrapped)
	if err != nil {
		return nil, fmt.Errorf("pii: tenant key %q: %w", s.tenant, err)
	}
	aead, err := newAEAD(hkdf(raw, "reasonr/wrap", KeySize))
	if err != nil {
		return nil, err
	}
	t = &tenantKeys{wrap: aead, blind: hkdf(raw, "reasonr/blind", KeySize), expires: now.Add(s.k.opts.TenantTTL), fields: map[string]*sync.Pool{}}
	s.k.mu.Lock()
	s.k.tenants[s.tenant] = t
	s.k.mu.Unlock()
	s.t = t
	return t, nil
}

// current returns a subject's live key; if there is no key or it is
// destroyed, it creates a new version (created). On a cache miss it calls
// CreateSubjectKey right away: it also returns the live key, and a miss on
// a write is most often a new subject (an existing one's key is already in
// the cache: the command read its data).
func (s *Sealer) current(ctx context.Context, ref port.SubjectRef) (k subjectKey, created bool, err error) {
	if s.reseal {
		k, err := s.resealKey(ctx, ref)
		return k, false, err
	}
	now := s.k.opts.Now()
	s.buf = cacheKey(s.buf[:0], s.tenant, ref)
	if v, ok := s.k.cache.get(s.buf, now); ok && v.aead != nil {
		return v, false, nil
	}
	t, err := s.tenantKeys(ctx)
	if err != nil {
		return subjectKey{}, false, err
	}
	wrapped, err := s.wrapKey(t, ref, randomKey(s.k.opts.Rand))
	if err != nil {
		return subjectKey{}, false, err
	}
	wk, err := s.k.keys.CreateSubjectKey(ctx, s.tenant, ref, wrapped)
	if err != nil {
		return subjectKey{}, false, err
	}
	if wk.Wrapped == nil {
		return subjectKey{}, false, fmt.Errorf("pii: the store did not create the subject key %s %s", ref.Type, ref.ID)
	}
	k, err = s.remember(t, wk, now)
	return k, bytes.Equal(wk.Wrapped, wrapped), err // otherwise — the key of the race winner
}

// resealKey returns the resealer's key: the versions from opened.
func (s *Sealer) resealKey(ctx context.Context, ref port.SubjectRef) (subjectKey, error) {
	for _, kv := range s.opened {
		if kv.Subject != ref {
			continue
		}
		k, ok, err := s.atVersion(ctx, ref, kv.Version)
		if err == nil && !ok {
			err = ErrKeyGone
		}
		return k, err
	}
	return subjectKey{}, ErrKeyGone
}

// noteOpened remembers the key version that decrypted a value.
func (s *Sealer) noteOpened(ref port.SubjectRef, v uint32) {
	for _, kv := range s.opened {
		if kv.Subject == ref {
			return
		}
	}
	s.opened = append(s.opened, port.KeyVersion{Subject: ref, Version: v})
}

// atVersion returns the subject key of version v; ok == false — there is no
// key of this version (destroyed, never created or replaced by a newer one).
func (s *Sealer) atVersion(ctx context.Context, ref port.SubjectRef, v uint32) (subjectKey, bool, error) {
	now := s.k.opts.Now()
	s.buf = cacheKey(s.buf[:0], s.tenant, ref)
	if c, ok := s.k.cache.get(s.buf, now); ok && c.version >= v {
		return c, c.version == v && c.aead != nil, nil
	}
	ks, err := s.k.keys.SubjectKeys(ctx, s.tenant, []port.SubjectRef{ref})
	if err != nil || len(ks) == 0 {
		return subjectKey{}, false, err
	}
	t, err := s.tenantKeys(ctx)
	if err != nil {
		return subjectKey{}, false, err
	}
	c, err := s.remember(t, ks[0], now)
	if err != nil {
		return subjectKey{}, false, err
	}
	return c, c.version == v && c.aead != nil, nil
}

// Prefetch loads subject keys into the cache with one store query: before
// decrypting a search page or a unit of work.
func (s *Sealer) Prefetch(ctx context.Context, refs []port.SubjectRef) error {
	now := s.k.opts.Now()
	var miss []port.SubjectRef
	for _, ref := range refs {
		s.buf = cacheKey(s.buf[:0], s.tenant, ref)
		if _, ok := s.k.cache.get(s.buf, now); !ok {
			miss = append(miss, ref)
		}
	}
	if len(miss) == 0 {
		return nil
	}
	t, err := s.tenantKeys(ctx)
	if err != nil {
		return err
	}
	ks, err := s.k.keys.SubjectKeys(ctx, s.tenant, miss)
	if err != nil {
		return err
	}
	for _, wk := range ks {
		if _, err := s.remember(t, wk, now); err != nil {
			return err
		}
	}
	return nil
}

// remember unwraps a subject key and puts it into the cache.
func (s *Sealer) remember(t *tenantKeys, wk port.WrappedKey, now time.Time) (subjectKey, error) {
	v := subjectKey{version: wk.Version}
	if wk.Wrapped != nil {
		raw, err := s.unwrapKey(t, wk.Subject, wk.Wrapped)
		if err != nil {
			return subjectKey{}, err
		}
		if v.aead, err = newAEAD(raw); err != nil {
			return subjectKey{}, err
		}
	}
	s.k.cache.put(string(cacheKey(nil, s.tenant, wk.Subject)), v, now)
	return v, nil
}

func (s *Sealer) wrapKey(t *tenantKeys, ref port.SubjectRef, raw []byte) ([]byte, error) {
	nonce := make([]byte, t.wrap.NonceSize(), t.wrap.NonceSize()+len(raw)+t.wrap.Overhead())
	if _, err := io.ReadFull(s.k.opts.Rand, nonce); err != nil {
		return nil, err
	}
	return t.wrap.Seal(nonce, nonce, raw, keyAAD(nil, s.tenant, ref)), nil
}

func (s *Sealer) unwrapKey(t *tenantKeys, ref port.SubjectRef, wrapped []byte) ([]byte, error) {
	n := t.wrap.NonceSize()
	if len(wrapped) < n+t.wrap.Overhead() {
		return nil, fmt.Errorf("pii: the wrapped subject key %s %s is too short", ref.Type, ref.ID)
	}
	raw, err := t.wrap.Open(nil, wrapped[:n], wrapped[n:], keyAAD(nil, s.tenant, ref))
	if err != nil {
		return nil, fmt.Errorf("pii: the subject key %s %s does not unwrap with the tenant key: %w", ref.Type, ref.ID, err)
	}
	return raw, nil
}

func (s *Sealer) markUsed(ref port.SubjectRef, v uint32, created bool) {
	for _, u := range s.used {
		if u.Subject == ref {
			return
		}
	}
	s.used = append(s.used, KeyVersion{Subject: ref, Version: v, Created: created})
}
