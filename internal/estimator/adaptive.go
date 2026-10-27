package estimator

import (
	"time"

	"moonwalk/internal/store"
)

// Adaptive is the queue simulation with each station's prep times scaled by its
// learned speed (a grill taking 10 min for an 8 min burger drifts toward 1.25).
type Adaptive struct{}

func (Adaptive) Name() string { return "adaptive" }

func (Adaptive) Estimate(now time.Time, r store.Restaurant, active []store.Order) map[int64]Estimate {
	return simulate(now, r, active, func(s store.Station) float64 {
		if s.Factor <= 0 {
			return 1 // nothing learned yet
		}
		return s.Factor
	})
}

const (
	learnRate = 0.2 // weight of one finished order
	minRatio  = 0.5 // one order counts as at most 2x faster...
	maxRatio  = 2.0 // ...or 2x slower
)

// Learn mixes one finished order (planned vs actual time) into a station's factor.
func Learn(factor float64, samples int, planned, actual time.Duration) (float64, int) {
	if planned <= 0 || actual <= 0 {
		return factor, samples // nothing useful to learn
	}
	if factor <= 0 {
		factor = 1
	}
	ratio := min(max(float64(actual)/float64(planned), minRatio), maxRatio)
	return (1-learnRate)*factor + learnRate*ratio, samples + 1
}

// Bottleneck returns the station with the most work in an order and its planned
// time there (3 burgers on a 2-slot grill = 2 rounds = 16 min).
func Bottleneck(r store.Restaurant, o store.Order) (stationID string, planned time.Duration) {
	units := map[string]int{}   // dishes per station
	longest := map[string]int{} // longest prep per station, in seconds
	for _, it := range o.Items {
		units[it.StationID] += it.Qty
		longest[it.StationID] = max(longest[it.StationID], it.PrepSeconds)
	}

	// config order keeps ties stable
	for _, s := range r.Stations {
		n := units[s.ID]
		if n == 0 {
			continue
		}
		c := max(s.Capacity, 1)
		rounds := (n + c - 1) / c
		if d := scale(rounds*longest[s.ID], 1); d > planned {
			stationID, planned = s.ID, d
		}
	}
	return stationID, planned
}
