// Package features is the engine's feature flag rollout (RFC §23.4): the
// rules from the store (port.FeatureStore) in memory and the caller's flag
// set, which the kernel passes to the interpreter once per unit of work
// (reasonr.Caller.Features). Instances learn about changes through the hub
// (ports.FeatureHub) and reread the rules every Refresh.
package features

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tycheg/reasonr-engine/internal/codecs"
	"github.com/tycheg/reasonr-engine/internal/ports"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/port"
)

// Refresh is how often an instance rereads the rules without a hub message.
const Refresh = 5 * time.Second

// Resolver holds the rollout rules in memory. Its methods are safe for
// concurrent calls. A zero Store means no rules: only tests switch flags on
// (SetTest).
type Resolver struct {
	Store port.FeatureStore
	Hub   ports.FeatureHub
	Log   *slog.Logger

	mu      sync.RWMutex
	rules   []port.FeatureRule
	test    map[port.Tenant][]string // the flags of ephemeral test tenants
	version uint64
	views   map[*reasonr.Schema]*view
}

// New returns a rollout on the store store (nil — without a store) and the
// hub hub (nil — without broadcasting).
func New(store port.FeatureStore, hub ports.FeatureHub, log *slog.Logger) *Resolver {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	r := &Resolver{Store: store, Hub: hub, Log: log}
	if hub != nil {
		hub.OnFeatures(func() {
			if err := r.Load(context.Background()); err != nil {
				r.Log.Error("feature flags not reread", "error", err)
			}
		})
	}
	return r
}

// Load rereads the rules from the store.
func (r *Resolver) Load(ctx context.Context) error {
	if r.Store == nil {
		return nil
	}
	rules, err := r.Store.FeatureRules(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rules = rules
	r.changed()
	return nil
}

// changed resets the computed sets; called under r.mu.
func (r *Resolver) changed() {
	r.version++
	r.views = nil
}

// Run rereads the rules every Refresh until ctx is cancelled.
func (r *Resolver) Run(ctx context.Context) error {
	if r.Store == nil {
		return nil
	}
	if l, ok := r.Hub.(ports.Listening); ok {
		if err := l.Listen(ctx); err != nil {
			r.Log.Warn("the hub does not listen to feature flag changes: rereading only", "error", err)
		}
	}
	tick := time.NewTicker(Refresh)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if err := r.Load(ctx); err != nil && ctx.Err() == nil {
				r.Log.Warn("feature flags not reread", "error", err)
			}
		}
	}
}

// Change applies a rollout change, rereads the rules and tells the
// instances about it.
func (r *Resolver) Change(ctx context.Context, c port.FeatureChange) error {
	if err := r.Store.ChangeFeature(ctx, c); err != nil {
		return err
	}
	if err := r.Load(ctx); err != nil {
		return err
	}
	if r.Hub != nil {
		return r.Hub.PublishFeatures(ctx)
	}
	return nil
}

// Rules returns the rollout rules in memory.
func (r *Resolver) Rules() []port.FeatureRule {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.rules)
}

// SetTest switches the flag name on in the tenant tenant for a test: a
// feature line at the start of a test (RFC §23.4). It is not written to the store.
func (r *Resolver) SetTest(tenant port.Tenant, name string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.test == nil {
		r.test = map[port.Tenant][]string{}
	}
	if !slices.Contains(r.test[tenant], name) {
		r.test[tenant] = append(r.test[tenant], name)
		r.changed()
	}
}

// DropTest switches off the tenant's test flags.
func (r *Resolver) DropTest(tenant port.Tenant) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.test[tenant]; ok {
		delete(r.test, tenant)
		r.changed()
	}
}

// Features returns the flags switched on for the caller c of the tenant
// tenant in the model with schema s; the caller's Subject is in the arena
// a. It does not allocate once the schema's sets are computed.
func (r *Resolver) Features(tenant port.Tenant, s *reasonr.Schema, c *reasonr.Caller, a *reasonr.Arena) reasonr.Features {
	if r == nil || len(s.Features) == 0 {
		return reasonr.Features{}
	}
	v := r.view(s)
	f := v.system
	f.Or(v.tenants[tenant])
	for _, role := range c.Roles {
		f.Or(v.roles[roleKey{tenant, role}])
	}
	if c.Actor != 0 && len(v.actors) > 0 {
		if ti := s.Type(c.Actor); ti != nil {
			name := entityName(ti)
			for i := range v.actors {
				x := &v.actors[i]
				if (x.tenant == "" || x.tenant == tenant) && x.entity == name && reasonr.Equal(v.arena, x.id, a, c.Subject) {
					f.Or(x.set)
				}
			}
		}
	}
	return f
}

// view holds the rules laid out by the schema's flag sets.
type view struct {
	system  reasonr.Features
	tenants map[port.Tenant]reasonr.Features
	roles   map[roleKey]reasonr.Features
	actors  []actorSet
	arena   *reasonr.Arena // actor identifiers
}

type roleKey struct {
	tenant port.Tenant
	role   string
}

type actorSet struct {
	tenant port.Tenant
	entity string
	id     reasonr.Value
	set    reasonr.Features
}

func (r *Resolver) view(s *reasonr.Schema) *view {
	r.mu.RLock()
	v := r.views[s]
	r.mu.RUnlock()
	if v != nil {
		return v
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if v = r.views[s]; v != nil {
		return v
	}
	v = r.build(s)
	if r.views == nil {
		r.views = map[*reasonr.Schema]*view{}
	}
	r.views[s] = v
	return v
}

// build lays the rules out by the flag sets of the schema s; called under r.mu.
func (r *Resolver) build(s *reasonr.Schema) *view {
	v := &view{tenants: map[port.Tenant]reasonr.Features{}, roles: map[roleKey]reasonr.Features{}, arena: reasonr.NewArena()}
	for tenant, names := range r.test {
		for _, name := range names {
			if i := s.Feature(name); i >= 0 {
				set := v.tenants[tenant]
				set.Set(i)
				v.tenants[tenant] = set
			}
		}
	}
	codec := codecs.JSONOf(s, codecs.Options{})
	for _, rule := range r.rules {
		i := s.Feature(rule.Feature)
		if i < 0 {
			continue // the flag was removed from the code or is from another model
		}
		sc := rule.Scope
		switch {
		case sc.Actor != "":
			typ, id, _ := strings.Cut(sc.Actor, ":")
			ti := s.Lookup(typ)
			if ti == nil || ti.Kind != reasonr.TypeActor {
				continue
			}
			val, err := codec.DecodeIDText(v.arena, ti.Ref, id)
			if err != nil {
				r.Log.Warn("feature flag: a wrong actor", "feature", rule.Feature, "actor", sc.Actor, "error", err)
				continue
			}
			x := actorSet{tenant: sc.Tenant, entity: entityName(ti), id: val}
			x.set.Set(i)
			v.actors = append(v.actors, x)
		case sc.Role != "":
			k := roleKey{sc.Tenant, s.Role(sc.Role)}
			set := v.roles[k]
			set.Set(i)
			v.roles[k] = set
		case sc.Tenant != "":
			set := v.tenants[sc.Tenant]
			set.Set(i)
			v.tenants[sc.Tenant] = set
		default:
			v.system.Set(i)
		}
	}
	return v
}

// Resolve returns the full name of a flag of the schema s by the name name:
// the full one ("Ordering.tips") or the short one if it is unique in the
// schema; "" — no flag.
func Resolve(s *reasonr.Schema, name string) string {
	if s.Feature(name) >= 0 {
		return name
	}
	found := ""
	for _, f := range s.Features {
		if short, ok := strings.CutPrefix(f.Name, contextOf(f.Name)+"."); ok && short == name || f.Name == name {
			if found != "" {
				return ""
			}
			found = f.Name
		}
	}
	return found
}

func contextOf(name string) string {
	s, _, _ := strings.Cut(name, ".")
	return s
}

// entityName is the actor name for comparison: the type's full name ("Ordering.Customer").
func entityName(ti *reasonr.TypeInfo) string { return ti.Name }

// Apply applies a change to the rules: the reference for store adapters.
func Apply(rules []port.FeatureRule, c port.FeatureChange) []port.FeatureRule {
	if c.On {
		r := port.FeatureRule{Feature: c.Feature, Scope: c.Scope}
		if !slices.Contains(rules, r) {
			rules = append(rules, r)
		}
		return rules
	}
	return slices.DeleteFunc(rules, func(r port.FeatureRule) bool {
		return r.Feature == c.Feature && (c.Everywhere || r.Scope == c.Scope)
	})
}
