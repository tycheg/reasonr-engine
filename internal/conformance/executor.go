package conformance

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// ExecutorHistory runs one history of commands and queries of the model
// testdata/store on ex and returns its transcript: errors, notifications to
// the initiator and to another actor, issued identifiers, a repeat by key,
// query results, a timer firing. The transcripts of executors are compared
// with each other: one history gives the same answers on every engine
// kind. Positions are not in the transcript — they differ between engines;
// only "has a position". The executor must be empty, the tenant
// port.DefaultTenant with the model Machine. Several executors are entry
// points of one engine: the steps go through them in turn, the
// subscription is at the last one.
func ExecutorHistory(t *testing.T, exs ...kind.Executor) []string {
	t.Helper()
	h := &history{t: t, exs: exs, s: Machine(t).Schema()}
	return h.run()
}

// CompareHistories compares the transcripts of two executors line by line.
func CompareHistories(t *testing.T, want, got []string) {
	t.Helper()
	if slices.Equal(want, got) {
		return
	}
	for i := range max(len(want), len(got)) {
		var w, g string
		if i < len(want) {
			w = want[i]
		}
		if i < len(got) {
			g = got[i]
		}
		if w != g {
			t.Errorf("step %d:\n  want: %s\n  got:  %s", i+1, w, g)
		}
	}
}

type history struct {
	t    *testing.T
	exs  []kind.Executor
	step int
	s    *reasonr.Schema
	a    *reasonr.Arena
	out  []string
	pos  port.Position
}

// ex is the executor of the next step.
func (h *history) ex() kind.Executor {
	h.step++
	return h.exs[h.step%len(h.exs)]
}

func (h *history) logf(format string, args ...any) {
	h.out = append(h.out, fmt.Sprintf(format, args...))
}

func (h *history) run() []string {
	ctx := context.Background()
	s := h.s
	keeper, box, ticket, view := s.Lookup("Keeper"), s.Lookup("Box"), s.Lookup("Ticket"), s.Lookup("BoxView")
	a := reasonr.NewArena()
	h.a = a
	admin := reasonr.Caller{Kind: reasonr.CallerUser, Roles: []string{"admin"}}
	k1 := reasonr.ID(keeper.Ref, reasonr.UUID([16]byte{0xe1, 1}))
	k2 := reasonr.ID(keeper.Ref, reasonr.UUID([16]byte{0xe1, 2}))
	as := func(k reasonr.Value) reasonr.Caller {
		return reasonr.Caller{Kind: reasonr.CallerUser, Subject: k, Actor: keeper.Ref}
	}
	small, big := reasonr.Enum(s.Lookup("Kind").Ref, 0), reasonr.Enum(s.Lookup("Kind").Ref, 1)
	b1 := reasonr.ID(box.Ref, reasonr.UUID([16]byte{0xb1, 1}))
	b2 := reasonr.ID(box.Ref, reasonr.UUID([16]byte{0xb1, 2}))

	sub, err := h.exs[len(h.exs)-1].Subscribe(ctx, port.DefaultTenant, a, k2)
	if err != nil {
		h.t.Fatal(err)
	}
	defer sub.Close()

	join := keeper.Method("Join").Ref
	h.command(ctx, "k1.Join(A)", "", keeper.Ref, k1, join, a.Record(0, a.String("Anna"), a.String("A")), as(k1))
	h.command(ctx, "k2.Join(A)", "", keeper.Ref, k2, join, a.Record(0, a.String("Boris"), a.String("A")), as(k2))
	h.command(ctx, "k2.Join(B)", "", keeper.Ref, k2, join, a.Record(0, a.String("Boris"), a.String("B")), as(k2))
	h.command(ctx, "k1.Poke(k2)", "", keeper.Ref, k1, keeper.Method("Poke").Ref, a.Record(0, k2), as(k1))
	select {
	case d := <-sub.C():
		h.logf("k2 got %s", h.notification(d.Arena, d.Notification))
	case <-time.After(10 * time.Second):
		h.logf("k2 got no notification")
	}
	h.command(ctx, "k1.Poke(k2) without rights", "", keeper.Ref, k1, keeper.Method("Poke").Ref, a.Record(0, k2), as(k2))

	put := box.Method("Put").Ref
	h.command(ctx, "b1.Put(small, 5, k1)", "put-1", box.Ref, b1, put, a.Record(0, small, reasonr.Int(5), k1), admin)
	h.command(ctx, "b1.Put(small, 5, k1) repeat by key", "put-1", box.Ref, b1, put, a.Record(0, small, reasonr.Int(5), k1), admin)
	h.command(ctx, "b1.Put without a role", "", box.Ref, b1, put, a.Record(0, small, reasonr.Int(6), reasonr.None), as(k1))
	size := view.Method("Size").Ref
	current := box.Method("Current").Ref
	for n := int64(1); n <= 4; n++ {
		h.command(ctx, fmt.Sprintf("b1.Put(small, %d, k1)", 5+n), "", box.Ref, b1, put, a.Record(0, small, reasonr.Int(5+n), k1), admin)
		h.query(ctx, "Box.Current(b1)", box.Ref, current, a.Record(0, b1), admin)
		h.query(ctx, "Box.Current(b1) once more", box.Ref, current, a.Record(0, b1), admin)
	}
	h.query(ctx, "Box.Current(b2)", box.Ref, current, a.Record(0, b2), admin)
	h.query(ctx, "BoxView.Size(b1)", view.Ref, size, a.Record(0, b1), admin)
	h.query(ctx, "BoxView.Size(b2)", view.Ref, size, a.Record(0, b2), admin)
	h.command(ctx, "b1.Relabel(big)", "", box.Ref, b1, box.Method("Relabel").Ref, a.Record(0, big), admin)
	h.query(ctx, "BoxView.Size(b1) after Relabel", view.Ref, size, a.Record(0, b1), admin)
	h.command(ctx, "b2.Relabel(big) — no box", "", box.Ref, b2, box.Method("Relabel").Ref, a.Record(0, big), admin)
	for range 3 {
		h.command(ctx, "Ticket.Open()", "", ticket.Ref, reasonr.None, ticket.Method("Open").Ref, a.Record(0), admin)
	}

	// A timer: Arm sets a deadline, expire deletes the projection row and
	// writes an external request.
	h.command(ctx, "b2.Put(big, 7)", "", box.Ref, b2, put, a.Record(0, big, reasonr.Int(7), reasonr.None), admin)
	h.command(ctx, "b2.Arm(50ms)", "", box.Ref, b2, box.Method("Arm").Ref, a.Record(0, reasonr.Duration(50*time.Millisecond)), admin)
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		out, err := h.ex().Query(ctx, &kind.QueryRequest{Tenant: port.DefaultTenant, After: h.pos, Call: reasonr.QueryCall{
			Type: view.Ref, Query: size, Args: a.Record(0, b2), Arena: a, Caller: admin}})
		if err != nil {
			h.t.Fatal(err)
		}
		gone := len(out.Errors) > 0
		out.Release()
		if gone {
			h.logf("timer b2 fired")
			break
		}
		if time.Now().After(deadline) {
			h.logf("timer b2 did not fire")
			break
		}
	}
	h.query(ctx, "BoxView.Size(b2) after the timer", view.Ref, size, a.Record(0, b2), admin)
	h.command(ctx, "b2.Disarm()", "", box.Ref, b2, box.Method("Disarm").Ref, a.Record(0), admin)

	h.command(ctx, "k1.Leave()", "", keeper.Ref, k1, keeper.Method("Leave").Ref, a.Record(0), as(k1))
	h.command(ctx, "k1.Join(A) after forget", "", keeper.Ref, k1, join, a.Record(0, a.String("Anna"), a.String("C")), as(k1))
	return h.out
}

func (h *history) command(ctx context.Context, step, key string, t reasonr.TypeRef, id reasonr.Value, cmd reasonr.MethodRef, args reasonr.Record, caller reasonr.Caller) {
	h.t.Helper()
	res, err := h.ex().Command(ctx, &kind.CommandRequest{Tenant: port.DefaultTenant, Idempotency: key, Call: reasonr.CommandCall{
		Entity: t, ID: id, Command: cmd, Args: args, Arena: h.a, Caller: caller}})
	if err != nil {
		h.t.Fatalf("%s: %v", step, err)
	}
	defer res.Release()
	var parts []string
	if res.Position > 0 {
		h.pos = max(h.pos, res.Position)
		parts = append(parts, "accepted")
	}
	if res.Replayed {
		parts = append(parts, "replayed")
	}
	if !res.ID.IsNone() {
		parts = append(parts, "id "+h.value(res.Arena, res.ID))
	}
	for _, e := range res.Errors {
		parts = append(parts, "error "+e.Name+h.record(res.Arena, e.Args))
	}
	for _, n := range res.Notifications {
		parts = append(parts, "notification "+h.notification(res.Arena, n))
	}
	h.out = append(h.out, step+": "+strings.Join(parts, ", "))
}

func (h *history) query(ctx context.Context, step string, t reasonr.TypeRef, q reasonr.MethodRef, args reasonr.Record, caller reasonr.Caller) {
	h.t.Helper()
	out, err := h.ex().Query(ctx, &kind.QueryRequest{Tenant: port.DefaultTenant, After: h.pos, Call: reasonr.QueryCall{
		Type: t, Query: q, Args: args, Arena: h.a, Caller: caller}})
	if err != nil {
		h.t.Fatalf("%s: %v", step, err)
	}
	defer out.Release()
	var b strings.Builder
	b.WriteString(step + ":")
	for _, e := range out.Errors {
		fmt.Fprintf(&b, " error %s%s", e.Name, h.record(out.Arena, e.Args))
	}
	if !out.Result.IsNone() {
		fmt.Fprintf(&b, " %s", h.record(out.Arena, out.Result.Record()))
	}
	h.out = append(h.out, b.String())
}

func (h *history) notification(a *reasonr.Arena, n reasonr.Notification) string {
	name := fmt.Sprint(n.Name)
	if ti := h.s.Type(n.Owner); ti != nil {
		for i := range ti.Methods {
			if ti.Methods[i].Ref == n.Name {
				name = ti.Methods[i].Name
			}
		}
	}
	to := ""
	if !n.To.IsNone() {
		to = " to " + h.value(a, n.To)
	}
	return name + h.record(a, n.Args) + to
}

func (h *history) record(a *reasonr.Arena, r reasonr.Record) string {
	return h.value(a, r.Value())
}

// value is a value without a schema: time and timers differ between
// engines, so only their kind is in the transcript.
func (h *history) value(a *reasonr.Arena, v reasonr.Value) string {
	switch v.Kind {
	case reasonr.KindNone:
		return "none"
	case reasonr.KindString:
		return strconv.Quote(a.Text(v))
	case reasonr.KindTimestamp, reasonr.KindTimerID:
		return v.Kind.String()
	case reasonr.KindID:
		switch {
		case v.Boxed():
			return h.value(a, a.Elems(v)[0])
		case v.Hi == 0: // sequence
			return fmt.Sprint(v.Int())
		}
		return fmt.Sprintf("%x", v.UUID())
	case reasonr.KindRecord, reasonr.KindList, reasonr.KindSet, reasonr.KindMap, reasonr.KindPage:
		var parts []string
		for _, e := range a.Elems(v) {
			parts = append(parts, h.value(a, e))
		}
		return "(" + strings.Join(parts, ", ") + ")"
	}
	return fmt.Sprintf("%s:%d:%d", v.Kind, v.Lo, v.Hi)
}
