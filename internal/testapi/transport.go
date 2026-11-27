package testapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/http/integration"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// FakesConsumer is the name of the outbox consumer that runs fake calls.
const FakesConsumer = "fakes"

// Fakes is the fake integration transport (RFC §18.1): in reasonrd --test
// the requests of ordinary tenants are run by fakes through the delivery
// queue rather than by the cascade, as for ephemeral ones. It is both a
// transport (Send) and an outbox consumer (Deliver):
//
//   - Send runs the request implementation with a command of the fake
//     instance: a request to a system without a fake or to a fake not
//     created is only logged, as in a test; the command's idempotency key
//     is the request key, so a repeated delivery does not run the fake twice;
//   - Deliver runs the calls of model commands the fakes made — from a
//     request implementation, a fake command through /_test/{tenant}/fakes
//     or a fake timer — with the system role; the idempotency key is
//     fake/{position}/{number}.
//
// The test API cascade runs the requests and calls of ephemeral tenants:
// the transport and the consumer skip them.
type Fakes struct {
	api *API
	log *slog.Logger
}

var _ ports.Transport = (*Fakes)(nil)

// Fakes returns the fake transport of the test API; log is the journal of
// requests the fakes did not run and of rejected commands.
func (api *API) Fakes(log *slog.Logger) *Fakes {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Fakes{api: api, log: log}
}

// Send runs a request with the system's fake.
func (f *Fakes) Send(ctx context.Context, lr *kind.LeasedRequest) error {
	if f.api.Ephemeral(lr.Tenant) {
		return nil
	}
	s, err := f.api.Programs.Schema(lr.Tenant)
	if err != nil {
		return err
	}
	req := &lr.Request
	ti := s.Type(req.System)
	if ti == nil || int(req.Request) >= len(ti.Methods) {
		return fmt.Errorf("%w: unknown request %d.%d", port.ErrPermanent, req.System, req.Request)
	}
	mi := &ti.Methods[req.Request]
	name := ti.Name + "." + mi.Name
	if !mi.Fake {
		f.log.Info("the fake has no implementation of the request: the request is not run", "tenant", string(lr.Tenant), "request", name)
		return nil
	}
	id := reasonr.FakeID(req.System)
	_, _, exists, err := f.api.engine().Load(ctx, lr.Tenant, lr.Arena, req.System, id)
	if err != nil {
		return err
	}
	if !exists {
		f.log.Info("the fake is not created (POST /_test/{tenant}/fakes/…): the request is not run", "tenant", string(lr.Tenant), "request", name)
		return nil
	}
	res, err := f.api.engine().Command(ctx, &kind.CommandRequest{Tenant: lr.Tenant, Idempotency: integration.Key(lr.Arena, req.Key),
		Call: reasonr.CommandCall{Entity: req.System, ID: id, Command: req.Request, Args: req.Args, Arena: lr.Arena,
			Caller: reasonr.Caller{Kind: reasonr.CallerFake, Subject: reasonr.None}}})
	if err != nil {
		return err
	}
	defer res.Release()
	if len(res.Errors) > 0 {
		f.log.Warn("the fake rejected the request", "tenant", string(lr.Tenant), "request", name, "errors", errorNames(res.Errors))
	}
	return nil
}

// Name is the outbox consumer name.
func (f *Fakes) Name() string { return FakesConsumer }

// Deliver runs the fake calls of a unit of work.
func (f *Fakes) Deliver(ctx context.Context, u *kind.CommittedUnit, _ int, _ func(int, string) error) error {
	if len(u.Calls) == 0 || f.api.Ephemeral(u.Tenant) {
		return nil
	}
	s, err := f.api.Programs.Schema(u.Tenant)
	if err != nil {
		return err
	}
	// Its own arena: the arena of the outbox unit does not change, while
	// the kernel writes into the call arena.
	a := reasonr.NewArena()
	for i := range u.Calls { // repeating the whole unit is safe: the calls have idempotency keys
		c := &u.Calls[i]
		res, err := f.api.engine().Command(ctx, &kind.CommandRequest{Tenant: u.Tenant, Idempotency: fmt.Sprintf("fake/%d/%d", u.Position, i),
			Call: reasonr.CommandCall{Entity: c.Entity, ID: a.Copy(u.Arena, c.ID), Command: c.Command, Args: a.Copy(u.Arena, c.Args.Value()).Record(), Arena: a,
				Caller: reasonr.Caller{Kind: reasonr.CallerUser, Subject: reasonr.None, Roles: []string{s.Type(c.System).Role}}}})
		if err != nil {
			return err
		}
		if len(res.Errors) > 0 {
			ti := s.Type(c.Entity)
			f.log.Warn("the command a fake called was rejected", "tenant", string(u.Tenant), "position", uint64(u.Position),
				"system", s.Type(c.System).Name, "command", ti.Name+"."+ti.Methods[c.Command].Name, "errors", errorNames(res.Errors))
		}
		res.Release()
	}
	return nil
}

func errorNames(errs []reasonr.BusinessError) []string {
	out := make([]string, len(errs))
	for i, e := range errs {
		out[i] = e.Name
	}
	return out
}

// Ephemeral reports that t is an ephemeral test tenant: its timers fire
// through /_test/{tenant}/clock by the virtual clock, and its fakes run by
// the cascade.
func (api *API) Ephemeral(t port.Tenant) bool {
	api.mu.Lock()
	defer api.mu.Unlock()
	return api.tenants[t] != nil
}
