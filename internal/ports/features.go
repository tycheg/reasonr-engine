package ports

import (
	"context"

	"github.com/tycheg/reasonr-engine/kind"
)

// FeatureHub broadcasts "the flag rollout has changed" between engine
// instances; the notification hub does it. Without it, instances learn
// about a change when they periodically reread the rules.
type FeatureHub interface {
	// PublishFeatures tells the instances that the rules have changed.
	PublishFeatures(ctx context.Context) error
	// OnFeatures subscribes fn to these messages, its own included.
	OnFeatures(fn func())
}

// Listening is a hub that needs a running listener to receive messages of
// other instances (LISTEN in PostgreSQL).
type Listening interface {
	Listen(ctx context.Context) error
}

// FeatureExecutor is an executor that knows the feature flag rollout: the
// app API evaluates layout conditions with it.
type FeatureExecutor interface {
	Features() kind.FeatureSource
}
