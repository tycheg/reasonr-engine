package memory

import (
	"context"
	"fmt"
	"slices"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/pii"
	"github.com/tycheg/reasonr-engine/internal/storekit"
	"github.com/tycheg/reasonr-engine/port"
)

// Personal data in memory (RFC §20): rows, events and the outbox keep
// values without personal ones (none), and their ciphertexts next to them.
// The index keys of personal fields are blind values. forget destroys the
// subject key and erases its ciphertexts and blind values in the rows
// found by the bySubject index.

// sealPending encrypts the state and events of the pending row p: it
// prepares stripped, sealed, blind, the encodings of the state and the
// event arguments.
func (sp *space) sealPending(ctx context.Context, seal *pii.Sealer, a *reasonr.Arena, p *pending) error {
	ti := sp.schema.Type(p.t)
	subjects := sp.crypto.SubjectsOf(a, ti.Fields, p.state, p.id)
	stripped, sealed, err := sp.crypto.Seal(ctx, seal, a, ti.Name, ti.Fields, p.state, subjects)
	if err != nil {
		return err
	}
	p.sealed, p.blind = sealed, nil
	for _, path := range sp.blindPaths(p.t) {
		v := storekit.At(a, p.state, p.id, path)
		if v.Kind == reasonr.KindNone {
			continue
		}
		b, err := sp.crypto.Blind(ctx, seal, a, p.t, path, v)
		if err != nil {
			return err
		}
		refs, err := subjects(int(path[0]))
		if err != nil {
			return err
		}
		p.blind = append(p.blind, blindKey{path: path, subject: refs[0], value: b})
	}
	if p.data, err = sp.codec.AppendRecord(nil, a, stripped.Record()); err != nil {
		return fmt.Errorf("memory: state of %s: %w", ti.Name, err)
	}
	p.evArgs, p.evStripped, p.evSealed = nil, nil, nil
	for _, ev := range p.events {
		mi := &ti.Methods[ev.Name]
		es, esealed, err := sp.crypto.Seal(ctx, seal, a, ti.Name+"."+mi.Name, mi.Params, ev.Args.Value(),
			sp.crypto.SubjectsOf(a, mi.Params, p.state, p.id))
		if err != nil {
			return err
		}
		data, err := sp.codec.AppendFields(nil, a, mi.Params, es.Record())
		if err != nil {
			return fmt.Errorf("memory: event %s.%s: %w", ti.Name, mi.Name, err)
		}
		p.evArgs = append(p.evArgs, data)
		p.evStripped = append(p.evStripped, es)
		p.evSealed = append(p.evSealed, esealed)
	}
	return nil
}

// blindPaths returns the personal index keys of type t without repeats.
func (sp *space) blindPaths(t reasonr.TypeRef) []reasonr.FieldPath {
	if paths, ok := sp.blindKeys[t]; ok {
		return paths
	}
	var paths []reasonr.FieldPath
	ti := sp.schema.Type(t)
	for i := range ti.Indexes {
	keys:
		for _, k := range ti.Indexes[i].Keys {
			if !sp.crypto.Personal(t, k.Field) {
				continue
			}
			for _, p := range paths {
				if samePath(p, k.Field) {
					continue keys
				}
			}
			paths = append(paths, k.Field)
		}
	}
	sp.blindKeys[t] = paths
	return paths
}

func samePath(x, y reasonr.FieldPath) bool {
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// blindOf returns the blind value of the key path; ok == false — there is
// none (the value is none or forgotten).
func blindOf(blind []blindKey, path reasonr.FieldPath) (string, bool) {
	for _, b := range blind {
		if samePath(b.path, path) {
			return b.value, true
		}
	}
	return "", false
}

// blindMatch checks the search conditions on personal fields.
func blindMatch(blind []blindKey, conds []storekit.BlindCond) bool {
	for _, c := range conds {
		v, ok := blindOf(blind, c.Field)
		if len(c.Values) == 0 { // == none
			if ok {
				return false
			}
			continue
		}
		match := false
		for _, w := range c.Values {
			if ok && v == w {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	return true
}

// open decrypts the personal fields of the row r into its copy rec (in the arena a).
func (sp *space) open(ctx context.Context, seal *pii.Sealer, a *reasonr.Arena, r *row, rec reasonr.Value) error {
	if len(r.sealed) == 0 {
		return nil
	}
	ti := sp.schema.Type(r.t)
	return sp.crypto.Open(ctx, seal, a, ti.Name, ti.Fields, rec, r.sealed)
}

// setSubjects updates the row's subjects and the bySubject index.
func (sp *space) setSubjects(r *row) {
	for _, s := range r.subjects {
		if m := sp.bySubject[s]; m != nil {
			delete(m, r)
			if len(m) == 0 {
				delete(sp.bySubject, s)
			}
		}
	}
	r.subjects = storekit.SubjectRefs(r.sealed)
	for _, b := range r.blind {
		if !hasRef(r.subjects, b.subject) {
			r.subjects = append(r.subjects, b.subject)
		}
	}
	for _, s := range r.subjects {
		m := sp.bySubject[s]
		if m == nil {
			m = map[*row]bool{}
			sp.bySubject[s] = m
		}
		m[r] = true
	}
}

func hasRef(refs []port.SubjectRef, r port.SubjectRef) bool {
	for _, x := range refs {
		if x == r {
			return true
		}
	}
	return false
}

// checkKeys checks the key versions the unit of work encrypted with
// against the live ones (RFC §20): a key destroyed after encryption is a
// conflict, and the kernel repeats the command.
func (s *Store) checkKeys(tenant port.Tenant, seal *pii.Sealer) error {
	for _, kv := range seal.Used() {
		if v, live := s.keys.Version(tenant, kv.Subject); !live || v != kv.Version {
			return port.ErrConflict
		}
	}
	return nil
}

// forget destroys the subject keys and erases their ciphertexts and blind
// values in the tenant's rows; the row versions grow, so concurrent
// commands get a conflict and reread the data.
func (s *Store) forget(tenant port.Tenant, sp *space, a *reasonr.Arena, subjects []reasonr.Subject) ([]port.SubjectRef, error) {
	refs, err := sp.refs(a, subjects)
	if err != nil {
		return nil, err
	}
	s.keys.Destroy(tenant, refs)
	return refs, sp.erase(refs, nil)
}

// refs returns the key store references to the subjects.
func (sp *space) refs(a *reasonr.Arena, subjects []reasonr.Subject) ([]port.SubjectRef, error) {
	refs := make([]port.SubjectRef, 0, len(subjects))
	for _, subj := range subjects {
		ref, err := sp.crypto.Ref(a, subj.Type, subj.ID)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// erase erases the ciphertexts and blind values of the subjects refs in the
// rows of the types only (nil — of all) and increases these rows' versions.
func (sp *space) erase(refs []port.SubjectRef, only []reasonr.TypeRef) error {
	for _, ref := range refs {
		for r := range sp.bySubject[ref] {
			if only != nil && !slices.Contains(only, r.t) {
				continue
			}
			sealed := r.sealed[:0:0]
			for _, sv := range r.sealed {
				if !hasRef(sv.Subjects, ref) {
					sealed = append(sealed, sv)
				}
			}
			blind := r.blind[:0:0]
			for _, b := range r.blind {
				if b.subject != ref {
					blind = append(blind, b)
				}
			}
			r.sealed, r.blind = sealed, blind
			r.version++
			if err := sp.index(r); err != nil {
				return err
			}
			sp.setSubjects(r)
		}
	}
	return nil
}

// Dump returns everything the store keeps, as is: rows, events,
// ciphertexts, blind values, the outbox, timers, responses by key and the
// event store. To check that personal data is not kept as plaintext.
func (s *Store) Dump() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b []byte
	sealed := func(ss []storekit.Sealed) {
		for _, sv := range ss {
			b = append(b, sv.Data...)
		}
	}
	for _, sp := range s.spaces {
		for _, tab := range sp.rows {
			for _, r := range tab {
				b = append(b, r.data...)
				b = r.a.AppendFormat(b, r.state)
				sealed(r.sealed)
				for _, bk := range r.blind {
					b = append(b, bk.value...)
				}
				for _, ev := range r.events {
					b = append(b, ev.Args...)
					sealed(ev.Sealed)
				}
			}
		}
		for _, tm := range sp.timers {
			b = tm.a.AppendFormat(b, tm.args.Value())
			b = append(b, tm.sealed...)
		}
		for _, r := range sp.keys {
			b = append(b, r.data...)
		}
		for _, r := range sp.requests {
			b = r.a.AppendFormat(b, r.rq.Args.Value())
			sealed(r.sealed)
		}
	}
	for i := range s.outbox {
		e := &s.outbox[i]
		a := e.unit.Arena
		for _, ee := range e.unit.Entities {
			for _, ev := range ee.Events {
				b = a.AppendFormat(b, ev.Args.Value())
			}
		}
		for _, op := range e.unit.Projections {
			for _, fv := range op.Set {
				b = a.AppendFormat(b, fv.Value)
			}
		}
		for _, rq := range e.unit.Requests {
			b = a.AppendFormat(b, rq.Args.Value())
		}
		for _, group := range [][][]storekit.Sealed{e.events, e.ops, e.requests} {
			for _, ss := range group {
				sealed(ss)
			}
		}
	}
	return s.events.dump(b)
}

// DropKeys drops from the cache the keys of subjects another instance
// forgot (ports.KeyCache).
func (s *Store) DropKeys(tenant port.Tenant, subjects []port.SubjectRef) {
	s.ring.Forgotten(tenant, subjects)
}

// Keyring returns the store's keys (pii.Shared).
func (s *Store) Keyring() *pii.Keyring { return s.ring }

// UseKeyring replaces the store's keys with the main store's (pii.Shared):
// this is how the event store and asynchronous projections in a separate
// database work.
func (s *Store) UseKeyring(k *pii.Keyring) { s.ring = k }

// TenantKeys returns the tenant keys (port.TenantKeyRotator).
func (s *Store) TenantKeys(ctx context.Context) (map[port.Tenant][]byte, error) {
	return s.keys.TenantKeys(ctx)
}

// ReplaceTenantKey replaces a tenant's wrapped key (port.TenantKeyRotator).
func (s *Store) ReplaceTenantKey(ctx context.Context, tenant port.Tenant, old, wrapped []byte) (bool, error) {
	return s.keys.ReplaceTenantKey(ctx, tenant, old, wrapped)
}
