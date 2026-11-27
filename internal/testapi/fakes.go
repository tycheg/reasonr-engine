package testapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/codecs"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// MaxCascade is how many units of work the fake cascade of one test API
// call may run (RFC §27.1), as in the in-memory engine.
const MaxCascade = 1000

// fake is a command of a fake of an external system from a test (RFC
// §18.1): POST /_test/{tenant}/fakes/{system}/{command}, the body is the
// arguments. The response is the command's business errors; for an
// ephemeral tenant the fake cascade runs next. An ordinary tenant sets up
// the fakes of the fake transport this way: Fakes runs its requests and
// fake calls.
func (api *API) fake(w http.ResponseWriter, r *http.Request) {
	t := port.Tenant(r.PathValue("tenant"))
	api.mu.Lock()
	e := api.tenants[t]
	api.mu.Unlock()
	s, err := api.Programs.Schema(t)
	if err != nil {
		fail(w, http.StatusNotFound, "no tenant %q", t)
		return
	}
	ti := s.Lookup(r.PathValue("system"))
	if ti == nil || ti.Kind != reasonr.TypeExternal || ti.ID == reasonr.IDNone {
		fail(w, http.StatusNotFound, "no fake of the external system %q", r.PathValue("system"))
		return
	}
	mi := ti.Method(r.PathValue("command"))
	if mi == nil || mi.Kind != reasonr.MethodCommand || !mi.Public {
		fail(w, http.StatusNotFound, "fake %s has no public command %q", ti.Name, r.PathValue("command"))
		return
	}
	codec := codecs.JSONOf(s, codecs.Options{})
	a := reasonr.NewArena()
	var body json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "body: %v", err)
		return
	}
	args, err := codec.DecodeFields(a, 0, mi.Params, body)
	if err != nil {
		fail(w, http.StatusBadRequest, "arguments: %v", err)
		return
	}
	res, err := api.engine().Command(r.Context(), &kind.CommandRequest{Tenant: t, Call: reasonr.CommandCall{
		Entity: ti.Ref, ID: reasonr.FakeID(ti.Ref), Command: mi.Ref, Args: args, Arena: a,
		Caller: reasonr.Caller{Kind: reasonr.CallerTest, Subject: reasonr.None},
	}})
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	errs := []json.RawMessage{}
	for _, be := range res.Errors {
		b, err := EncodeError(codec, res.Arena, be)
		if err != nil {
			res.Release()
			fail(w, http.StatusInternalServerError, "%v", err)
			return
		}
		errs = append(errs, b)
	}
	pos := res.Position
	res.Release()
	if e != nil {
		e.mu.Lock()
		e.need = max(e.need, pos)
		err = api.cascade(r.Context(), t, e)
		e.mu.Unlock()
		if err != nil {
			fail(w, http.StatusInternalServerError, "%v", err)
			return
		}
	}
	reply(w, map[string]any{"errors": errs})
}

// cascade runs the fakes of the tenant t after commits (as memory.Engine
// does): external requests to created fakes run their implementations,
// the fakes' calls of model commands run with the system role. The
// notifications to the initiator of these commands go to the hub journal,
// the errors to the tenant's fake error journal. Called under e.mu.
func (api *API) cascade(ctx context.Context, t port.Tenant, e *ephemeral) error {
	s, _ := api.Programs.Schema(t)
	for n := 0; ; {
		units, err := api.pending(ctx, e.cascaded, e.need)
		if err != nil {
			return err
		}
		if len(units) == 0 {
			return nil
		}
		for _, u := range units {
			e.cascaded = u.Position
			if u.Tenant != t {
				continue
			}
			// Its own arena: the arena of the outbox unit does not change —
			// the request sender reads it (port.LeasedRequest.Arena), while
			// Load and the kernel write into the call arena.
			a := reasonr.NewArena()
			for _, rq := range u.Requests {
				ti := s.Type(rq.System)
				if ti == nil || !ti.Methods[rq.Request].Fake {
					continue
				}
				id := reasonr.FakeID(rq.System)
				_, _, exists, err := api.engine().Load(ctx, t, a, rq.System, id)
				if err != nil {
					return err
				}
				if !exists {
					continue
				}
				if n++; n > MaxCascade {
					return errCascade
				}
				if err := api.run(ctx, t, s, e, nil, reasonr.CommandCall{
					Entity: rq.System, ID: id, Command: rq.Request, Args: a.Copy(u.Arena, rq.Args.Value()).Record(), Arena: a,
					Caller: reasonr.Caller{Kind: reasonr.CallerFake, Subject: reasonr.None},
				}); err != nil {
					return err
				}
			}
			for _, c := range u.Calls {
				if n++; n > MaxCascade {
					return errCascade
				}
				c.ID, c.Args = a.Copy(u.Arena, c.ID), a.Copy(u.Arena, c.Args.Value()).Record()
				if err := api.run(ctx, t, s, e, &c, reasonr.CommandCall{
					Entity: c.Entity, ID: c.ID, Command: c.Command, Args: c.Args, Arena: a,
					Caller: reasonr.Caller{Kind: reasonr.CallerUser, Subject: reasonr.None, Roles: []string{s.Type(c.System).Role}},
				}); err != nil {
					return err
				}
			}
		}
	}
}

var errCascade = fmt.Errorf("the fake cascade is longer than %d units of work: the fake and the model call each other endlessly", MaxCascade)

// run runs a cascade command; call is the fake call whose errors are
// recorded.
func (api *API) run(ctx context.Context, t port.Tenant, s *reasonr.Schema, e *ephemeral, call *reasonr.FakeCall, cc reasonr.CommandCall) error {
	res, err := api.engine().Command(ctx, &kind.CommandRequest{Tenant: t, Call: cc})
	if err != nil {
		return err
	}
	defer res.Release()
	e.need = max(e.need, res.Position)
	api.Hub.mu.Lock()
	if j := api.Hub.logs[t]; j != nil {
		for _, n := range res.Notifications {
			j.notes = append(j.notes, reasonr.Notification{Owner: n.Owner, Name: n.Name,
				Args: j.a.Copy(res.Arena, n.Args.Value()).Record(), To: reasonr.None})
		}
	}
	api.Hub.mu.Unlock()
	if call == nil || len(res.Errors) == 0 {
		return nil
	}
	codec := codecs.JSONOf(s, codecs.Options{})
	ti := s.Type(call.Entity)
	mi := &ti.Methods[call.Command]
	id, err := codec.Append(nil, cc.Arena, reasonr.Type{Kind: reasonr.KindID, Ref: call.Entity}, call.ID)
	if err != nil {
		return err
	}
	args, err := codec.AppendFields(nil, cc.Arena, mi.Params, call.Args)
	if err != nil {
		return err
	}
	var errs []json.RawMessage
	for _, be := range res.Errors {
		b, err := EncodeError(codec, res.Arena, be)
		if err != nil {
			return err
		}
		errs = append(errs, b)
	}
	b, err := json.Marshal(map[string]any{"system": s.Type(call.System).Name, "entity": ti.Name, "id": json.RawMessage(id),
		"command": mi.Name, "args": json.RawMessage(args), "errors": errs})
	if err != nil {
		return err
	}
	e.fakeErrs = append(e.fakeErrs, b)
	return nil
}
