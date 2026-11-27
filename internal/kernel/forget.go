package kernel

import (
	"context"
	"errors"
	"fmt"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/storekit"
	"github.com/tycheg/reasonr-engine/port"
)

// Forget forgets subjects outside a model command — reasonrd pii replay
// after the main database is restored from a backup (RFC §20.3): one unit
// of work with UnitOfWork.Forget. The store destroys the subject keys and
// erases their data, the effect stream erases the data of asynchronous
// projections, the hub tells other instances. A repeated forget of a
// forgotten subject loses nothing: forget is idempotent.
func (k *Kernel) Forget(ctx context.Context, tenant port.Tenant, subjects []port.SubjectRef) (port.Position, error) {
	interp, err := k.cfg.Programs.Interpreter(tenant)
	if err != nil {
		return 0, err
	}
	s := interp.Schema()
	c := storekit.NewCrypto(s)
	for attempt := 0; attempt < k.cfg.Retries; attempt++ {
		a := reasonr.NewArena()
		out := &reasonr.Outcome{Arena: a, Work: &reasonr.UnitOfWork{}}
		for _, ref := range subjects {
			subj, err := c.Subject(a, ref)
			if err != nil {
				return 0, err
			}
			out.Work.Forget = append(out.Work.Forget, subj)
		}
		pos, err := k.cfg.Store.Commit(ctx, tenant, k.unit(s, k.clock.Now(tenant), out, "", reasonr.None, nil))
		if errors.Is(err, port.ErrConflict) {
			continue
		}
		if err != nil {
			return 0, err
		}
		k.forgotten(ctx, tenant, s, out)
		return pos, nil
	}
	return 0, fmt.Errorf("%w: forget was not committed in %d attempts", port.ErrConflict, k.cfg.Retries)
}
