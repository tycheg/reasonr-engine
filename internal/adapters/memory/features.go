package memory

import (
	"context"
	"slices"

	"github.com/tycheg/reasonr-engine/internal/features"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/port"
)

var (
	_ port.FeatureStore = (*Store)(nil)
	_ ports.FeatureHub  = (*Hub)(nil)
)

// FeatureRules returns the feature flag rollout (port.FeatureStore).
func (s *Store) FeatureRules(context.Context) ([]port.FeatureRule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.features), nil
}

// ChangeFeature changes the rollout and journals the change (port.FeatureStore).
func (s *Store) ChangeFeature(_ context.Context, c port.FeatureChange) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.At = c.At.UTC()
	s.features = features.Apply(s.features, c)
	s.flagLog = append(s.flagLog, c)
	return nil
}

// FeatureLog returns the rollout change journal (port.FeatureStore).
func (s *Store) FeatureLog(_ context.Context, feature string) ([]port.FeatureChange, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []port.FeatureChange
	for _, c := range s.flagLog {
		if feature == "" || c.Feature == feature {
			out = append(out, c)
		}
	}
	return out, nil
}

// PublishFeatures tells the subscribers that the rollout changed (ports.FeatureHub).
func (h *Hub) PublishFeatures(context.Context) error {
	h.mu.Lock()
	fns := h.flags
	h.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
	return nil
}

// OnFeatures subscribes fn to rollout changes (ports.FeatureHub).
func (h *Hub) OnFeatures(fn func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.flags = append(h.flags, fn)
}
