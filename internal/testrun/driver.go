package testrun

import (
	"context"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/memory"
	"github.com/tycheg/reasonr-engine/kind"
)

// memoryDriver is a test driver on the in-memory engine (reasonrd test
// --driver memory): steps are direct engine calls, the step's
// notifications and external requests come from its delivery journal.
type memoryDriver struct {
	e      *memory.Engine
	a      *reasonr.Arena // the step arena: Step values until the next call
	sent   int            // delivery journal entries read
	failed int            // errors of fake calls read
}

func newMemoryDriver(e *memory.Engine) *memoryDriver {
	return &memoryDriver{e: e, a: reasonr.NewArena()}
}

func (d *memoryDriver) step() *reasonr.Step {
	d.a.Reset()
	return &reasonr.Step{Arena: d.a, Result: reasonr.None}
}

// addErrors copies errors from the arena src into the step.
func (d *memoryDriver) addErrors(st *reasonr.Step, src *reasonr.Arena, errs []reasonr.BusinessError) {
	for _, be := range errs {
		be.Args = d.a.Copy(src, be.Args.Value()).Record()
		st.Errors = append(st.Errors, be)
	}
}

// deliveries adds to the step what was delivered since the last step and
// the errors of commands the fakes called.
func (d *memoryDriver) deliveries(st *reasonr.Step) {
	flog, errs := d.e.FakeErrors(d.failed)
	d.failed += len(errs)
	for _, fe := range errs {
		out := reasonr.FakeError{Call: fe.Call}
		out.Call.ID = d.a.Copy(flog, fe.Call.ID)
		out.Call.Args = d.a.Copy(flog, fe.Call.Args.Value()).Record()
		for _, be := range fe.Errors {
			be.Args = d.a.Copy(flog, be.Args.Value()).Record()
			out.Errors = append(out.Errors, be)
		}
		st.FakeErrors = append(st.FakeErrors, out)
	}
	log, sent := d.e.Deliveries(d.sent)
	d.sent += len(sent)
	for _, dv := range sent {
		if n := dv.Notification; n != nil {
			st.Notifications = append(st.Notifications, reasonr.Notification{
				Owner: n.Owner, Name: n.Name, Args: d.a.Copy(log, n.Args.Value()).Record(), To: d.a.Copy(log, n.To),
			})
			continue
		}
		rq := dv.Request
		st.Requests = append(st.Requests, reasonr.ExternalRequest{
			System: rq.System, Request: rq.Request, Args: d.a.Copy(log, rq.Args.Value()).Record(),
			Key: d.a.Copy(log, rq.Key),
		})
	}
}

func (d *memoryDriver) Command(ctx context.Context, call reasonr.CommandCall) (*reasonr.Step, error) {
	out, err := d.e.Execute(ctx, call)
	if err != nil {
		return nil, err
	}
	defer out.Release()
	st := d.step()
	d.addErrors(st, out.Arena, out.Errors)
	d.deliveries(st)
	return st, nil
}

func (d *memoryDriver) Query(ctx context.Context, call reasonr.QueryCall) (*reasonr.Step, error) {
	out, err := d.e.Query(ctx, call)
	if err != nil {
		return nil, err
	}
	defer out.Release()
	st := d.step()
	d.addErrors(st, out.Arena, out.Errors)
	st.Result = d.a.Copy(out.Arena, out.Result)
	return st, nil
}

func (d *memoryDriver) Advance(ctx context.Context, dur time.Duration) (*reasonr.Step, error) {
	st := d.step()
	err := d.e.Advance(ctx, dur, func(res *kind.CommandResult) { d.addErrors(st, res.Arena, res.Errors) })
	if err != nil {
		return nil, err
	}
	d.deliveries(st)
	return st, nil
}

// EnableFeature switches on a test feature flag (reasonr.FeatureDriver).
func (d *memoryDriver) EnableFeature(ctx context.Context, name string) error {
	return d.e.EnableFeature(ctx, name)
}

func (d *memoryDriver) NewID(ctx context.Context, a *reasonr.Arena, t reasonr.TypeRef) (reasonr.Value, error) {
	return d.e.NewID(ctx, a, t)
}

// MemoryDriver returns a test driver on a new in-memory engine of the model
// m with its clock at the current time: drivers that wrap the in-memory
// one (a codec round trip) start from it.
func MemoryDriver(m *reasonr.Model) reasonr.Driver {
	return newMemoryDriver(memory.New(m, memory.Options{Now: time.Now().UTC()}))
}
