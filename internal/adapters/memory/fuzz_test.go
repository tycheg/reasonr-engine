package memory

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/testmodels"
)

// Fuzzing the VM and the engine: the models of the FuzzLower corpus and of
// the engine tests, random calls of public commands and queries with
// random arguments and callers, clock shifts. Checked: no panics; faults
// only of the expected kinds; a repeated execution of each command gives
// the same unit of work (CheckDeterminism); after the run, the state of
// each entity equals the fold of its events.

var fuzzModels = []struct{ dir, tenant string }{
	{testmodels.Dir("taxi"), ""},
	{testmodels.Dir("taxi"), "acme"},
	{testmodels.Dir("bank"), ""},
	{testmodels.Dir("constructs"), ""},
	{filepath.Join("..", "..", "conformance", "testdata", "store"), ""},
}

var fuzzMachines []*reasonr.Model

func fuzzMachine(t testing.TB, i int) *reasonr.Model {
	if fuzzMachines == nil {
		for _, c := range fuzzModels {
			m, err := reasonr.Build(c.dir, reasonr.Options{Extension: c.tenant, Test: true})
			if err != nil {
				t.Fatal(err)
			}
			fuzzMachines = append(fuzzMachines, m)
		}
	}
	return fuzzMachines[i%len(fuzzMachines)]
}

// gen is a source of decisions from the fuzz test input; the end of the input gives zeros.
type gen struct {
	data []byte
	pos  int
	s    *reasonr.Schema
}

func (g *gen) byte() byte {
	if g.pos >= len(g.data) {
		return 0
	}
	g.pos++
	return g.data[g.pos-1]
}

func (g *gen) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(g.byte()) % n
}

func (g *gen) done() bool { return g.pos >= len(g.data) }

var fuzzStrings = []string{"", "a", "+79990000000", "John", "A001AA77", "x y", "1"}

// id is an identifier of type t from a small set: so calls hit the same
// entities.
func (g *gen) id(a *reasonr.Arena, t reasonr.TypeRef, depth int) reasonr.Value {
	n := byte(g.intn(3) + 1)
	ti := g.s.Type(t)
	base := t
	for ti != nil && ti.ID == reasonr.IDDerived {
		base = ti.IDType
		ti = g.s.Type(base)
	}
	if ti == nil {
		return reasonr.None
	}
	switch ti.ID {
	case reasonr.IDSequence:
		return reasonr.ID(t, reasonr.Bigint(int64(n)))
	case reasonr.IDGiven:
		if ti.IDType != 0 {
			return a.ID(t, g.value(a, reasonr.Type{Kind: reasonr.KindRecord, Ref: ti.IDType}, depth+1))
		}
		return a.ID(t, a.String(fuzzStrings[int(n)%len(fuzzStrings)]))
	}
	return reasonr.ID(t, reasonr.UUID([16]byte{0: byte(base), 15: n}))
}

// value is a random value of type t in the arena a.
func (g *gen) value(a *reasonr.Arena, t reasonr.Type, depth int) reasonr.Value {
	if t.Optional && g.intn(4) == 0 || depth > 4 {
		return reasonr.None
	}
	now := DefaultNow
	switch t.Kind {
	case reasonr.KindInt:
		if g.intn(8) == 0 {
			return reasonr.Int(1<<31 - 1)
		}
		return reasonr.Int(int64(int8(g.byte())))
	case reasonr.KindBigint:
		return reasonr.Bigint(int64(int8(g.byte())) * 1000)
	case reasonr.KindFloat:
		return reasonr.Float(float64(int8(g.byte())) / 4)
	case reasonr.KindDecimal:
		d := reasonr.DecFromInt(int64(int8(g.byte())) * 10)
		return reasonr.Decimal(d)
	case reasonr.KindString:
		return a.String(fuzzStrings[g.intn(len(fuzzStrings))])
	case reasonr.KindBytes:
		return a.Bytes([]byte{g.byte()})
	case reasonr.KindBool:
		return reasonr.Bool(g.intn(2) == 1)
	case reasonr.KindUUID:
		return reasonr.UUID([16]byte{15: g.byte()})
	case reasonr.KindDate:
		return reasonr.DateDays(739000 + int64(g.byte()))
	case reasonr.KindTime:
		return reasonr.TimeOfDay(time.Duration(g.intn(24)) * time.Hour)
	case reasonr.KindTimestamp:
		return reasonr.Timestamp(now.Add(time.Duration(int8(g.byte())) * time.Minute))
	case reasonr.KindDuration:
		return reasonr.Duration(time.Duration(g.byte()) * time.Second)
	case reasonr.KindEnum:
		return reasonr.Enum(t.Ref, g.intn(len(g.s.Type(t.Ref).Enum)))
	case reasonr.KindID:
		return g.id(a, t.Ref, depth)
	case reasonr.KindTimerID:
		return reasonr.TimerID([16]byte{15: g.byte()})
	case reasonr.KindRecord:
		ti := g.s.Type(t.Ref)
		vals := make([]reasonr.Value, len(ti.Fields))
		for i := range ti.Fields {
			vals[i] = g.value(a, ti.Fields[i].Type, depth+1)
		}
		return a.Record(t.Ref, vals...).Value()
	case reasonr.KindList, reasonr.KindSet, reasonr.KindMap:
		m := a.Mark()
		n := g.intn(3)
		for range n {
			k := g.value(a, *t.Elem, depth+1)
			if t.Kind == reasonr.KindMap {
				k = g.value(a, *t.Key, depth+1)
			}
			dup := false
			for i := m; i < a.Mark() && t.Kind != reasonr.KindList; i += map[bool]int{true: 2, false: 1}[t.Kind == reasonr.KindMap] {
				dup = dup || a.Equal(a.Stack(i), k)
			}
			if dup {
				continue
			}
			a.Push(k)
			if t.Kind == reasonr.KindMap {
				a.Push(g.value(a, *t.Elem, depth+1))
			}
		}
		return a.End(t.Kind, 0, m)
	case reasonr.KindPageRequest:
		m := a.Mark()
		a.Push(reasonr.Int(int64(g.intn(101))))
		a.Push(reasonr.None)
		return a.End(reasonr.KindPageRequest, 0, m)
	}
	return reasonr.None
}

// caller is an actor from the set of identifiers, a role or an
// authenticated caller without a role.
func (g *gen) caller(a *reasonr.Arena) reasonr.Caller {
	c := reasonr.Caller{Kind: reasonr.CallerUser, Subject: reasonr.None}
	var actors []reasonr.TypeRef
	for _, ti := range g.s.Types {
		if ti.Kind == reasonr.TypeActor {
			actors = append(actors, ti.Ref)
		}
	}
	switch k := g.intn(3); {
	case k == 0 && len(actors) > 0:
		c.Actor = actors[g.intn(len(actors))]
		c.Subject = g.id(a, c.Actor, 0)
	case k == 1 && len(g.s.Roles) > 0:
		c.Roles = []string{g.s.Roles[g.intn(len(g.s.Roles))]}
	}
	return c
}

// callerFor is a caller by a random access condition of the method: a
// role, the actor itself (target), an actor of the type or of a field;
// sometimes a random one (forbidden).
func (g *gen) callerFor(a *reasonr.Arena, owner reasonr.TypeRef, mi *reasonr.MethodInfo, target reasonr.Value) reasonr.Caller {
	if len(mi.Access) == 0 || g.intn(8) == 0 {
		return g.caller(a)
	}
	acc := mi.Access[g.intn(len(mi.Access))]
	c := reasonr.Caller{Kind: reasonr.CallerUser, Subject: reasonr.None}
	switch acc.Kind {
	case reasonr.AccessRole:
		c.Roles = []string{acc.Role}
	case reasonr.AccessSelf:
		c.Actor, c.Subject = owner, target
	case reasonr.AccessActor:
		c.Actor = acc.Actor
		c.Subject = g.id(a, acc.Actor, 0)
	case reasonr.AccessField:
		t := reasonr.Type{Kind: reasonr.KindRecord, Ref: owner}
		for _, f := range acc.Field {
			if ti := g.s.Type(t.Ref); ti != nil && int(f) < len(ti.Fields) {
				t = ti.Fields[f].Type
			}
		}
		if t.Kind == reasonr.KindID {
			c.Actor = t.Ref
			c.Subject = g.id(a, t.Ref, 0)
		}
	}
	return c
}

// methods are the public methods of kind kind: the type and the number.
func methods(s *reasonr.Schema, kind reasonr.MethodKind) [][2]int {
	var out [][2]int
	for _, ti := range s.Types {
		for _, mi := range ti.Methods {
			if mi.Kind == kind && mi.Public {
				out = append(out, [2]int{int(ti.Ref), int(mi.Ref)})
			}
		}
	}
	return out
}

// expectedFault is a fault the model may cause on any data.
func expectedFault(err error) bool {
	var flt *reasonr.Fault
	if !errors.As(err, &flt) {
		return false
	}
	for _, e := range []error{reasonr.ErrOverflow, reasonr.ErrDivZero, reasonr.ErrCheck, reasonr.ErrLimit, reasonr.ErrConvert, reasonr.ErrInvalidRegex} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

func fuzzSeeds() [][]byte {
	return [][]byte{
		{0, 0, 0, 1, 2, 3, 4, 5, 6, 7},
		{1, 0, 1, 1, 1, 1, 0, 2, 2, 2, 2, 8, 9, 9},
		{2, 0, 0, 0, 0, 3, 1, 0, 1, 5, 0, 0, 2, 2, 1, 3},
		{3, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		{4, 0, 3, 0, 1, 2, 0, 0, 4, 1, 1, 7, 8, 200, 0, 1, 2},
		{2, 0, 1, 1, 0, 0, 0, 3, 1, 1, 0, 2, 5, 0, 2, 0, 0, 0, 0, 1},
	}
}

func FuzzEngine(f *testing.F) {
	for _, s := range fuzzSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 {
			return
		}
		m := fuzzMachine(t, int(data[0]))
		runFuzz(t, m, data[1:])
	})
}

func runFuzz(t *testing.T, m *reasonr.Model, data []byte) *Engine {
	ctx := context.Background()
	s := m.Schema()
	e := New(m, Options{CheckDeterminism: true})
	g := &gen{data: data, s: s}
	cmds, queries := methods(s, reasonr.MethodCommand), methods(s, reasonr.MethodQuery)
	a := reasonr.NewArena()
	check := func(what string, err error) {
		if err != nil && !expectedFault(err) {
			t.Fatalf("%s: an unexpected fault: %v", what, err)
		}
	}
	for step := 0; step < 40 && !g.done(); step++ {
		a.Reset()
		switch k := g.intn(10); {
		case k < 6 && len(cmds) > 0:
			tm := cmds[g.intn(len(cmds))]
			ti := s.Type(reasonr.TypeRef(tm[0]))
			mi := &ti.Methods[tm[1]]
			args := make([]reasonr.Value, len(mi.Params))
			for i := range mi.Params {
				args[i] = g.value(a, mi.Params[i].Type, 0)
			}
			id := g.id(a, ti.Ref, 0)
			out, err := e.Execute(ctx, reasonr.CommandCall{
				Entity: ti.Ref, ID: id, Command: mi.Ref, Args: a.Record(0, args...), Arena: a, Caller: g.callerFor(a, ti.Ref, mi, id),
			})
			check(ti.Name+"."+mi.Name, err)
			if err == nil {
				out.Release()
			}
		case k < 9 && len(queries) > 0:
			tm := queries[g.intn(len(queries))]
			ti := s.Type(reasonr.TypeRef(tm[0]))
			mi := &ti.Methods[tm[1]]
			args := make([]reasonr.Value, len(mi.Params))
			for i := range mi.Params {
				args[i] = g.value(a, mi.Params[i].Type, 0)
			}
			out, err := e.Query(ctx, reasonr.QueryCall{
				Type: ti.Ref, Query: mi.Ref, Args: a.Record(0, args...), Arena: a, Caller: g.callerFor(a, ti.Ref, mi, reasonr.None),
			})
			check(ti.Name+"."+mi.Name, err)
			if err == nil {
				out.Release()
			}
		default:
			check("time passes", e.Advance(ctx, time.Duration(g.byte())*10*time.Second, nil))
		}
	}
	checkFolds(t, e, m)
	return e
}

// checkFolds: the state of each entity equals the fold of its events.
func checkFolds(t *testing.T, e *Engine, m *reasonr.Model) {
	ctx := context.Background()
	for _, ti := range m.Schema().Types {
		if ti.Kind != reasonr.TypeEntity && ti.Kind != reasonr.TypeActor {
			continue
		}
		a := reasonr.NewArena()
		rows, err := e.Find(ctx, a, &reasonr.FindSpec{Type: ti.Ref, Index: reasonr.NoIndex, Cursor: reasonr.None, Limit: -1})
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range a.Elems(rows.Records) {
			vals := a.Elems(rec)
			id := vals[len(vals)-1]
			state := a.Record(ti.Ref, vals[:len(vals)-1]...)
			evs, err := e.Events(a, ti.Ref, id)
			if err != nil {
				t.Fatal(err)
			}
			fold, err := m.Fold(ctx, a, a, ti.Ref, id, evs)
			if err != nil {
				t.Fatalf("%s %s: event fold: %v", ti.Name, a.Format(id), err)
			}
			if !a.Equal(fold.Value(), state.Value()) {
				t.Fatalf("%s %s: state %s, event fold %s", ti.Name, a.Format(id), a.Format(state.Value()), a.Format(fold.Value()))
			}
		}
	}
}

// TestFuzzSeeds runs the seed corpus and long random sequences on each
// model (engine properties in CI).
func TestFuzzSeeds(t *testing.T) {
	for _, s := range fuzzSeeds() {
		runFuzz(t, fuzzMachine(t, int(s[0])), s[1:])
	}
	for i := range fuzzModels {
		data := make([]byte, 400)
		x := uint32(i*7919 + 1)
		for j := range data {
			x = x*1664525 + 1013904223
			data[j] = byte(x >> 24)
		}
		e := runFuzz(t, fuzzMachine(t, i), data)
		if e.store.(*Store).Position() == 0 {
			t.Errorf("%s: no unit of work was committed", fuzzModels[i].dir)
		}
		t.Logf("%s %s: %d units of work", fuzzModels[i].dir, fuzzModels[i].tenant, e.store.(*Store).Position())
	}
}
