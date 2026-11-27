package kernel

import (
	"fmt"

	reasonr "github.com/tycheg/reasonr-compiler"
)

// diffOutcomes compares the results of two executions of one command and
// returns the first difference or "".
func diffOutcomes(x, y *reasonr.Outcome) string {
	ax, ay := x.Arena, y.Arena
	eq := func(v, w reasonr.Value) bool { return reasonr.Equal(ax, v, ay, w) }
	rec := func(v, w reasonr.Record) bool { return eq(v.Value(), w.Value()) }
	subjects := func(sx, sy []reasonr.ArgSubject) bool {
		if len(sx) != len(sy) {
			return false
		}
		for i := range sx {
			if sx[i].Arg != sy[i].Arg || sx[i].Subject.Type != sy[i].Subject.Type || !eq(sx[i].Subject.ID, sy[i].Subject.ID) {
				return false
			}
		}
		return true
	}
	if len(x.Errors) != len(y.Errors) {
		return fmt.Sprintf("errors %d and %d", len(x.Errors), len(y.Errors))
	}
	for i := range x.Errors {
		ex, ey := &x.Errors[i], &y.Errors[i]
		if ex.Owner != ey.Owner || ex.Name != ey.Name || !rec(ex.Args, ey.Args) {
			return fmt.Sprintf("error %d: %s and %s", i, ex.Name, ey.Name)
		}
	}
	if (x.Work == nil) != (y.Work == nil) {
		return "only one execution has a unit of work"
	}
	if x.Work == nil {
		return ""
	}
	wx, wy := x.Work, y.Work
	if len(wx.Entities) != len(wy.Entities) || len(wx.Projections) != len(wy.Projections) ||
		len(wx.Timers) != len(wy.Timers) || len(wx.Notifications) != len(wy.Notifications) ||
		len(wx.Requests) != len(wy.Requests) || len(wx.Forget) != len(wy.Forget) || len(wx.Calls) != len(wy.Calls) {
		return "a different number of effects"
	}
	for i := range wx.Entities {
		cx, cy := &wx.Entities[i], &wy.Entities[i]
		if cx.Type != cy.Type || !eq(cx.ID, cy.ID) || cx.Existed != cy.Existed || cx.Version != cy.Version ||
			len(cx.Events) != len(cy.Events) || !rec(cx.State, cy.State) {
			return fmt.Sprintf("entity %d", i)
		}
		for j := range cx.Events {
			vx, vy := &cx.Events[j], &cy.Events[j]
			if vx.Name != vy.Name || vx.Seq != vy.Seq || !rec(vx.Args, vy.Args) {
				return fmt.Sprintf("entity %d, event %d", i, j)
			}
		}
	}
	for i := range wx.Projections {
		px, py := &wx.Projections[i], &wy.Projections[i]
		if px.Kind != py.Kind || px.Projection != py.Projection || !eq(px.ID, py.ID) || px.Seq != py.Seq ||
			len(px.Set) != len(py.Set) {
			return fmt.Sprintf("projection operation %d", i)
		}
		for j := range px.Set {
			if px.Set[j].Field != py.Set[j].Field || !eq(px.Set[j].Value, py.Set[j].Value) {
				return fmt.Sprintf("projection operation %d, field %d", i, j)
			}
		}
	}
	for i := range wx.Timers {
		tx, ty := &wx.Timers[i], &wy.Timers[i]
		if tx.Kind != ty.Kind || !eq(tx.ID, ty.ID) || !tx.At.Equal(ty.At) || tx.Entity != ty.Entity ||
			!eq(tx.EntityID, ty.EntityID) || tx.Command != ty.Command || !rec(tx.Args, ty.Args) ||
			!subjects(tx.Subjects, ty.Subjects) {
			return fmt.Sprintf("timer %d", i)
		}
	}
	for i := range wx.Notifications {
		nx, ny := &wx.Notifications[i], &wy.Notifications[i]
		if nx.Owner != ny.Owner || nx.Name != ny.Name || !rec(nx.Args, ny.Args) || !eq(nx.To, ny.To) ||
			!subjects(nx.Subjects, ny.Subjects) {
			return fmt.Sprintf("notification %d", i)
		}
	}
	for i := range wx.Requests {
		rx, ry := &wx.Requests[i], &wy.Requests[i]
		if rx.System != ry.System || rx.Request != ry.Request || !rec(rx.Args, ry.Args) ||
			!eq(rx.Key, ry.Key) || !subjects(rx.Subjects, ry.Subjects) {
			return fmt.Sprintf("external request %d", i)
		}
	}
	for i := range wx.Forget {
		if wx.Forget[i].Type != wy.Forget[i].Type || !eq(wx.Forget[i].ID, wy.Forget[i].ID) {
			return fmt.Sprintf("forget %d", i)
		}
	}
	for i := range wx.Calls {
		cx, cy := &wx.Calls[i], &wy.Calls[i]
		if cx.System != cy.System || cx.Entity != cy.Entity || !eq(cx.ID, cy.ID) || cx.Command != cy.Command || !rec(cx.Args, cy.Args) {
			return fmt.Sprintf("fake call %d", i)
		}
	}
	return ""
}

// idLog holds the identifiers issued to the first execution of a command:
// the second execution gets the same ones in order.
type idLog struct {
	types  []reasonr.TypeRef
	vals   []reasonr.Value
	replay bool
	pos    int
	extra  bool // the second execution asked for other identifiers
}

func (l *idLog) add(t reasonr.TypeRef, v reasonr.Value) {
	l.types, l.vals = append(l.types, t), append(l.vals, v)
}

func (l *idLog) next(t reasonr.TypeRef) (reasonr.Value, error) {
	if l.pos >= len(l.vals) || l.types[l.pos] != t {
		l.extra = true
		return reasonr.None, fmt.Errorf("%w: an identifier the first execution did not have was requested", ErrNondetermined)
	}
	l.pos++
	return l.vals[l.pos-1], nil
}

// done reports whether the second execution got exactly the same identifiers.
func (l *idLog) done() bool { return !l.extra && l.pos == len(l.vals) }
