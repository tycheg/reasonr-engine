package pii

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/tycheg/reasonr-engine/port"
)

// TenantKeyContext is the KMS context of a tenant key: a wrapped key cannot
// be unwrapped as another tenant's key.
func TenantKeyContext(tenant port.Tenant) string { return "reasonr/tenant/" + string(tenant) }

// Rotated is the result of a master key rotation.
type Rotated struct {
	Rotated int // rewrapped
	Skipped int // already wrapped with the new master key (a repeated rotation)
}

// Rotate rewraps the tenant keys from keys: it unwraps them with the master
// key from and wraps them with the master key to (reasonrd pii rotate, RFC
// §20). The tenant keys themselves do not change, so ciphertexts and the
// caches of running instances stay valid. from and to may be the same —
// that is how one rewraps with a new key version in the KMS (Vault
// Transit). A key that from does not unwrap but to does was already
// rewrapped by an interrupted rotation and is skipped; a key that no one
// unwraps stops the rotation.
func Rotate(ctx context.Context, keys port.TenantKeyRotator, from, to port.KMS) (Rotated, error) {
	var res Rotated
	all, err := keys.TenantKeys(ctx)
	if err != nil {
		return res, err
	}
	tenants := make([]port.Tenant, 0, len(all))
	for t := range all {
		tenants = append(tenants, t)
	}
	sort.Slice(tenants, func(i, j int) bool { return tenants[i] < tenants[j] })
	for _, t := range tenants {
		kc := TenantKeyContext(t)
		raw, err := from.Unwrap(ctx, kc, all[t])
		if err != nil {
			if _, err2 := to.Unwrap(ctx, kc, all[t]); err2 == nil {
				res.Skipped++
				continue
			}
			return res, fmt.Errorf("pii: the tenant key %q does not unwrap with the previous master key: %w", t, err)
		}
		wrapped, err := to.Wrap(ctx, kc, raw)
		if err != nil {
			return res, fmt.Errorf("pii: KMS: %w", err)
		}
		if back, err := to.Unwrap(ctx, kc, wrapped); err != nil || !bytes.Equal(back, raw) {
			return res, fmt.Errorf("pii: the new master key does not unwrap the tenant key %q: %v", t, err)
		}
		ok, err := keys.ReplaceTenantKey(ctx, t, all[t], wrapped)
		if err != nil {
			return res, err
		}
		if !ok {
			return res, fmt.Errorf("pii: the tenant key %q changed during the rotation", t)
		}
		res.Rotated++
	}
	return res, nil
}
