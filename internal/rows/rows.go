// Package rows adapts a row store of another repository (port.RowStore and
// the other byte-level ports: PostgreSQL, MongoDB) to the engine's store
// interfaces at the level of model values (package storage): it lays the
// model out in tables and key columns, encodes and decodes the state,
// encrypts personal values and builds blind keys (RFC §20), encodes the
// outbox, timer and request records (plan 0021, decision 5).
package rows

import (
	"context"
	"io"
	"sync"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/pii"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Store is a row store seen as a store of model values. It implements the
// storage interfaces its row store has ports for.
type Store struct {
	db      port.RowStore
	catalog kind.Catalog
	ring    *pii.Keyring
	keys    port.KeyStore // the key store of ring, nil — none

	mu     sync.Mutex
	models map[*reasonr.Schema]*model
}

var (
	_ kind.UnitStore = (*Store)(nil)
	_ pii.Shared     = (*Store)(nil)
)

// Options are the settings of the adaptation.
type Options struct {
	// KMS wraps the tenant keys of personal data; nil — personal data
	// cannot be written (pii.ErrNoKMS).
	KMS  port.KMS
	Keys pii.Options
}

// New returns the row store db as a store of model values with the tenant
// models catalog. db's key store (port.KeyStore) keeps the personal data keys.
func New(db port.RowStore, catalog kind.Catalog, opts Options) *Store {
	s := &Store{db: db, catalog: catalog, models: map[*reasonr.Schema]*model{}}
	kms := opts.KMS
	if kms == nil {
		kms = pii.NoKMS{}
	}
	var ks port.KeyStore
	if k, ok := db.(port.KeyStore); ok {
		ks = &keyStore{KeyStore: k}
	}
	s.ring, s.keys = pii.NewKeyring(kms, ks, opts.Keys), ks
	return s
}

// Wrap returns v as a store of model values: a row store (port.RowStore)
// is adapted, anything else is returned as it is.
func Wrap(v any, catalog kind.Catalog, opts Options) any {
	if db, ok := v.(port.RowStore); ok {
		return New(db, catalog, opts)
	}
	return v
}

// Unwrap returns the row store.
func (s *Store) Unwrap() any { return s.db }

// Keyring and UseKeyring share one set of personal data keys between the
// stores (pii.Shared, RFC §31.2).
func (s *Store) Keyring() *pii.Keyring     { return s.ring }
func (s *Store) UseKeyring(k *pii.Keyring) { s.ring = k }

// DropKeys drops the keys of subjects another instance forgot from the cache (ports.KeyCache).
func (s *Store) DropKeys(tenant port.Tenant, subjects []port.SubjectRef) {
	s.ring.Forgotten(tenant, subjects)
}

// Close closes the row store.
func (s *Store) Close() error {
	if c, ok := s.db.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// Capabilities are the row store's.
func (s *Store) Capabilities() port.Capabilities { return s.db.Capabilities() }

// model returns the tenant's model.
func (s *Store) model(tenant port.Tenant) (*model, error) {
	sc, err := s.catalog.Schema(tenant)
	if err != nil {
		return nil, err
	}
	return s.modelOf(sc)
}

func (s *Store) modelOf(sc *reasonr.Schema) (*model, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.models[sc]; m != nil {
		return m, nil
	}
	m, err := newModel(sc)
	if err != nil {
		return nil, err
	}
	s.models[sc] = m
	return m, nil
}

// As returns st as the port T: the store itself or, for a row store seen as
// values, the row store it adapts (port.Coordinator, port.KeyStore, …).
func As[T any](st any) (T, bool) {
	if t, ok := st.(T); ok {
		return t, true
	}
	if w, ok := st.(interface{ Unwrap() any }); ok {
		t, ok := w.Unwrap().(T)
		return t, ok
	}
	var zero T
	return zero, false
}

// Revive brings the dead timers of a tenant back (reasonrd timers retry).
func (s *Store) Revive(ctx context.Context, tenant port.Tenant) (int64, error) {
	r, ok := s.db.(interface {
		Revive(ctx context.Context, tenant port.Tenant) (int64, error)
	})
	if !ok {
		return 0, nil
	}
	return r.Revive(ctx, tenant)
}

// DropTenant deletes a tenant's data (an ephemeral tenant in test mode).
func (s *Store) DropTenant(ctx context.Context, tenant port.Tenant) error {
	s.ring.DropTenant(tenant)
	if d, ok := s.db.(interface {
		DropTenant(ctx context.Context, tenant port.Tenant) error
	}); ok {
		return d.DropTenant(ctx, tenant)
	}
	return nil
}

// keyStore is the row store's key store; in a commit (newKeys in the
// context) a new subject key is not written at once but goes into the unit
// of work: the store writes it in the commit transaction. A new subject is
// assumed to have no key (version 1) without a read; if it had one, the
// commit conflicts, and the retry reads the key first.
type keyStore struct {
	port.KeyStore
	mu      sync.Mutex
	recheck map[port.SubjectRef]bool // subjects whose new key conflicted
}

// newKeys collects the subject keys a unit of work creates.
type newKeys struct{ keys []port.WrappedKey }

type newKeysKey struct{}

func (k *keyStore) CreateSubjectKey(ctx context.Context, tenant port.Tenant, ref port.SubjectRef, wrapped []byte) (port.WrappedKey, error) {
	nk, ok := ctx.Value(newKeysKey{}).(*newKeys)
	if !ok {
		return k.KeyStore.CreateSubjectKey(ctx, tenant, ref, wrapped)
	}
	version := uint32(1)
	k.mu.Lock()
	recheck := k.recheck[ref]
	delete(k.recheck, ref)
	k.mu.Unlock()
	if recheck {
		have, err := k.SubjectKeys(ctx, tenant, []port.SubjectRef{ref})
		if err != nil {
			return port.WrappedKey{}, err
		}
		for _, w := range have {
			if w.Subject != ref {
				continue
			}
			if w.Wrapped != nil {
				return w, nil // a live key
			}
			version = w.Version + 1 // over the tombstone
		}
	}
	wk := port.WrappedKey{Subject: ref, Version: version, Wrapped: wrapped}
	nk.keys = append(nk.keys, wk)
	return wk, nil
}

// conflicted marks the subjects of keys a failed commit created: the retry
// reads their keys.
func (k *keyStore) conflicted(keys []port.WrappedKey) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.recheck == nil {
		k.recheck = map[port.SubjectRef]bool{}
	}
	for _, w := range keys {
		k.recheck[w.Subject] = true
	}
}
