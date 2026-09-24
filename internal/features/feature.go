// Package features defines the feature-module contract. Each feature owns
// one automation domain, registers scheduled/queued tasks, and stays off
// unless enabled in config.
package features

import (
	"context"

	"hhauto/internal/queue"
	"hhauto/internal/scheduler"
)

// Feature is implemented by every automation module.
type Feature interface {
	Name() string
	Enabled() bool
	// RegisterTasks subscribes the feature to schedules and seeds initial
	// queued work. Called once per engine start.
	RegisterTasks(ctx context.Context, q queue.Queue, reg *scheduler.Registry) error
}

// Registry holds all constructed features.
type Registry struct {
	features []Feature
}

func (r *Registry) Add(f Feature) { r.features = append(r.features, f) }

func (r *Registry) All() []Feature { return r.features }

// Enabled returns only enabled features (engine drives these).
func (r *Registry) Enabled() []Feature {
	out := make([]Feature, 0, len(r.features))
	for _, f := range r.features {
		if f.Enabled() {
			out = append(out, f)
		}
	}
	return out
}
