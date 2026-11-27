// Package transfer moves a tenant's state between engines (reasonrd
// export, RFC §32): timers, sequence counters and entities.
package transfer

import (
	"context"
	"fmt"
	"sort"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// ImportState loads timers and counters into the store: timers as units of
// work (one per scenario clock offset), so that the store encrypts personal
// arguments with its own keys; now is the time of the units by the
// receiver's clock. Loading the same timer again is an identifier
// conflict, so load into an empty tenant.
func ImportState(ctx context.Context, dst kind.UnitStore, tenant port.Tenant, a *reasonr.Arena, st kind.EngineState, now time.Time) error {
	byOffset := map[time.Duration][]reasonr.TimerOp{}
	var offsets []time.Duration
	for _, t := range st.Timers {
		if _, ok := byOffset[t.Offset]; !ok {
			offsets = append(offsets, t.Offset)
		}
		byOffset[t.Offset] = append(byOffset[t.Offset], reasonr.TimerOp{Kind: reasonr.TimerSchedule, ID: t.ID, At: t.At.Add(t.Offset),
			Entity: t.Entity, EntityID: t.EntityID, Command: t.Command, Args: t.Args, Subjects: t.Subjects})
	}
	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
	for _, off := range offsets {
		u := &kind.Unit{Arena: a, Work: &reasonr.UnitOfWork{Timers: byOffset[off]}, Offset: off, Now: now, FiredTimer: reasonr.None}
		if _, err := dst.Commit(ctx, tenant, u); err != nil {
			return fmt.Errorf("timers: %w", err)
		}
	}
	if len(st.Sequences) == 0 {
		return nil
	}
	tr, ok := dst.(kind.StateTransfer)
	if !ok {
		return fmt.Errorf("the store does not take sequence counters (port.StateTransfer)")
	}
	return tr.ImportSequences(ctx, tenant, st.Sequences)
}

// Stored reports whether a type is stored as rows: aggregates and actors
// (inner entities live in the document of their aggregate).
func Stored(ti *reasonr.TypeInfo) bool { return kind.StoredType(ti) }

// Entities walks the tenant's entities and actors in the store src by type:
// the state and version of each row.
func Entities(ctx context.Context, src kind.Reader, s *reasonr.Schema, tenant port.Tenant, each func(a *reasonr.Arena, rows []kind.EntityState) error) error {
	for i := range s.Types {
		ti := &s.Types[i]
		if !Stored(ti) {
			continue
		}
		a := reasonr.NewArena()
		rows, err := src.Find(ctx, tenant, a, &reasonr.FindSpec{Type: ti.Ref, Index: reasonr.NoIndex, Limit: -1, Cursor: reasonr.None})
		if err != nil {
			return fmt.Errorf("%s: %w", ti.Name, err)
		}
		var batch []kind.EntityState
		for _, rec := range a.Elems(rows.Records) {
			fields := a.Elems(rec)
			id := fields[len(fields)-1]
			st, version, exists, err := src.Load(ctx, tenant, a, ti.Ref, id)
			if err != nil {
				return fmt.Errorf("%s: %w", ti.Name, err)
			}
			if exists {
				batch = append(batch, kind.EntityState{Type: ti.Ref, ID: id, State: st, Version: version})
			}
		}
		if len(batch) > 0 {
			if err := each(a, batch); err != nil {
				return err
			}
		}
	}
	return nil
}
