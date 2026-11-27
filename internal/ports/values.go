package ports

import (
	"context"
	"fmt"

	"github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// SchemaStore is a store that needs the model schema in advance (tables,
// key columns, indexes): the server calls it at startup and in a migration
// for each model and its data tenants.
type SchemaStore interface {
	// LastSchema returns the schema of the model written last; nil — the
	// model was never deployed.
	LastSchema(ctx context.Context, model string) (*reasonr.Schema, error)
	// EnsureSchema creates missing tables, key columns and indexes, fills
	// new key columns in the rows of the tenants tenants and records the
	// schema version.
	EnsureSchema(ctx context.Context, model string, s *reasonr.Schema, tenants []port.Tenant) error
}

// EventStore is the event store (audit, analytics).
type EventStore interface {
	// Append appends the events of units of work; a repeated unit (by
	// position) changes nothing. Personal arguments are encrypted with the
	// key versions CommittedUnit.Keys, as in ProjectionStore.Apply.
	Append(ctx context.Context, units []kind.CommittedUnit) error
}

// UniqueViolation is a unique index violation at commit: the kernel turns
// it into the business error from IndexInfo.Error.
type UniqueViolation struct {
	Type  reasonr.TypeRef
	Index reasonr.IndexRef
	// Key holds the index key values in the arena of the unit of work.
	Key []reasonr.Value
}

func (u *UniqueViolation) Error() string {
	return fmt.Sprintf("unique index %d of type %d violated", u.Index, u.Type)
}
