package memory

import (
	"context"
	"sort"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/storekit"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

var _ kind.StateTransfer = (*Store)(nil)

// ExportState returns a tenant's live timers and counters
// (port.StateTransfer).
func (s *Store) ExportState(ctx context.Context, tenant port.Tenant, a *reasonr.Arena) (kind.EngineState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.space(tenant)
	if err != nil {
		return kind.EngineState{}, err
	}
	st := kind.EngineState{Sequences: map[reasonr.TypeRef]int64{}}
	for t, v := range sp.seqs {
		st.Sequences[t] = v
	}
	timers := make([]*timer, 0, len(sp.timers))
	for _, t := range sp.timers {
		if t.dead == "" {
			timers = append(timers, t)
		}
	}
	sort.Slice(timers, func(i, j int) bool { return timers[i].order < timers[j].order })
	for _, t := range timers {
		lt := kind.LeasedTimer{Tenant: tenant, ID: t.id, Entity: t.entity, EntityID: t.entityID, Command: t.command, Args: t.args, Arena: t.a}
		var subjects []reasonr.ArgSubject
		if t.sealed != nil {
			if err := s.openTimer(ctx, &lt, t.sealed); err != nil {
				return kind.EngineState{}, err
			}
			mi := &sp.schema.Type(t.entity).Methods[t.command]
			ss, err := storekit.DecodeSealed(mi.Params, t.sealed)
			if err != nil {
				return kind.EngineState{}, err
			}
			if subjects, err = sp.crypto.ArgSubjectsOf(a, ss); err != nil {
				return kind.EngineState{}, err
			}
		}
		st.Timers = append(st.Timers, kind.TimerState{
			ID: a.Copy(t.a, t.id), At: t.at, Offset: t.offset, Entity: t.entity, EntityID: a.Copy(t.a, t.entityID),
			Command: t.command, Args: a.Copy(lt.Arena, lt.Args.Value()).Record(), Subjects: subjects,
		})
	}
	return st, nil
}

// ClearTimers deletes a tenant's timers (port.StateTransfer).
func (s *Store) ClearTimers(_ context.Context, tenant port.Tenant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.space(tenant)
	if err != nil {
		return err
	}
	clear(sp.timers)
	return nil
}

// ImportSequences raises a tenant's counters (port.StateTransfer).
func (s *Store) ImportSequences(_ context.Context, tenant port.Tenant, seqs map[reasonr.TypeRef]int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.space(tenant)
	if err != nil {
		return err
	}
	for t, v := range seqs {
		sp.seqs[t] = max(sp.seqs[t], v)
	}
	return nil
}
