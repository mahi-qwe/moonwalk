package estimator

import (
	"time"

	"moonwalk/internal/store"
)

// Simple adds up prep times as if nothing else were cooking. Baseline only.
type Simple struct{}

func (Simple) Name() string { return "simple" }

func (Simple) Estimate(now time.Time, _ store.Restaurant, active []store.Order) map[int64]Estimate {
	out := make(map[int64]Estimate, len(active))
	for _, o := range active {
		var total time.Duration
		for _, it := range o.Items {
			total += scale(it.PrepSeconds*it.Qty, 1)
		}
		start := o.CreatedAt
		if o.StartedAt != nil {
			start = *o.StartedAt
		}

		est := Estimate{ReadyAt: start.Add(total)}
		if !est.ReadyAt.After(now) {
			est = Estimate{ReadyAt: now.Add(Grace), AlmostReady: true}
		}
		out[o.ID] = est
	}
	return out
}
