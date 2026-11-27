package port

import (
	"context"
	"time"
)

// Feature flags (RFC §23.4): which code flags are on for whom. The engine
// keeps the state, not engine.yaml: switching on needs no restart. A flag
// is on for a caller if it is on at least at one of its levels: actor, role
// in a tenant, tenant, everyone.

// FeatureScope is a flag rollout level. Empty — everyone (every tenant);
// Tenant — a tenant; Role with Tenant — a role in a tenant; Actor — an actor
// ("Ordering.Customer:<id>", as in the Test actor= header), with Tenant —
// only in that tenant.
type FeatureScope struct {
	Tenant Tenant `json:"tenant,omitempty"`
	Role   string `json:"role,omitempty"`
	Actor  string `json:"actor,omitempty"`
}

// All reports whether the level is everyone.
func (s FeatureScope) All() bool { return s == FeatureScope{} }

// String is the level for the journal and output: "all", "tenant=acme",
// "role=admin tenant=acme", "actor=Ordering.Customer:7 tenant=acme".
func (s FeatureScope) String() string {
	if s.All() {
		return "all"
	}
	out := ""
	add := func(k, v string) {
		if v == "" {
			return
		}
		if out != "" {
			out += " "
		}
		out += k + "=" + v
	}
	add("actor", s.Actor)
	add("role", s.Role)
	add("tenant", string(s.Tenant))
	return out
}

// FeatureRule: the flag Feature (full name) is on at the level Scope.
type FeatureRule struct {
	Feature string       `json:"feature"`
	Scope   FeatureScope `json:"scope"`
}

// FeatureChange is a rollout change: switch on (On) at the level Scope or
// switch off — at the level Scope, and with Everywhere — at every level at
// once. By — who changed it (the OS user, an operator); At — when.
type FeatureChange struct {
	Feature    string       `json:"feature"`
	On         bool         `json:"on"`
	Scope      FeatureScope `json:"scope"`
	Everywhere bool         `json:"everywhere,omitempty"`
	By         string       `json:"by,omitempty"`
	At         time.Time    `json:"at"`
}

// FeatureStore keeps the feature flag state and the change journal. A
// change and its journal entry are atomic.
type FeatureStore interface {
	// FeatureRules returns the rollout rules of every flag.
	FeatureRules(ctx context.Context) ([]FeatureRule, error)
	// ChangeFeature applies a change and writes it to the journal: switching
	// on the same level again and switching off a level that is not on
	// change nothing but are journaled.
	ChangeFeature(ctx context.Context, c FeatureChange) error
	// FeatureLog returns the change journal of a flag ("" — of all) by time.
	FeatureLog(ctx context.Context, feature string) ([]FeatureChange, error)
}
