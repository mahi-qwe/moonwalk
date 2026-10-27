// Package estimator predicts when each active order will be ready (simple, queue, adaptive).
package estimator

import (
	"sort"
	"time"

	"moonwalk/internal/store"
)

// Default is used when a restaurant doesn't pick a strategy.
const Default = "adaptive"

// Grace is the extra minute shown for overdue dishes, instead of a stuck 00:00.
const Grace = time.Minute

// Estimate is the prediction for one order.
type Estimate struct {
	ReadyAt     time.Time
	AlmostReady bool // something is overdue, ReadyAt includes the grace minute
}

// Strategy maps active (placed/cooking) orders to ready times without modifying them.
type Strategy interface {
	Name() string
	Estimate(now time.Time, r store.Restaurant, active []store.Order) map[int64]Estimate
}

var strategies = map[string]Strategy{
	"simple":   Simple{},
	"queue":    Queue{},
	"adaptive": Adaptive{},
}

// Get finds a strategy by name.
func Get(name string) (Strategy, bool) {
	s, ok := strategies[name]
	return s, ok
}

// Names lists the strategies, sorted.
func Names() []string {
	names := make([]string, 0, len(strategies))
	for n := range strategies {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// fifo sorts oldest first without touching the caller's slice.
func fifo(orders []store.Order) []store.Order {
	out := append([]store.Order(nil), orders...)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func scale(seconds int, factor float64) time.Duration {
	return time.Duration(float64(seconds) * factor * float64(time.Second))
}
