package kernel

import (
	"context"
	"errors"
	"fmt"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Fire runs the command of a leased timer and commits its unit of work
// together with deleting the timer row. A business error is a result: the
// effects are dropped, the timer is deleted. An execution or store fault
// releases the lease with a retry after 1, 2, 4… s; after TimerAttempts
// faults the timer becomes dead; error is that fault. A nil response
// without an error: the timer is gone (another instance ran it or a command
// deleted it) or the command does not run because the subject of its
// non-optional personal argument is forgotten. The caller releases the
// response (Release).
func (k *Kernel) Fire(ctx context.Context, t *kind.LeasedTimer) (*kind.CommandResult, error) {
	if k.cfg.Timers == nil {
		return nil, errors.New("kernel: no timer source")
	}
	interp, err := k.cfg.Programs.Interpreter(t.Tenant)
	if err != nil {
		return nil, err
	}
	// A scenario timer fires in its own time: the command's new timers get
	// the same offset (RFC §27.5).
	ctx = port.WithOffset(ctx, t.Offset)
	now := k.clock.Now(t.Tenant).Add(t.Offset)
	if name := forgottenArg(interp.Schema(), t); name != "" {
		return nil, k.dropForgotten(ctx, interp.Schema(), t, now, name)
	}
	call := reasonr.CommandCall{
		Entity: t.Entity, ID: t.EntityID, Command: t.Command, Args: t.Args, Arena: t.Arena,
		Caller: reasonr.Caller{Kind: reasonr.CallerTimer, Tenant: string(t.Tenant)}, Now: now, Contexts: k.cfg.Contexts,
	}
	k.features(t.Tenant, interp.Schema(), &call.Caller, call.Arena)
	res, err := k.execute(ctx, interp, t.Tenant, call, "", t.ID)
	k.timers.Add(1, outcome(res, err))
	if err == nil {
		if res != nil && len(res.Errors) > 0 {
			k.log.Info("timer command rejected", "tenant", string(t.Tenant), "command", k.commandName(interp.Schema(), t),
				"error", res.Errors[0].Name)
		}
		return res, nil
	}
	if ctx.Err() != nil {
		return nil, err
	}
	name := k.commandName(interp.Schema(), t)
	if t.Attempts >= k.cfg.TimerAttempts {
		k.timers.Add(1, "dead")
		k.log.Error("timer dead", "tenant", string(t.Tenant), "command", name, "attempts", t.Attempts, "error", err)
		if derr := k.cfg.Timers.Dead(ctx, t, err.Error()); derr != nil {
			return nil, errors.Join(err, derr)
		}
		return nil, err
	}
	retry := now.Add(Backoff(t.Attempts))
	k.log.Warn("timer fault, retrying", "tenant", string(t.Tenant), "command", name, "attempts", t.Attempts, "retry", retry, "error", err)
	if rerr := k.cfg.Timers.Retry(ctx, t, retry.Add(-t.Offset)); rerr != nil { // the store uses real time
		return nil, errors.Join(err, rerr)
	}
	return nil, err
}

// forgottenArg returns the name of a non-optional personal parameter of
// the timer command whose argument is none: its subject was forgotten
// after scheduling (the store did not decrypt the value). "" — none.
func forgottenArg(s *reasonr.Schema, t *kind.LeasedTimer) string {
	ti := s.Type(t.Entity)
	if ti == nil || int(t.Command) >= len(ti.Methods) {
		return ""
	}
	params := ti.Methods[t.Command].Params
	args := t.Arena.Elems(t.Args.Value())
	for i := range params {
		if params[i].Personal != nil && !params[i].Type.Optional && i < len(args) && args[i].Kind == reasonr.KindNone {
			return params[i].Name
		}
	}
	return ""
}

// dropForgotten deletes a timer whose command cannot run: the subject of
// its personal argument is forgotten (RFC §20). It is an incident: the
// model does not get a command it was waiting for.
func (k *Kernel) dropForgotten(ctx context.Context, s *reasonr.Schema, t *kind.LeasedTimer, now time.Time, param string) error {
	k.timers.Add(1, "forgotten")
	k.log.Error("timer deleted: the subject of a personal argument is forgotten", "tenant", string(t.Tenant),
		"command", k.commandName(s, t), "param", param)
	_, err := k.cfg.Store.Commit(ctx, t.Tenant, &kind.Unit{Arena: t.Arena, Now: now, FiredTimer: t.ID})
	if errors.Is(err, port.ErrTimerGone) {
		return nil
	}
	return err
}

// Backoff is the pause before a retry after attempts failed attempts: 1 s,
// 2 s, 4 s… at most 10 min.
func Backoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > 10 {
		return 10 * time.Minute
	}
	return min(time.Second<<(attempts-1), 10*time.Minute)
}

func (k *Kernel) commandName(s *reasonr.Schema, t *kind.LeasedTimer) string {
	if ti := s.Type(t.Entity); ti != nil && int(t.Command) < len(ti.Methods) {
		return ti.Name + "." + ti.Methods[t.Command].Name
	}
	return fmt.Sprintf("%d.%d", t.Entity, t.Command)
}
