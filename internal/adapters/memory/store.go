package memory

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/bus"
	"github.com/tycheg/reasonr-engine/internal/codecs"
	"github.com/tycheg/reasonr-engine/internal/pii"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/internal/storekit"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Store is an in-memory store: the main store (port.UnitStore), an
// asynchronous projection store (port.ProjectionStore), a timer source
// (port.TimerSource), the outbox (port.EffectSource) and external requests
// waiting to be sent (port.RequestQueue). Tenant data is separated; model
// schemas come from the catalog. Its methods are safe for concurrent
// calls. It is the reference behavior for the other adapters.
//
// Personal data is stored encrypted with subject keys (RFC §20): rows,
// events and the outbox keep values without it, and the ciphertexts next
// to them; the keys are in the in-memory key store.
type Store struct {
	catalog kind.Catalog
	keys    *pii.MemoryKeys
	ring    *pii.Keyring

	mu       sync.Mutex
	spaces   map[port.Tenant]*space
	position port.Position
	outbox   []outboxEntry        // values in their own arenas, personal ones as ciphertexts
	wake     chan struct{}        // closes on a new outbox entry
	timerSeq uint64               // the timer scheduling order
	events   *Events              // the event store, when Store is an EventStore
	forgets  []port.Forgotten     // the forget journal, when Store is a port.ForgetLog
	features []port.FeatureRule   // the feature flag rollout (port.FeatureStore)
	flagLog  []port.FeatureChange // the rollout change journal
}

var (
	_ kind.UnitStore       = (*Store)(nil)
	_ kind.ProjectionStore = (*Store)(nil)
	_ kind.TimerSource     = (*Store)(nil)
	_ kind.EffectSource    = (*Store)(nil)
	_ ports.EventStore     = (*Store)(nil)
	_ port.EventFeed       = (*Store)(nil)
	_ kind.EntitySink      = (*Store)(nil)
)

// NewStore returns an empty store with the schemas from the catalog.
func NewStore(catalog kind.Catalog) *Store {
	return NewStoreKMS(catalog, pii.NewMemoryKMS())
}

// NewStoreKMS returns an empty store whose tenant keys kms wraps.
func NewStoreKMS(catalog kind.Catalog, kms port.KMS) *Store {
	return NewStoreKeys(catalog, kms, pii.Options{})
}

// NewStoreKeys returns an empty store with the key settings opts: an engine
// kind sets the clock and the randomness source of the unit of work in
// them, so that copies of the store encrypt identically.
func NewStoreKeys(catalog kind.Catalog, kms port.KMS, opts pii.Options) *Store {
	keys := pii.NewMemoryKeys()
	return &Store{
		catalog: catalog, keys: keys, ring: pii.NewKeyring(kms, keys, opts),
		spaces: map[port.Tenant]*space{}, wake: make(chan struct{}), events: NewEvents(),
	}
}

// space holds the data of one tenant.
type space struct {
	schema   *reasonr.Schema
	codec    *reasonr.RecordCodec
	json     *codecs.Values            // responses by key: personal values are none, the ciphertexts next to them
	seqs     map[reasonr.TypeRef]int64 // sequence counters
	rows     map[reasonr.TypeRef]map[string]*row
	timers   map[[16]byte]*timer
	requests map[[16]byte]*request  // external requests waiting to be sent, by key
	keys     map[string]*remembered // responses by idempotency key
	uniq     map[uniqKey]string     // taken unique index values → the row key
	applied  port.Position          // as an asynchronous projection store: the greatest applied position
	deleted  map[rowKey]uint64      // deletion marks of asynchronous projection rows: the deletion version
	crypto   *storekit.Crypto
	// bySubject holds the rows with a subject's data: forget erases them
	// without walking the tenant (RFC §20).
	bySubject map[port.SubjectRef]map[*row]bool
	blindKeys map[reasonr.TypeRef][]reasonr.FieldPath // the personal index keys of a type
}

// row is an entity or a projection row: the canonical encoding of the
// state without personal values, and the same decoded into the row's own
// arena; personal values are the ciphertexts sealed, and the index keys
// over them are the blind values blind.
type row struct {
	t        reasonr.TypeRef
	a        *reasonr.Arena
	id       reasonr.Value
	state    reasonr.Value // the record: fields by schema
	data     []byte
	version  uint64
	events   []storedEvent // for entities
	key      string        // the row key in the table
	uniq     []uniqKey     // the row's unique index values (without none)
	sealed   []storekit.Sealed
	blind    []blindKey
	subjects []port.SubjectRef
}

// blindKey is the blind value of the personal index key path of a row and
// the subject it belongs to.
type blindKey struct {
	path    reasonr.FieldPath
	subject port.SubjectRef
	value   string
}

// uniqKey is a unique index key value: the canonical encoding of the key
// values. The in-memory uniqueness index answers "whose row takes this
// value" in O(1) instead of walking the rows of the type.
type uniqKey struct {
	t   reasonr.TypeRef
	ix  reasonr.IndexRef
	key string
}

// storedEvent is an event in an entity's journal: the arguments without
// personal values in the canonical encoding by the event parameters, and
// the ciphertexts.
type storedEvent struct {
	Position port.Position
	Name     reasonr.MethodRef
	Args     []byte
	Sealed   []storekit.Sealed
}

// kit returns the row for storekit comparisons.
func (r *row) kit() storekit.Row { return storekit.Row{A: r.a, State: r.state, ID: r.id} }

// space returns the tenant's data, creating it; called under the lock.
func (s *Store) space(t port.Tenant) (*space, error) {
	if sp := s.spaces[t]; sp != nil {
		return sp, nil
	}
	schema, err := s.catalog.Schema(t)
	if err != nil {
		return nil, err
	}
	sp := &space{
		schema: schema, codec: reasonr.NewRecordCodec(schema),
		seqs: map[reasonr.TypeRef]int64{}, rows: map[reasonr.TypeRef]map[string]*row{},
		timers: map[[16]byte]*timer{}, keys: map[string]*remembered{},
		uniq: map[uniqKey]string{}, deleted: map[rowKey]uint64{}, crypto: storekit.NewCrypto(schema),
		bySubject: map[port.SubjectRef]map[*row]bool{}, blindKeys: map[reasonr.TypeRef][]reasonr.FieldPath{},
	}
	sp.json = codecs.JSONOf(schema, codecs.Options{SealedPersonal: true})
	s.spaces[t] = sp
	return sp, nil
}

// Append appends events to the store journal (ports.EventStore): personal
// arguments are encrypted with the keys of the subjects from Event.Subjects
// of the versions CommittedUnit.Keys, as in ev_events of PostgreSQL.
func (s *Store) Append(ctx context.Context, units []kind.CommittedUnit) error {
	out := make([]kind.CommittedUnit, len(units))
	sealed := make([][]byte, len(units))
	for i, u := range units {
		s.mu.Lock()
		sp, err := s.space(u.Tenant)
		s.mu.Unlock()
		if err != nil {
			return err
		}
		seal := s.ring.Resealer(u.Tenant, u.Keys)
		u.Entities = slices.Clone(u.Entities)
		for ei := range u.Entities {
			ee := &u.Entities[ei]
			ti := sp.schema.Type(ee.Type)
			ee.Events = slices.Clone(ee.Events)
			for k := range ee.Events {
				ev := &ee.Events[k]
				if len(ev.Subjects) == 0 {
					continue
				}
				mi := &ti.Methods[ev.Name]
				args, ss, err := sp.crypto.Seal(ctx, seal, u.Arena, ti.Name+"."+mi.Name, mi.Params, ev.Args.Value(), sp.crypto.ArgSubjects(u.Arena, ev.Subjects))
				if err != nil {
					return err
				}
				ev.Args = args.Record()
				for _, sv := range ss {
					sealed[i] = append(sealed[i], sv.Data...)
				}
			}
		}
		out[i] = u
	}
	s.events.append(out, sealed)
	return nil
}

// ReadEvents reads the event store (port.EventFeed). The events of a tenant
// deleted by DropTenant are skipped.
func (s *Store) ReadEvents(_ context.Context, after, upto port.Position, limit int) ([]port.EventRecord, error) {
	var out []port.EventRecord
	for _, u := range s.events.between(after, upto) {
		s.mu.Lock()
		sp := s.spaces[u.Tenant]
		s.mu.Unlock()
		if sp == nil {
			continue
		}
		enc := bus.NewEncoder(sp.schema)
		for _, ee := range u.Entities {
			for i := range ee.Events {
				m, err := enc.Event(u.Tenant, u.Position, u.At, u.Arena, ee.Type, ee.ID, &ee.Events[i])
				if err != nil {
					return nil, err
				}
				out = append(out, m)
				if limit > 0 && len(out) == limit {
					return out, nil
				}
			}
		}
	}
	return out, nil
}

// Journal returns the store's event store.
func (s *Store) Journal() *Events { return s.events }

// DropTenant deletes a tenant's data (the ephemeral tenants of tests).
func (s *Store) DropTenant(_ context.Context, t port.Tenant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.spaces, t)
	s.keys.DropTenant(t)
	s.ring.DropTenant(t)
	return nil
}

// Capabilities: memory can do everything except full-text search.
func (s *Store) Capabilities() port.Capabilities {
	return port.Capabilities{Transactions: true, Unique: true, Conds: port.AllConds, SortDesc: true, Pages: true, Scan: true, Personal: true}
}

// key returns the key of an identifier: the canonical encoding.
func (sp *space) key(a *reasonr.Arena, t reasonr.TypeRef, id reasonr.Value) (string, error) {
	b, err := sp.codec.Append(nil, a, reasonr.Type{Kind: reasonr.KindID, Ref: t}, id)
	if err != nil {
		return "", fmt.Errorf("memory: identifier of %s: %w", sp.typeName(t), err)
	}
	return string(b), nil
}

func (sp *space) typeName(t reasonr.TypeRef) string {
	if ti := sp.schema.Type(t); ti != nil {
		return ti.Name
	}
	return fmt.Sprint(t)
}

// row returns a row or nil.
func (sp *space) row(a *reasonr.Arena, t reasonr.TypeRef, id reasonr.Value) (*row, string, error) {
	k, err := sp.key(a, t, id)
	if err != nil {
		return nil, "", err
	}
	return sp.rows[t][k], k, nil
}

// NextSequence allocates n sequence numbers of type t (port.UnitStore).
func (s *Store) NextSequence(_ context.Context, tenant port.Tenant, t reasonr.TypeRef, n int) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.space(tenant)
	if err != nil {
		return 0, err
	}
	sp.seqs[t] += int64(n)
	return sp.seqs[t] - int64(n) + 1, nil
}

// Load returns an entity or projection record (port.Reader).
func (s *Store) Load(ctx context.Context, tenant port.Tenant, a *reasonr.Arena, t reasonr.TypeRef, id reasonr.Value) (reasonr.Record, uint64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.space(tenant)
	if err != nil {
		return reasonr.Record{}, 0, false, err
	}
	r, _, err := sp.row(a, t, id)
	if err != nil {
		return reasonr.Record{}, 0, false, err
	}
	if r == nil {
		def, err := sp.codec.Default(a, t)
		return def, 0, false, err
	}
	st := a.Copy(r.a, r.state)
	if err := sp.open(ctx, s.ring.Sealer(tenant), a, r, st); err != nil {
		return reasonr.Record{}, 0, false, err
	}
	return st.Record(), r.version, true, nil
}

// Events returns an entity's events in order — the journal of the main
// in-memory store, for debugging and checks.
func (s *Store) Events(tenant port.Tenant, a *reasonr.Arena, t reasonr.TypeRef, id reasonr.Value) ([]reasonr.Event, error) {
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.space(tenant)
	if err != nil {
		return nil, err
	}
	r, _, err := sp.row(a, t, id)
	if err != nil || r == nil {
		return nil, err
	}
	ti := sp.schema.Type(t)
	out := make([]reasonr.Event, 0, len(r.events))
	for i, ev := range r.events {
		mi := &ti.Methods[ev.Name]
		args, err := sp.codec.DecodeFields(a, 0, mi.Params, ev.Args)
		if err != nil {
			return nil, err
		}
		if err := sp.crypto.Open(ctx, s.ring.Sealer(tenant), a, ti.Name+"."+mi.Name, mi.Params, args.Value(), ev.Sealed); err != nil {
			return nil, err
		}
		out = append(out, reasonr.Event{Name: ev.Name, Args: args, Seq: uint32(i)})
	}
	return out, nil
}

// Find runs a search (port.Reader): the rows of the type that satisfy the
// conditions, in sort order (by default — of the index keys), then by
// identifier; a page — with a cursor after the last row. Limit < 0 without
// a page — all rows (all in migrations).
func (s *Store) Find(ctx context.Context, tenant port.Tenant, a *reasonr.Arena, q *reasonr.FindSpec) (reasonr.Rows, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.space(tenant)
	if err != nil {
		return reasonr.Rows{}, err
	}
	sortKeys, err := storekit.SortKeys(sp.schema, q)
	if err != nil {
		return reasonr.Rows{}, err
	}
	seal := s.ring.Sealer(tenant)
	where, blind, err := sp.crypto.SplitConds(ctx, seal, a, q.Type, q.Where)
	if err != nil {
		return reasonr.Rows{}, err
	}
	var found []*row
	for _, r := range sp.rows[q.Type] {
		if storekit.Matches(r.a, r.state, r.id, a, where) && blindMatch(r.blind, blind) {
			found = append(found, r)
		}
	}
	sort.Slice(found, func(i, j int) bool { return storekit.Compare(found[i].kit(), found[j].kit(), sortKeys) < 0 })
	if q.Page && q.Cursor.Kind == reasonr.KindString {
		pos, err := storekit.DecodeCursor(sp.codec, a, q.Type, sortKeys, a.Data(q.Cursor))
		if err != nil {
			return reasonr.Rows{}, err
		}
		i := sort.Search(len(found), func(i int) bool { return storekit.AfterCursor(found[i].kit(), sortKeys, a, pos) > 0 })
		found = found[i:]
	}
	n := len(found)
	switch {
	case q.Page:
		n = min(n, q.Size)
	case q.Limit >= 0:
		n = min(n, q.Limit)
	}
	out := reasonr.Rows{Next: reasonr.None}
	if q.Page && n < len(found) && n > 0 {
		c, err := storekit.EncodeCursor(sp.codec, q.Type, found[n-1].kit(), sortKeys)
		if err != nil {
			return reasonr.Rows{}, err
		}
		out.Next = a.String(c)
	}
	m := a.Mark()
	for _, r := range found[:n] {
		mr := a.Mark()
		for _, f := range r.a.Elems(r.state) {
			a.Push(a.Copy(r.a, f))
		}
		a.Push(a.Copy(r.a, r.id))
		rec := a.End(reasonr.KindRecord, r.t, mr)
		if err := sp.open(ctx, seal, a, r, rec); err != nil {
			return reasonr.Rows{}, err
		}
		a.Push(rec)
	}
	out.Records = a.End(reasonr.KindList, 0, m)
	return out, nil
}
