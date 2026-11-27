package dispatch

import (
	"context"
	"fmt"
	"slices"
	"strings"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// ACL is an effect stream consumer that delivers events of other bounded
// contexts to the ACL reactions of this process's bounded contexts (RFC
// §23.2). The processes share the main store: the process of the source
// bounded context commits the unit of work, and this process runs the ACL
// reactions of the bounded contexts that process does not have
// (CommittedUnit.Contexts) — in its own unit of work per event, with a
// retry key: "at least once" delivery does not run a reaction twice. A
// reaction that returned an error is a dead letter.
type ACL struct {
	Contexts []string
	Catalog  kind.Catalog
	// Run runs the reactions to an event (kernel.Kernel.Deliver).
	Run func(ctx context.Context, tenant port.Tenant, a *reasonr.Arena, t reasonr.TypeRef, id reasonr.Value,
		ev reasonr.MethodRef, args reasonr.Record, key string) (*kind.CommandResult, error)
}

func (c *ACL) Name() string { return "acl:" + strings.Join(c.Contexts, ",") }

func (c *ACL) Deliver(ctx context.Context, u *kind.CommittedUnit, only int, bury func(int, string) error) error {
	if len(u.Entities) == 0 || u.Contexts == nil {
		return nil // a process of every bounded context ran the unit: the reactions are already in it
	}
	s, err := c.Catalog.Schema(u.Tenant)
	if err != nil {
		return err
	}
	item := -1
	for _, ee := range u.Entities {
		ti := s.Type(ee.Type)
		for _, ev := range ee.Events {
			item++
			if only >= 0 && item != only || ti == nil || int(ev.Name) >= len(ti.Methods) || !c.receives(ti.Methods[ev.Name].ACL, u.Contexts) {
				continue
			}
			key := fmt.Sprintf("acl/%s/%d/%d", c.Name(), u.Position, item)
			res, err := c.Run(ctx, u.Tenant, u.Arena, ee.Type, ee.ID, ev.Name, ev.Args, key)
			if err != nil {
				return err
			}
			if res == nil {
				continue
			}
			if len(res.Errors) > 0 {
				reason := fmt.Sprintf("the ACL reaction to %s.%s is rejected: %s", ti.Name, ti.Methods[ev.Name].Name, res.Errors[0].Name)
				if err := bury(item, reason); err != nil {
					res.Release()
					return err
				}
			}
			res.Release()
		}
	}
	return nil
}

// receives reports whether, among the bounded contexts whose ACLs react to
// the event (reactors), there is one of this process whose reactions the
// unit's process did not run (done).
func (c *ACL) receives(reactors, done []string) bool {
	for _, r := range reactors {
		if slices.Contains(c.Contexts, r) && !slices.Contains(done, r) {
			return true
		}
	}
	return false
}
