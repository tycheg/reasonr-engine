package server

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/tycheg/reasonr-engine/internal/host"
	"github.com/tycheg/reasonr-engine/internal/pii"
	"github.com/tycheg/reasonr-engine/internal/registry"
	"github.com/tycheg/reasonr-engine/internal/rows"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Operating personal data (RFC §20).

// piiCommand — reasonrd pii rotate|replay.
func piiCommand(ctx context.Context, args []string, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "rotate":
			return rotateKeys(ctx, args[1:], stderr)
		case "replay":
			return replayForgets(ctx, args[1:], stderr)
		}
	}
	fmt.Fprintln(stderr, "reasonrd: pii rotate — rewrap tenant keys with a new master key; "+
		"pii replay --since <time> — repeat forget after restoring from a backup")
	return 2
}

// rotateKeys rewraps the main store's tenant keys with the master key
// crypto.kms; the previous one is crypto.kms from --from (without it — the
// same KMS: a new Vault Transit key version).
func rotateKeys(ctx context.Context, args []string, stderr io.Writer) int {
	c := newCommon("pii rotate", stderr)
	from := c.fs.String("from", "", "engine.yaml with the previous crypto.kms; by default rewrap with the same KMS")
	if code := c.prepare(ctx, args, stderr, false); code != 0 {
		return code
	}
	defer c.engine.Close()
	rot, ok := rows.As[port.TenantKeyRotator](c.engine.Store)
	if !ok {
		fmt.Fprintln(stderr, "reasonrd: the main store does not keep tenant keys")
		return 2
	}
	if c.engine.KMS == nil {
		fmt.Fprintln(stderr, "reasonrd: crypto.kms is not set")
		return 2
	}
	old := c.engine.KMS
	if *from != "" {
		cfg, err := loadConfig(*from)
		if err != nil {
			fmt.Fprintf(stderr, "reasonrd: %v\n", err)
			return 2
		}
		if cfg.Crypto.KMS == nil {
			fmt.Fprintf(stderr, "reasonrd: %s has no crypto.kms\n", *from)
			return 2
		}
		if old, err = host.BuildKMS(cfg.Crypto.KMS, registry.Deps{Logger: c.log}); err != nil {
			fmt.Fprintf(stderr, "reasonrd: %s: %v\n", *from, err)
			return 2
		}
	}
	res, err := pii.Rotate(ctx, rot, old, c.engine.KMS)
	c.log.Info("tenant keys rewrapped", "rotated", res.Rotated, "skipped", res.Skipped)
	if err != nil {
		fmt.Fprintf(stderr, "reasonrd: %v\n", err)
		return 1
	}
	return 0
}

// replayForgets repeats the forgets from the crypto.forget_log journal
// written no earlier than --since, after restoring the main database from
// a backup: otherwise the backup would bring the forgotten back. It runs
// before the servers start.
func replayForgets(ctx context.Context, args []string, stderr io.Writer) int {
	c := newCommon("pii replay", stderr)
	since := c.fs.String("since", "", "the backup time (RFC 3339): repeat forgets no earlier than it")
	if code := c.prepare(ctx, args, stderr, false); code != 0 {
		return code
	}
	defer c.engine.Close()
	at, err := time.Parse(time.RFC3339Nano, *since)
	if err != nil {
		fmt.Fprintf(stderr, "reasonrd: --since: set the backup time in RFC 3339: %v\n", err)
		return 2
	}
	log, ok := rows.As[port.ForgetLog](c.engine.Stores[c.cfg.Crypto.ForgetLog])
	if !ok {
		fmt.Fprintln(stderr, "reasonrd: crypto.forget_log is not set or the store does not keep the forget journal")
		return 2
	}
	entries, err := log.Forgotten(ctx, at)
	if err != nil {
		fmt.Fprintf(stderr, "reasonrd: forget journal: %v\n", err)
		return 1
	}
	known := map[port.Tenant]bool{}
	for _, t := range c.programs.Tenants() {
		known[t] = true
	}
	var order []port.Tenant
	byTenant := map[port.Tenant][]port.SubjectRef{}
	seen := map[port.Forgotten]bool{}
	skipped := 0
	for _, e := range entries {
		if !known[e.Tenant] {
			skipped++ // the tenant is not in the configuration (removed, or an ephemeral test tenant)
			continue
		}
		key := port.Forgotten{Tenant: e.Tenant, Subject: e.Subject}
		if seen[key] {
			continue
		}
		seen[key] = true
		if byTenant[e.Tenant] == nil {
			order = append(order, e.Tenant)
		}
		byTenant[e.Tenant] = append(byTenant[e.Tenant], e.Subject)
	}
	const batch = 100
	forgotten := 0
	for _, t := range order {
		subjects := byTenant[t]
		for len(subjects) > 0 {
			n := min(batch, len(subjects))
			m, ok := c.engine.Executor.(kind.Maintainer)
			if !ok {
				fmt.Fprintln(stderr, "reasonrd: the engine kind does not forget subjects")
				return 2
			}
			if _, err := m.Forget(ctx, t, subjects[:n]); err != nil {
				fmt.Fprintf(stderr, "reasonrd: tenant %s: %v\n", t, err)
				return 1
			}
			forgotten += n
			subjects = subjects[n:]
		}
	}
	c.log.Info("forget repeated from the journal", "since", at, "subjects", forgotten, "tenants", len(order), "skipped", skipped)
	return 0
}
