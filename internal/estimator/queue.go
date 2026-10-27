package estimator

import (
	"time"

	"moonwalk/internal/store"
)

// Queue simulates the kitchen line: each station has `capacity` slots, cooking
// orders keep theirs, waiting orders take the earliest free slot (oldest first),
// and stations run in parallel, so an order is ready when its slowest dish is.
// Waiting orders count from when they were placed, so the countdown keeps moving.
type Queue struct{}

func (Queue) Name() string { return "queue" }

func (Queue) Estimate(now time.Time, r store.Restaurant, active []store.Order) map[int64]Estimate {
	return simulate(now, r, active, func(store.Station) float64 { return 1 })
}

// simulate is shared by queue and adaptive; speed scales prep times (1.2 = 20% slower).
func simulate(now time.Time, r store.Restaurant, active []store.Order, speed func(store.Station) float64) map[int64]Estimate {
	type station struct {
		info  store.Station
		slots []time.Time // when each slot is free again; zero = free now
	}
	stations := make(map[string]*station, len(r.Stations))
	for _, s := range r.Stations {
		stations[s.ID] = &station{info: s, slots: make([]time.Time, s.Capacity)}
	}

	// Cooking orders go first; they already hold slots.
	var cooking, waiting []store.Order
	for _, o := range fifo(active) {
		if o.StartedAt != nil {
			cooking = append(cooking, o)
		} else {
			waiting = append(waiting, o)
		}
	}

	out := make(map[int64]Estimate, len(active))
	for _, o := range append(cooking, waiting...) {
		start := o.CreatedAt
		if o.StartedAt != nil {
			start = *o.StartedAt
		}

		var est Estimate
		for _, it := range o.Items {
			st := stations[it.StationID]
			if st == nil {
				// Can't happen after validation; avoid a panic anyway.
				st = &station{info: store.Station{ID: it.StationID, Capacity: 1}, slots: make([]time.Time, 1)}
				stations[it.StationID] = st
			}
			prep := scale(it.PrepSeconds, speed(st.info))

			for range it.Qty {
				i := earliest(st.slots)
				begin := later(st.slots[i], start)
				end := begin.Add(prep)
				if !end.After(now) {
					// Overdue: give it a grace minute.
					end = now.Add(Grace)
					est.AlmostReady = true
				}
				st.slots[i] = end
				est.ReadyAt = later(est.ReadyAt, end)
			}
		}
		out[o.ID] = est
	}
	return out
}

func earliest(slots []time.Time) int {
	best := 0
	for i := range slots {
		if slots[i].Before(slots[best]) {
			best = i
		}
	}
	return best
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
