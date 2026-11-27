package ports

import (
	"context"

	"github.com/tycheg/reasonr-engine/port"
)

// KeyCache is a store with a cache of personal data keys: it drops from
// the cache the keys of subjects forgotten by another engine instance
// (RFC §20).
type KeyCache interface {
	DropKeys(tenant port.Tenant, subjects []port.SubjectRef)
}

// ForgetHub broadcasts forgotten subjects between engine instances (RFC
// §20); the notification hub that connects them does it. Without it, an
// instance would decrypt the outbox and responses by key with the old key
// until the cache expires. The stores are correct without it.
type ForgetHub interface {
	// PublishForgotten tells the instances about subjects forgotten by a commit.
	PublishForgotten(ctx context.Context, tenant port.Tenant, subjects []port.SubjectRef) error
	// OnForgotten subscribes fn to these messages, its own included.
	OnForgotten(fn func(tenant port.Tenant, subjects []port.SubjectRef))
}
