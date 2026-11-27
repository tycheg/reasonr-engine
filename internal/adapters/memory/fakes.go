package memory

import (
	"context"
	"fmt"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/kind"
)

// MaxCascade is how many units of work the fake cascade of one step may run
// (RFC §27.1): more means an endless exchange of the fake and the model.
const MaxCascade = 1000

// FakeError holds the errors of a model command a fake called. The values
// lie in the delivery journal arena.
type FakeError struct {
	Call   reasonr.FakeCall
	Errors []reasonr.BusinessError
}

// cascade runs the fakes after commit (RFC §18.1): external requests to
// created fakes run their implementations (CallerFake), the fakes' calls of
// model commands run with the role of the fake's system, like the answers
// of a real system through the API. New units of work are processed the
// same way while they appear.
func (e *Engine) cascade(ctx context.Context) error {
	for n := 0; ; {
		units, err := e.store.Read(ctx, e.cascaded, 0)
		if err != nil {
			return err
		}
		if len(units) == 0 {
			return nil
		}
		for _, u := range units {
			e.cascaded = u.Position
			// Its own arena: the arena of the outbox unit does not change —
			// the request sender reads it (port.LeasedRequest.Arena), while
			// Load and the kernel write into the call arena.
			a := reasonr.NewArena()
			for _, rq := range u.Requests {
				ti := e.schema.Type(rq.System)
				if ti == nil || !ti.Methods[rq.Request].Fake {
					continue
				}
				id := reasonr.FakeID(rq.System)
				_, _, exists, err := e.store.Load(ctx, Tenant, a, rq.System, id)
				if err != nil {
					return err
				}
				if !exists {
					continue // the test did not set up the fake: the request is only recorded
				}
				if n++; n > MaxCascade {
					return fmt.Errorf("the fake cascade is longer than %d units of work: the fake and the model call each other endlessly", MaxCascade)
				}
				res, err := e.k.Command(ctx, &kind.CommandRequest{Tenant: Tenant, Call: reasonr.CommandCall{
					Entity: rq.System, ID: id, Command: rq.Request, Args: a.Copy(u.Arena, rq.Args.Value()).Record(), Arena: a,
					Caller: reasonr.Caller{Kind: reasonr.CallerFake, Subject: reasonr.None},
				}})
				if err != nil {
					return err
				}
				if err := e.settle(res, nil, nil); err != nil {
					return err
				}
			}
			for _, c := range u.Calls {
				if n++; n > MaxCascade {
					return fmt.Errorf("the fake cascade is longer than %d units of work: the fake and the model call each other endlessly", MaxCascade)
				}
				role := e.schema.Type(c.System).Role
				c.ID, c.Args = a.Copy(u.Arena, c.ID), a.Copy(u.Arena, c.Args.Value()).Record()
				res, err := e.k.Command(ctx, &kind.CommandRequest{Tenant: Tenant, Call: reasonr.CommandCall{
					Entity: c.Entity, ID: c.ID, Command: c.Command, Args: c.Args, Arena: a,
					Caller: reasonr.Caller{Kind: reasonr.CallerUser, Subject: reasonr.None, Roles: []string{role}},
				}})
				if err != nil {
					return err
				}
				if err := e.settle(res, a, &c); err != nil {
					return err
				}
			}
		}
	}
}

// settle records what a unit of work of the cascade delivered and releases
// the response; the errors of the fake call call (values in the arena src)
// accumulate for the test runner.
func (e *Engine) settle(res *kind.CommandResult, src *reasonr.Arena, call *reasonr.FakeCall) error {
	defer res.Release()
	if err := e.collect(res); err != nil {
		return err
	}
	if call == nil || len(res.Errors) == 0 {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	fe := FakeError{Call: *call}
	fe.Call.ID = e.log.Copy(src, call.ID)
	fe.Call.Args = e.log.Copy(src, call.Args.Value()).Record()
	for _, be := range res.Errors {
		be.Args = e.log.Copy(res.Arena, be.Args.Value()).Record()
		fe.Errors = append(fe.Errors, be)
	}
	e.fakeErrs = append(e.fakeErrs, fe)
	return nil
}

// FakeErrors returns the journal arena and the errors of fake calls from
// the number from.
func (e *Engine) FakeErrors(from int) (*reasonr.Arena, []FakeError) {
	e.mu.Lock()
	defer e.mu.Unlock()
	from = min(from, len(e.fakeErrs))
	return e.log, append([]FakeError(nil), e.fakeErrs[from:]...)
}
