package kernel

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/internal/storekit"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Command runs a command and commits its unit of work (RFC §30.6): on a
// version conflict the command is repeated, a uniqueness violation becomes
// the error from IndexInfo.Error. The response by idempotency key is stored
// together with the unit of work; a repeated call with the same key gets it
// without running the command. After commit, notifications to other actors
// go to the hub, notifications to the initiator to the response. The kernel
// sets the call time. The caller releases the response (Release).
func (k *Kernel) Command(ctx context.Context, req *kind.CommandRequest) (*kind.CommandResult, error) {
	interp, err := k.cfg.Programs.Interpreter(req.Tenant)
	if err != nil {
		return nil, err
	}
	if err := k.local(interp.Schema(), req.Call.Entity); err != nil {
		return nil, err
	}
	call := req.Call
	call.Caller.Tenant = string(req.Tenant)
	call.Now = k.clock.Now(req.Tenant).Add(port.Offset(ctx))
	call.Contexts = k.cfg.Contexts
	k.features(req.Tenant, interp.Schema(), &call.Caller, call.Arena)
	if req.Idempotency != "" {
		if res, err := k.remembered(ctx, req.Tenant, req.Idempotency, call.Now); res != nil || err != nil {
			return res, err
		}
	}
	if call.ID.Kind == reasonr.KindNone {
		// A create command: the engine issues the identifier (RFC §10.3).
		ti := interp.Schema().Type(call.Entity)
		if ti == nil || int(call.Command) >= len(ti.Methods) || !ti.Methods[call.Command].Create {
			return nil, fmt.Errorf("kernel: only a create command is called without an identifier")
		}
		if call.ID, err = k.newID(ctx, req.Tenant, interp.Schema(), call.Entity); err != nil {
			return nil, err
		}
	}
	start := time.Now()
	res, err := k.execute(ctx, interp, req.Tenant, call, req.Idempotency, reasonr.None)
	k.duration.Observe(time.Since(start).Seconds())
	k.commands.Add(1, outcome(res, err))
	return res, err
}

func outcome(res *kind.CommandResult, err error) string {
	switch {
	case err != nil:
		return "failed"
	case res == nil:
		return "skipped"
	case len(res.Errors) > 0:
		return "rejected"
	}
	return "accepted"
}

// remembered returns the response stored by key, or nil.
func (k *Kernel) remembered(ctx context.Context, tenant port.Tenant, key string, now time.Time) (*kind.CommandResult, error) {
	a := reasonr.GetArena()
	res, err := k.cfg.Store.Remembered(ctx, tenant, key, now, a)
	if res == nil || err != nil {
		reasonr.PutArena(a)
		return nil, err
	}
	res.Arena, res.Outcome, res.Replayed = a, nil, true
	return res, nil
}

// execute is the loop "execution → commit → repeat". fired is the Timer.Id
// of the fired timer or None. A nil response without an error — the timer is gone.
func (k *Kernel) execute(ctx context.Context, interp reasonr.Interpreter, tenant port.Tenant, call reasonr.CommandCall, key string, fired reasonr.Value) (*kind.CommandResult, error) {
	s := interp.Schema()
	e := &env{k: k, tenant: tenant, schema: s}
	for attempt := range k.cfg.Retries {
		if attempt > 0 {
			if err := conflictPause(ctx, attempt); err != nil {
				return nil, err
			}
		}
		out, err := k.run(ctx, interp, call, e)
		if err != nil {
			return nil, err
		}
		if len(out.Errors) > 0 {
			return k.reject(ctx, tenant, call, key, fired, out)
		}
		if k.cfg.BeforeCommit != nil {
			k.cfg.BeforeCommit(out.Work)
		}
		res := &kind.CommandResult{Arena: out.Arena, Outcome: out, ID: reasonr.None}
		if ti := s.Type(call.Entity); ti != nil && int(call.Command) < len(ti.Methods) && ti.Methods[call.Command].Create {
			res.ID = call.ID // the issued identifier goes to the response and to the response by retry key
		}
		res.Notifications = k.address(out, call.Caller, call.Arena)
		u := k.unit(s, call.Now, out, key, fired, res)
		u.Offset = port.Offset(ctx)
		pos, err := k.cfg.Store.Commit(ctx, tenant, u)
		if err == nil && k.cfg.OnRequests != nil && out.Work != nil && len(out.Work.Requests) > 0 {
			k.cfg.OnRequests()
		}
		var uv *ports.UniqueViolation
		switch {
		case errors.As(err, &uv):
			out.SetErrors([]reasonr.BusinessError{uniqueError(s, out.Arena, uv)})
			return k.reject(ctx, tenant, call, key, fired, out)
		case errors.Is(err, port.ErrConflict):
			k.conflicts.Add(1)
			out.Release()
			continue
		case errors.Is(err, port.ErrDuplicateKey):
			out.Release()
			return k.remembered(ctx, tenant, key, call.Now)
		case errors.Is(err, port.ErrTimerGone):
			out.Release()
			return nil, nil
		case err != nil:
			out.Release()
			return nil, err
		}
		res.Position = pos
		k.publish(ctx, tenant, pos, call.Now, out, call.Caller, call.Arena)
		k.forgotten(ctx, tenant, s, out)
		return res, nil
	}
	return nil, fmt.Errorf("%w: the command was not committed in %d attempts", port.ErrConflict, k.cfg.Retries)
}

// conflictPause is a random pause before a repeat after a version
// conflict: concurrent commands of one entity spread out in time instead
// of colliding again. The upper bound doubles with each attempt: 1, 2, 4…
// ms, at most 50 ms.
func conflictPause(ctx context.Context, attempt int) error {
	limit := min(time.Millisecond<<(attempt-1), 50*time.Millisecond)
	t := time.NewTimer(time.Duration(rand.Int64N(int64(limit)) + 1))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// reject finishes a rejected command: the effects are dropped, but the
// response by key is stored and the fired timer is deleted.
func (k *Kernel) reject(ctx context.Context, tenant port.Tenant, call reasonr.CommandCall, key string, fired reasonr.Value, out *reasonr.Outcome) (*kind.CommandResult, error) {
	res := &kind.CommandResult{Errors: out.Errors, Arena: out.Arena, Outcome: out}
	if key == "" && fired.Kind == reasonr.KindNone {
		return res, nil
	}
	u := &kind.Unit{Arena: out.Arena, Now: call.Now, FiredTimer: fired}
	if key != "" {
		u.Idempotency, u.Result, u.Expires = key, res, call.Now.Add(k.cfg.IdempotencyTTL)
	}
	_, err := k.cfg.Store.Commit(ctx, tenant, u)
	switch {
	case errors.Is(err, port.ErrDuplicateKey):
		out.Release()
		return k.remembered(ctx, tenant, key, call.Now)
	case errors.Is(err, port.ErrTimerGone):
		out.Release()
		return nil, nil
	case err != nil:
		out.Release()
		return nil, err
	}
	return res, nil
}

// unit assembles a unit of work to commit: projection operations are split
// into synchronous and asynchronous ones.
func (k *Kernel) unit(s *reasonr.Schema, now time.Time, out *reasonr.Outcome, key string, fired reasonr.Value, res *kind.CommandResult) *kind.Unit {
	w := out.Work
	u := &kind.Unit{Arena: out.Arena, Work: w, Projections: w.Projections, Now: now, FiredTimer: fired, Contexts: k.cfg.Contexts}
	if key != "" {
		u.Idempotency, u.Result, u.Expires = key, res, now.Add(k.cfg.IdempotencyTTL)
	}
	if len(k.placement(s)) == 0 {
		return u
	}
	u.Projections = nil
	for _, op := range w.Projections {
		if k.async(s, op.Projection) != nil {
			u.Async = append(u.Async, op)
		} else {
			u.Projections = append(u.Projections, op)
		}
	}
	return u
}

// address addresses notifications without a recipient to the initiator of
// the root command (RFC §13) and returns those addressed to them. If there
// is no initiator (a timer, a migration, a role), such notifications are
// not sent. src is the arena holding the caller's Subject.
func (k *Kernel) address(out *reasonr.Outcome, caller reasonr.Caller, src *reasonr.Arena) []reasonr.Notification {
	w, a := out.Work, out.Arena
	var mine []reasonr.Notification
	for i := range w.Notifications {
		n := &w.Notifications[i]
		if caller.Subject.Kind == reasonr.KindNone {
			continue
		}
		if n.To.Kind == reasonr.KindNone && (src != nil || caller.Subject.Ref.Len == 0) {
			n.To = a.Copy(src, caller.Subject)
		}
		if toCaller(a, n.To, caller, src) {
			mine = append(mine, *n)
		}
	}
	return mine
}

// publish hands notifications to other actors to the hub, and every
// notification of the unit with position pos to the bus (Notify). A hub
// error does not cancel the committed command: notifications are delivered
// at most once (RFC §13).
func (k *Kernel) publish(ctx context.Context, tenant port.Tenant, pos port.Position, at time.Time, out *reasonr.Outcome, caller reasonr.Caller, src *reasonr.Arena) {
	if k.cfg.Notify != nil && len(out.Work.Notifications) > 0 {
		k.cfg.Notify(tenant, pos, at, out.Arena, out.Work.Notifications)
	}
	if k.cfg.Hub == nil {
		return
	}
	var others []reasonr.Notification
	for _, n := range out.Work.Notifications {
		if n.To.Kind == reasonr.KindNone {
			continue
		}
		if toCaller(out.Arena, n.To, caller, src) {
			continue
		}
		others = append(others, n)
	}
	if len(others) == 0 {
		return
	}
	if err := k.cfg.Hub.Publish(ctx, tenant, out.Arena, others); err != nil {
		k.log.Warn("notifications not passed to the hub", "tenant", string(tenant), "error", err)
	}
}

// forgotten tells other instances through the hub about the subjects the
// unit of work forgot: they drop their keys from the cache (RFC §20). A hub
// error does not cancel the commit.
func (k *Kernel) forgotten(ctx context.Context, tenant port.Tenant, s *reasonr.Schema, out *reasonr.Outcome) {
	fh, ok := k.cfg.Hub.(ports.ForgetHub)
	if !ok || len(out.Work.Forget) == 0 {
		return
	}
	refs, err := storekit.Refs(s, out.Arena, out.Work.Forget)
	if err == nil {
		err = fh.PublishForgotten(ctx, tenant, refs)
	}
	if err != nil {
		k.log.Warn("forgotten subjects not passed to the hub", "tenant", string(tenant), "error", err)
	}
}

// toCaller reports whether a notification (the recipient to in the arena
// a) is addressed to the command initiator.
func toCaller(a *reasonr.Arena, to reasonr.Value, caller reasonr.Caller, src *reasonr.Arena) bool {
	if caller.Subject.Kind == reasonr.KindNone {
		return false
	}
	if src == nil {
		src = a
	}
	return reasonr.Equal(a, to, src, caller.Subject)
}

// run runs a command; with CheckDeterminism — twice: the second execution
// gets the same identifiers, and the results are compared.
func (k *Kernel) run(ctx context.Context, interp reasonr.Interpreter, call reasonr.CommandCall, e *env) (*reasonr.Outcome, error) {
	if !k.cfg.CheckDeterminism {
		e.ids = nil
		return interp.Execute(ctx, call, e)
	}
	e.ids = &idLog{}
	first, err := interp.Execute(ctx, call, e)
	if err != nil {
		return nil, err
	}
	e.ids.replay = true
	second, err := interp.Execute(ctx, call, e)
	if err != nil {
		first.Release()
		return nil, fmt.Errorf("%w: %v", ErrNondetermined, err)
	}
	defer second.Release()
	if !e.ids.done() {
		first.Release()
		return nil, fmt.Errorf("%w: other identifiers were issued", ErrNondetermined)
	}
	if what := diffOutcomes(first, second); what != "" {
		first.Release()
		return nil, fmt.Errorf("%w: %s", ErrNondetermined, what)
	}
	return first, nil
}

// Deliver runs the ACL reactions of this process's bounded contexts to an
// event that a process of another bounded context committed (RFC §23.2):
// the event ev of the entity t with the identifier id and the arguments
// args (in the arena a). key is the retry key: "at least once" delivery
// does not run reactions twice. A nil response without an error — this
// process has no reactions to the event.
func (k *Kernel) Deliver(ctx context.Context, tenant port.Tenant, a *reasonr.Arena, t reasonr.TypeRef, id reasonr.Value, ev reasonr.MethodRef, args reasonr.Record, key string) (*kind.CommandResult, error) {
	interp, err := k.cfg.Programs.Interpreter(tenant)
	if err != nil {
		return nil, err
	}
	call := reasonr.CommandCall{
		Entity: t, ID: id, Command: ev, Args: args, Arena: a, Event: true, Contexts: k.cfg.Contexts,
		Caller: reasonr.Caller{Kind: reasonr.CallerMigration, Subject: reasonr.None, Tenant: string(tenant)},
		Now:    k.clock.Now(tenant),
	}
	if res, err := k.remembered(ctx, tenant, key, call.Now); res != nil || err != nil {
		return res, err
	}
	return k.execute(ctx, interp, tenant, call, key, reasonr.None)
}
