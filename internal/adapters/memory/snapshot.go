package memory

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/pii"
	"github.com/tycheg/reasonr-engine/internal/storekit"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// A store snapshot: the state of a memory store at a position. It is
// written as a stream: rows, events, timers, external requests, responses
// by key, the outbox, personal data keys, the flag rollout. The event
// journal (Events) is not part of the snapshot: an engine kind keeps it in a
// separate store.

const snapshotVersion = 1

// Snapshot writes the store state to w.
func (s *Store) Snapshot(w io.Writer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sw := &snapWriter{w: bufio.NewWriterSize(w, 1<<20)}
	sw.u(snapshotVersion)
	sw.u(uint64(s.position))
	sw.u(s.timerSeq)
	keys, err := json.Marshal(s.keys.Export())
	if err != nil {
		return err
	}
	sw.raw(keys)
	meta, err := json.Marshal(struct {
		Forgets  []port.Forgotten
		Features []port.FeatureRule
		FlagLog  []port.FeatureChange
	}{s.forgets, s.features, s.flagLog})
	if err != nil {
		return err
	}
	sw.raw(meta)
	tenants := make([]port.Tenant, 0, len(s.spaces))
	for t := range s.spaces {
		tenants = append(tenants, t)
	}
	sort.Slice(tenants, func(i, j int) bool { return tenants[i] < tenants[j] })
	sw.u(uint64(len(tenants)))
	for _, t := range tenants {
		sw.str(string(t))
		sw.space(s.spaces[t])
	}
	sw.u(uint64(len(s.outbox)))
	for i := range s.outbox {
		sw.entry(&s.outbox[i])
	}
	if sw.err != nil {
		return sw.err
	}
	return sw.w.Flush()
}

// Restore replaces the state of an empty store with a snapshot; the
// snapshot's tenant schemas must be in the catalog.
func (s *Store) Restore(r io.Reader) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sr := &snapReader{r: bufio.NewReaderSize(r, 1<<20)}
	if v := sr.u(); sr.err == nil && v != snapshotVersion {
		return fmt.Errorf("memory: snapshot version %d, want %d", v, snapshotVersion)
	}
	s.position = port.Position(sr.u())
	s.timerSeq = sr.u()
	var keys pii.KeysSnapshot
	if b := sr.raw(); sr.err == nil {
		if err := json.Unmarshal(b, &keys); err != nil {
			return err
		}
	}
	s.keys.Import(keys)
	var meta struct {
		Forgets  []port.Forgotten
		Features []port.FeatureRule
		FlagLog  []port.FeatureChange
	}
	if b := sr.raw(); sr.err == nil {
		if err := json.Unmarshal(b, &meta); err != nil {
			return err
		}
	}
	s.forgets, s.features, s.flagLog = meta.Forgets, meta.Features, meta.FlagLog
	s.spaces = map[port.Tenant]*space{}
	for range sr.count() {
		t := port.Tenant(sr.str())
		if sr.err != nil {
			break
		}
		sp, err := s.space(t)
		if err != nil {
			return fmt.Errorf("memory: snapshot: tenant %s: %w", t, err)
		}
		if err := sr.space(sp); err != nil {
			return err
		}
	}
	s.outbox = nil
	for range sr.count() {
		e := sr.entry()
		if sr.err != nil {
			break
		}
		s.outbox = append(s.outbox, e)
	}
	return sr.err
}

// ---------------------------------------------------------------------------

type snapWriter struct {
	w   *bufio.Writer
	buf []byte
	err error
}

func (w *snapWriter) write(p []byte) {
	if w.err == nil {
		_, w.err = w.w.Write(p)
	}
}

func (w *snapWriter) u(v uint64) {
	w.buf = binary.AppendUvarint(w.buf[:0], v)
	w.write(w.buf)
}

func (w *snapWriter) i(v int64) {
	w.buf = binary.AppendVarint(w.buf[:0], v)
	w.write(w.buf)
}

func (w *snapWriter) raw(p []byte) {
	w.u(uint64(len(p)))
	w.write(p)
}

func (w *snapWriter) str(s string) { w.raw([]byte(s)) }

func (w *snapWriter) time(t time.Time) {
	if t.IsZero() {
		w.i(math.MinInt64)
		return
	}
	w.i(t.UnixNano())
}

func (w *snapWriter) value(a *reasonr.Arena, v reasonr.Value) { w.raw(reasonr.AppendValue(nil, a, v)) }

func (w *snapWriter) sealed(ss []storekit.Sealed) {
	w.u(uint64(len(ss)))
	for _, s := range ss {
		w.u(uint64(s.Field))
		w.refs(s.Subjects)
		w.raw(s.Data)
		w.str(s.Member)
	}
}

func (w *snapWriter) refs(rs []port.SubjectRef) {
	w.u(uint64(len(rs)))
	for _, r := range rs {
		w.str(r.Type)
		w.str(r.ID)
	}
}

func (w *snapWriter) subjects(a *reasonr.Arena, ss []reasonr.ArgSubject) {
	w.u(uint64(len(ss)))
	for _, s := range ss {
		w.u(uint64(s.Arg))
		w.u(uint64(s.Subject.Type))
		w.value(a, s.Subject.ID)
	}
}

func (w *snapWriter) space(sp *space) {
	types := make([]reasonr.TypeRef, 0, len(sp.seqs))
	for t := range sp.seqs {
		types = append(types, t)
	}
	sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })
	w.u(uint64(len(types)))
	for _, t := range types {
		w.u(uint64(t))
		w.i(sp.seqs[t])
	}
	var rows []*row
	for _, tab := range sp.rows {
		for _, r := range tab {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].t != rows[j].t {
			return rows[i].t < rows[j].t
		}
		return rows[i].key < rows[j].key
	})
	w.u(uint64(len(rows)))
	for _, r := range rows {
		w.u(uint64(r.t))
		w.str(r.key)
		w.value(r.a, r.id)
		w.raw(r.data)
		w.u(r.version)
		w.sealed(r.sealed)
		w.u(uint64(len(r.blind)))
		for _, b := range r.blind {
			w.u(uint64(len(b.path)))
			for _, f := range b.path {
				w.u(uint64(f))
			}
			w.str(b.subject.Type)
			w.str(b.subject.ID)
			w.str(b.value)
		}
		w.u(uint64(len(r.events)))
		for _, e := range r.events {
			w.u(uint64(e.Position))
			w.u(uint64(e.Name))
			w.raw(e.Args)
			w.sealed(e.Sealed)
		}
	}
	w.u(uint64(len(sp.timers)))
	for _, t := range sp.timers {
		w.value(t.a, t.id)
		w.time(t.at)
		w.i(int64(t.offset))
		w.u(t.order)
		w.u(uint64(t.entity))
		w.value(t.a, t.entityID)
		w.u(uint64(t.command))
		w.value(t.a, t.args.Value())
		w.raw(t.sealed)
		w.time(t.leaseUntil)
		w.u(uint64(t.attempts))
		w.str(t.dead)
	}
	w.u(uint64(len(sp.requests)))
	for key, r := range sp.requests {
		w.write(key[:])
		w.u(uint64(r.position))
		w.u(uint64(r.index))
		w.time(r.at)
		w.write(r.timer[:])
		w.time(r.next)
		w.u(uint64(r.attempts))
		w.str(r.last)
		w.u(uint64(r.rq.System))
		w.u(uint64(r.rq.Request))
		w.value(r.a, r.rq.Args.Value())
		w.value(r.a, r.rq.Key)
		w.sealed(r.sealed)
	}
	w.u(uint64(len(sp.keys)))
	for key, k := range sp.keys {
		w.str(key)
		w.raw(k.data)
		w.time(k.expires)
	}
	w.u(uint64(sp.applied))
	w.u(uint64(len(sp.deleted)))
	for k, v := range sp.deleted {
		w.u(uint64(k.t))
		w.str(k.key)
		w.u(v)
	}
}

func (w *snapWriter) entry(e *outboxEntry) {
	u, a := &e.unit, e.unit.Arena
	w.str(string(u.Tenant))
	w.u(uint64(u.Position))
	w.time(u.At)
	w.u(boolBit(u.Contexts != nil))
	w.u(uint64(len(u.Contexts)))
	for _, c := range u.Contexts {
		w.str(c)
	}
	w.u(uint64(len(u.Entities)))
	for _, ee := range u.Entities {
		w.u(uint64(ee.Type))
		w.value(a, ee.ID)
		w.u(ee.Version)
		w.u(uint64(len(ee.Events)))
		for _, ev := range ee.Events {
			w.u(uint64(ev.Name))
			w.value(a, ev.Args.Value())
			w.u(uint64(ev.Seq))
			w.subjects(a, ev.Subjects)
		}
	}
	w.u(uint64(len(u.Projections)))
	for _, op := range u.Projections {
		w.u(uint64(op.Kind))
		w.u(uint64(op.Projection))
		w.value(a, op.ID)
		w.u(uint64(op.Seq))
		w.u(uint64(len(op.Set)))
		for _, fv := range op.Set {
			w.u(uint64(fv.Field))
			w.value(a, fv.Value)
		}
	}
	w.u(uint64(len(u.Requests)))
	for _, rq := range u.Requests {
		w.u(uint64(rq.System))
		w.u(uint64(rq.Request))
		w.value(a, rq.Args.Value())
		w.value(a, rq.Key)
		w.subjects(a, rq.Subjects)
	}
	w.u(uint64(len(u.Forget)))
	for _, f := range u.Forget {
		w.u(uint64(f.Type))
		w.value(a, f.ID)
	}
	w.u(uint64(len(u.Calls)))
	for _, c := range u.Calls {
		w.u(uint64(c.System))
		w.u(uint64(c.Entity))
		w.value(a, c.ID)
		w.u(uint64(c.Command))
		w.value(a, c.Args.Value())
	}
	w.u(uint64(len(u.Keys)))
	for _, k := range u.Keys {
		w.str(k.Subject.Type)
		w.str(k.Subject.ID)
		w.u(uint64(k.Version))
	}
	for _, group := range [][][]storekit.Sealed{e.events, e.ops, e.requests} {
		w.u(uint64(len(group)))
		for _, ss := range group {
			w.sealed(ss)
		}
	}
}

func boolBit(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------

type snapReader struct {
	r   *bufio.Reader
	err error
}

func (r *snapReader) fail(err error) {
	if r.err == nil {
		r.err = err
	}
}

func (r *snapReader) u() uint64 {
	if r.err != nil {
		return 0
	}
	v, err := binary.ReadUvarint(r.r)
	r.fail(err)
	return v
}

func (r *snapReader) i() int64 {
	if r.err != nil {
		return 0
	}
	v, err := binary.ReadVarint(r.r)
	r.fail(err)
	return v
}

// count is a number of elements: at most 2³², otherwise the snapshot is corrupt.
func (r *snapReader) count() uint64 {
	n := r.u()
	if n > math.MaxUint32 {
		r.fail(fmt.Errorf("memory: corrupt snapshot: %d elements", n))
		return 0
	}
	return n
}

func (r *snapReader) bytes(n int) []byte {
	if r.err != nil {
		return nil
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r.r, b)
	r.fail(err)
	return b
}

func (r *snapReader) raw() []byte {
	n := r.u()
	if n > 1<<31 {
		r.fail(fmt.Errorf("memory: corrupt snapshot: a record of length %d", n))
		return nil
	}
	return r.bytes(int(n))
}

func (r *snapReader) str() string { return string(r.raw()) }

func (r *snapReader) time() time.Time {
	n := r.i()
	if n == math.MinInt64 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

func (r *snapReader) value(a *reasonr.Arena) reasonr.Value {
	b := r.raw()
	if r.err != nil {
		return reasonr.None
	}
	v, _, err := reasonr.ReadValue(b, a)
	r.fail(err)
	return v
}

func (r *snapReader) sealed() []storekit.Sealed {
	var out []storekit.Sealed
	for range r.count() {
		out = append(out, storekit.Sealed{Field: int(r.u()), Subjects: r.refs(), Data: r.raw(), Member: r.str()})
	}
	return out
}

func (r *snapReader) refs() []port.SubjectRef {
	var out []port.SubjectRef
	for range r.count() {
		out = append(out, port.SubjectRef{Type: r.str(), ID: r.str()})
	}
	return out
}

func (r *snapReader) subjects(a *reasonr.Arena) []reasonr.ArgSubject {
	var out []reasonr.ArgSubject
	for range r.count() {
		s := reasonr.ArgSubject{Arg: int(r.u())}
		s.Subject.Type = reasonr.TypeRef(r.u())
		s.Subject.ID = r.value(a)
		out = append(out, s)
	}
	return out
}

func (r *snapReader) space(sp *space) error {
	for range r.count() {
		t := reasonr.TypeRef(r.u())
		sp.seqs[t] = r.i()
	}
	src := reasonr.NewArena()
	for range r.count() {
		t := reasonr.TypeRef(r.u())
		key := r.str()
		id := r.value(src)
		data := r.raw()
		version := r.u()
		sealed := r.sealed()
		var blind []blindKey
		for range r.count() {
			var b blindKey
			for range r.count() {
				b.path = append(b.path, reasonr.FieldRef(r.u()))
			}
			b.subject = port.SubjectRef{Type: r.str(), ID: r.str()}
			b.value = r.str()
			blind = append(blind, b)
		}
		var events []storedEvent
		for range r.count() {
			events = append(events, storedEvent{Position: port.Position(r.u()), Name: reasonr.MethodRef(r.u()), Args: r.raw(), Sealed: r.sealed()})
		}
		if r.err != nil {
			return r.err
		}
		tab := sp.rows[t]
		if tab == nil {
			tab = map[string]*row{}
			sp.rows[t] = tab
		}
		rw := &row{t: t, key: key, events: events}
		tab[key] = rw
		if err := sp.store(rw, src, id, data, version, sealed, blind); err != nil {
			return err
		}
	}
	for range r.count() {
		a := reasonr.NewArena()
		tm := &timer{a: a, id: r.value(a), at: r.time(), offset: time.Duration(r.i()), order: r.u(), entity: reasonr.TypeRef(r.u())}
		tm.entityID = r.value(a)
		tm.command = reasonr.MethodRef(r.u())
		tm.args = r.value(a).Record()
		tm.sealed = r.raw()
		tm.leaseUntil = r.time()
		tm.attempts = int(r.u())
		tm.dead = r.str()
		if r.err != nil {
			return r.err
		}
		sp.timers[tm.id.UUID()] = tm
	}
	if n := r.count(); n > 0 {
		if sp.requests == nil {
			sp.requests = map[[16]byte]*request{}
		}
		for range n {
			var key [16]byte
			copy(key[:], r.bytes(16))
			rq := &request{position: port.Position(r.u()), index: int(r.u()), at: r.time()}
			copy(rq.timer[:], r.bytes(16))
			rq.next, rq.attempts, rq.last = r.time(), int(r.u()), r.str()
			rq.a = reasonr.NewArena()
			rq.rq = reasonr.ExternalRequest{System: reasonr.TypeRef(r.u()), Request: reasonr.MethodRef(r.u())}
			rq.rq.Args = r.value(rq.a).Record()
			rq.rq.Key = r.value(rq.a)
			rq.sealed = r.sealed()
			sp.requests[key] = rq
		}
	}
	for range r.count() {
		key := r.str()
		sp.keys[key] = &remembered{data: r.raw(), expires: r.time()}
	}
	sp.applied = port.Position(r.u())
	for range r.count() {
		k := rowKey{t: reasonr.TypeRef(r.u()), key: r.str()}
		sp.deleted[k] = r.u()
	}
	return r.err
}

func (r *snapReader) entry() outboxEntry {
	a := reasonr.NewArena()
	u := kind.CommittedUnit{Arena: a, Tenant: port.Tenant(r.str()), Position: port.Position(r.u()), At: r.time()}
	has := r.u() == 1
	for range r.count() {
		u.Contexts = append(u.Contexts, r.str())
	}
	if has && u.Contexts == nil {
		u.Contexts = []string{}
	}
	for range r.count() {
		ee := kind.EntityEvents{Type: reasonr.TypeRef(r.u()), ID: r.value(a), Version: r.u()}
		for range r.count() {
			ev := reasonr.Event{Name: reasonr.MethodRef(r.u()), Args: r.value(a).Record(), Seq: uint32(r.u())}
			ev.Subjects = r.subjects(a)
			ee.Events = append(ee.Events, ev)
		}
		u.Entities = append(u.Entities, ee)
	}
	for range r.count() {
		op := reasonr.ProjectionOp{Kind: reasonr.ProjectionOpKind(r.u()), Projection: reasonr.TypeRef(r.u()), ID: r.value(a), Seq: uint32(r.u())}
		for range r.count() {
			op.Set = append(op.Set, reasonr.FieldValue{Field: reasonr.FieldRef(r.u()), Value: r.value(a)})
		}
		u.Projections = append(u.Projections, op)
	}
	for range r.count() {
		rq := reasonr.ExternalRequest{System: reasonr.TypeRef(r.u()), Request: reasonr.MethodRef(r.u())}
		rq.Args = r.value(a).Record()
		rq.Key = r.value(a)
		rq.Subjects = r.subjects(a)
		u.Requests = append(u.Requests, rq)
	}
	for range r.count() {
		u.Forget = append(u.Forget, reasonr.Subject{Type: reasonr.TypeRef(r.u()), ID: r.value(a)})
	}
	for range r.count() {
		c := reasonr.FakeCall{System: reasonr.TypeRef(r.u()), Entity: reasonr.TypeRef(r.u()), ID: r.value(a), Command: reasonr.MethodRef(r.u())}
		c.Args = r.value(a).Record()
		u.Calls = append(u.Calls, c)
	}
	for range r.count() {
		u.Keys = append(u.Keys, port.KeyVersion{Subject: port.SubjectRef{Type: r.str(), ID: r.str()}, Version: uint32(r.u())})
	}
	e := outboxEntry{unit: u}
	for g := range 3 {
		var group [][]storekit.Sealed
		for range r.count() {
			group = append(group, r.sealed())
		}
		switch g {
		case 0:
			e.events = group
		case 1:
			e.ops = group
		case 2:
			e.requests = group
		}
	}
	return e
}
