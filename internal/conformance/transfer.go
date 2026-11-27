package conformance

import (
	"context"
	"testing"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/transfer"
	"github.com/tycheg/reasonr-engine/kind"
)

// RunStateTransfer checks port.StateTransfer (moving state between
// engines, RFC §32): live timers with personal arguments and sequence
// counters are exported from the store src and imported into dst; the
// timer in dst fires at the same moment with the same arguments, the
// counter continues; src has no more timers.
func RunStateTransfer(t *testing.T, src, dst Factory) {
	f := newPersonal(t, src)
	if _, ok := f.st.Unit.(kind.StateTransfer); !ok {
		t.Skip("the store does not transfer state (port.StateTransfer)")
	}
	ctx := context.Background()
	p := f.id("Person")
	if errs := f.register(p, phoneA, nameA); len(errs) > 0 {
		t.Fatal(errs)
	}
	if errs := f.exec(actor(p), p, "Remind", f.a.String("note-marker")); len(errs) > 0 {
		t.Fatal(errs)
	}
	person := f.typ("Person")
	if _, err := f.st.Unit.NextSequence(ctx, T1, person, 5); err != nil {
		t.Fatal(err)
	}
	a := reasonr.NewArena()
	st, err := f.st.Unit.(kind.StateTransfer).ExportState(ctx, T1, a)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Timers) != 2 || st.Sequences[person] != 5 {
		t.Fatalf("%d timers exported, counter %d", len(st.Timers), st.Sequences[person])
	}
	for _, tm := range st.Timers {
		if got := a.Text(a.Elems(tm.Args.Value())[0]); got != "note-marker" || len(tm.Subjects) == 0 {
			t.Fatalf("timer argument %q, %d subjects", got, len(tm.Subjects))
		}
		if !tm.At.Equal(Start.Add(time.Hour)) {
			t.Fatalf("timer moment %v", tm.At)
		}
	}
	if err := f.st.Unit.(kind.StateTransfer).ClearTimers(ctx, T1); err != nil {
		t.Fatal(err)
	}
	if left := f.lease(T1, Start.Add(2*time.Hour), time.Minute); len(left) != 0 {
		t.Fatalf("%d timers left in the source after the transfer", len(left))
	}
	g := newFixture(t, dst, options{machine: f.m})
	if err := transfer.ImportState(ctx, g.st.Unit, T1, a, st, Start); err != nil {
		t.Fatal(err)
	}
	if first, err := g.st.Unit.NextSequence(ctx, T1, person, 1); err != nil || first != 6 {
		t.Fatalf("the counter continued from %d: %v", first, err)
	}
	if early := g.lease(T1, Start.Add(59*time.Minute), time.Minute); len(early) != 0 {
		t.Fatalf("timers fired before their time: %d", len(early))
	}
	ts := g.lease(T1, Start.Add(time.Hour), time.Minute)
	if len(ts) != 2 {
		t.Fatalf("%d timers fired, want 2", len(ts))
	}
	for _, lt := range ts {
		if got := lt.Arena.Text(lt.Arena.Elems(lt.Args.Value())[0]); got != "note-marker" || lt.EntityID.UUID() != p.UUID() {
			t.Fatalf("the timer in the receiver: argument %q, entity %v", got, lt.EntityID)
		}
	}
}
