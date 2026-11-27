package kind

import (
	"context"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/port"
)

// Catalog holds the model schemas of data tenants: adapters encode
// records, build index keys and search by them.
type Catalog interface {
	Schema(t port.Tenant) (*reasonr.Schema, error)
}

// NotificationHub carries addressed notifications between server
// instances: one ran the command, the recipient is connected to another.
// Delivery is at most once and only to connected recipients (RFC §13).
type NotificationHub interface {
	// Publish sends notifications to their recipients (Notification.To).
	Publish(ctx context.Context, tenant port.Tenant, a *reasonr.Arena, ns []reasonr.Notification) error
	// Subscribe subscribes to the notifications of the actor actor.
	Subscribe(ctx context.Context, tenant port.Tenant, a *reasonr.Arena, actor reasonr.Value) (Subscription, error)
}

// FeatureSource computes the feature flags of the caller c of the tenant
// tenant in the model with schema s; the caller's Subject is in the arena
// a (RFC §23.4).
type FeatureSource interface {
	Features(tenant port.Tenant, s *reasonr.Schema, c *reasonr.Caller, a *reasonr.Arena) reasonr.Features
}

// LeasedRequest is a leased external request at the level of model values:
// what the request queue gives the sender. The values lie in Arena,
// personal arguments are decrypted (none for forgotten subjects). The
// sender fills the ready request (port.LeasedRequest) before a transport
// of another module gets it.
type LeasedRequest struct {
	port.LeasedRequest
	Request reasonr.ExternalRequest
	Arena   *reasonr.Arena
}
