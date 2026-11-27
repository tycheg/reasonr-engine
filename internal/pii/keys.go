package pii

import (
	"bytes"
	"context"
	"maps"
	"sync"

	"github.com/tycheg/reasonr-engine/port"
)

// MemoryKeys is an in-memory key store (port.KeyStore): for the memory
// store and tests. The forget commit calls Destroy.
type MemoryKeys struct {
	mu       sync.Mutex
	tenants  map[port.Tenant][]byte
	subjects map[port.Tenant]map[port.SubjectRef]port.WrappedKey
}

var _ port.KeyStore = (*MemoryKeys)(nil)

// NewMemoryKeys creates an empty key store.
func NewMemoryKeys() *MemoryKeys {
	return &MemoryKeys{tenants: map[port.Tenant][]byte{}, subjects: map[port.Tenant]map[port.SubjectRef]port.WrappedKey{}}
}

// TenantKey — port.KeyStore.
func (m *MemoryKeys) TenantKey(_ context.Context, tenant port.Tenant) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tenants[tenant], nil
}

// CreateTenantKey — port.KeyStore.
func (m *MemoryKeys) CreateTenantKey(_ context.Context, tenant port.Tenant, wrapped []byte) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w := m.tenants[tenant]; w != nil {
		return w, nil
	}
	m.tenants[tenant] = wrapped
	return wrapped, nil
}

// SubjectKeys — port.KeyStore.
func (m *MemoryKeys) SubjectKeys(_ context.Context, tenant port.Tenant, refs []port.SubjectRef) ([]port.WrappedKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []port.WrappedKey
	for _, r := range refs {
		if k, ok := m.subjects[tenant][r]; ok {
			out = append(out, k)
		}
	}
	return out, nil
}

// CreateSubjectKey — port.KeyStore.
func (m *MemoryKeys) CreateSubjectKey(_ context.Context, tenant port.Tenant, ref port.SubjectRef, wrapped []byte) (port.WrappedKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ks := m.subjects[tenant]
	if ks == nil {
		ks = map[port.SubjectRef]port.WrappedKey{}
		m.subjects[tenant] = ks
	}
	k := ks[ref]
	if k.Wrapped != nil {
		return k, nil
	}
	k = port.WrappedKey{Subject: ref, Version: k.Version + 1, Wrapped: wrapped}
	ks[ref] = k
	return k, nil
}

// Version returns the version of a subject key and whether there is a live key.
func (m *MemoryKeys) Version(tenant port.Tenant, ref port.SubjectRef) (uint32, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := m.subjects[tenant][ref]
	return k.Version, k.Wrapped != nil
}

// Destroy destroys subject keys, leaving tombstones of their versions.
func (m *MemoryKeys) Destroy(tenant port.Tenant, refs []port.SubjectRef) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range refs {
		if k, ok := m.subjects[tenant][r]; ok {
			k.Wrapped = nil
			m.subjects[tenant][r] = k
		}
	}
}

// TenantKeys — port.TenantKeyRotator.
func (m *MemoryKeys) TenantKeys(context.Context) (map[port.Tenant][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.tenants), nil
}

// ReplaceTenantKey — port.TenantKeyRotator.
func (m *MemoryKeys) ReplaceTenantKey(_ context.Context, tenant port.Tenant, old, wrapped []byte) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w := m.tenants[tenant]; w == nil || !bytes.Equal(w, old) {
		return false, nil
	}
	m.tenants[tenant] = wrapped
	return true, nil
}

// DropTenant deletes every key of the tenant.
func (m *MemoryKeys) DropTenant(tenant port.Tenant) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tenants, tenant)
	delete(m.subjects, tenant)
}

// KeysSnapshot is the content of MemoryKeys: part of a memory store
// snapshot.
type KeysSnapshot struct {
	Tenants  map[port.Tenant][]byte
	Subjects map[port.Tenant][]port.WrappedKey
}

// Export returns a copy of the keys.
func (m *MemoryKeys) Export() KeysSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := KeysSnapshot{Tenants: maps.Clone(m.tenants), Subjects: map[port.Tenant][]port.WrappedKey{}}
	for t, ks := range m.subjects {
		for _, k := range ks {
			out.Subjects[t] = append(out.Subjects[t], k)
		}
	}
	return out
}

// Import replaces the keys with a snapshot.
func (m *MemoryKeys) Import(s KeysSnapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tenants = maps.Clone(s.Tenants)
	if m.tenants == nil {
		m.tenants = map[port.Tenant][]byte{}
	}
	m.subjects = map[port.Tenant]map[port.SubjectRef]port.WrappedKey{}
	for t, ks := range s.Subjects {
		m.subjects[t] = map[port.SubjectRef]port.WrappedKey{}
		for _, k := range ks {
			m.subjects[t][k.Subject] = k
		}
	}
}
