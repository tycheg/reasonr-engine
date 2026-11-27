package kernel

import (
	"context"
	"errors"
	"fmt"
	"strings"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/port"
)

// Migrate runs the manual migration name of the tenant's model: each unit
// of work (a command call from the migration) is committed separately
// before the next call (RFC §32, item 3). A business error, a uniqueness
// violation or a version conflict stops the migration; what is committed stays.
func (k *Kernel) Migrate(ctx context.Context, tenant port.Tenant, name string) error {
	interp, err := k.cfg.Programs.Interpreter(tenant)
	if err != nil {
		return err
	}
	mg, ok := interp.(Migrator)
	if !ok {
		return ErrNoMigrations
	}
	s := interp.Schema()
	now := k.clock.Now(tenant)
	caller := reasonr.Caller{Kind: reasonr.CallerMigration, Tenant: string(tenant)}
	stop := func(errs []reasonr.BusinessError) error {
		names := make([]string, len(errs))
		for i := range errs {
			names[i] = errs[i].Name
		}
		return fmt.Errorf("kernel: migration %s stopped: error %s", name, strings.Join(names, ", "))
	}
	return mg.Migrate(ctx, name, &env{k: k, tenant: tenant, schema: s}, now, func(out *reasonr.Outcome) error {
		if len(out.Errors) > 0 {
			return stop(out.Errors)
		}
		k.address(out, caller, nil)
		pos, err := k.cfg.Store.Commit(ctx, tenant, k.unit(s, now, out, "", reasonr.None, nil))
		var uv *ports.UniqueViolation
		switch {
		case errors.As(err, &uv):
			return stop([]reasonr.BusinessError{uniqueError(s, out.Arena, uv)})
		case err != nil:
			return fmt.Errorf("kernel: migration %s stopped: %w", name, err)
		}
		k.publish(ctx, tenant, pos, now, out, caller, nil)
		return nil
	})
}
