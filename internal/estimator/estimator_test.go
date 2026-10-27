package estimator

import (
	"math"
	"testing"
	"time"

	"moonwalk/internal/store"
)

// A tiny kitchen that's easy to do in your head:
// grill fits 2 at once, fryer fits 1. Burger 8 min, fries 4 min.
var (
	now     = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	kitchen = store.Restaurant{
		ID: "test",
		Stations: []store.Station{
			{ID: "grill", Capacity: 2, Factor: 1},
			{ID: "fryer", Capacity: 1, Factor: 1},
		},
	}
)

func burger(qty int) store.OrderItem {
	return store.OrderItem{MenuItemID: "burger", StationID: "grill", PrepSeconds: 480, Qty: qty}
}

func fries(qty int) store.OrderItem {
	return store.OrderItem{MenuItemID: "fries", StationID: "fryer", PrepSeconds: 240, Qty: qty}
}

// order is placed `ago` before now; startedAgo >= 0 means it's already cooking.
func order(id int64, ago time.Duration, startedAgo time.Duration, items ...store.OrderItem) store.Order {
	o := store.Order{ID: id, CreatedAt: now.Add(-ago), Items: items, Status: store.StatusPlaced}
	if startedAgo >= 0 {
		t := now.Add(-startedAgo)
		o.StartedAt, o.Status = &t, store.StatusCooking
	}
	return o
}

const waiting = -1 // not started yet

func wantReady(t *testing.T, got map[int64]Estimate, id int64, in time.Duration) {
	t.Helper()
	if want := now.Add(in); !got[id].ReadyAt.Equal(want) {
		t.Errorf("order %d: ready in %v, want %v", id, got[id].ReadyAt.Sub(now), in)
	}
}

func TestSimpleAddsEverythingUp(t *testing.T) {
	got := Simple{}.Estimate(now, kitchen, []store.Order{order(1, 0, waiting, burger(1), fries(1))})
	wantReady(t, got, 1, 12*time.Minute)
}

func TestQueueStationsWorkInParallel(t *testing.T) {
	got := Queue{}.Estimate(now, kitchen, []store.Order{order(1, 0, waiting, burger(1), fries(1))})
	wantReady(t, got, 1, 8*time.Minute) // fries finish while the burger is still on
}

func TestQueueRespectsCapacity(t *testing.T) {
	// 3 burgers, 2 grill slots: two now, the third after them.
	got := Queue{}.Estimate(now, kitchen, []store.Order{order(1, 0, waiting, burger(3))})
	wantReady(t, got, 1, 16*time.Minute)
}

func TestQueueBacklogIsFirstComeFirstServed(t *testing.T) {
	older := order(1, 2*time.Minute, waiting, burger(2)) // takes both grill slots
	newer := order(2, time.Minute, waiting, burger(1))   // has to wait for one

	// Pass them newest first to make sure the strategy sorts by itself.
	got := Queue{}.Estimate(now, kitchen, []store.Order{newer, older})
	wantReady(t, got, 1, 6*time.Minute)  // placed 2 min ago, so 2 of its 8 are gone
	wantReady(t, got, 2, 14*time.Minute) // starts when the older one frees a slot
}

func TestQueueCountdownMovesBeforeStart(t *testing.T) {
	// Nobody pressed Start yet, but time passes: the estimate must not slide forward.
	o := order(1, 0, waiting, burger(1))
	first := Queue{}.Estimate(now, kitchen, []store.Order{o})[1].ReadyAt
	later := Queue{}.Estimate(now.Add(3*time.Minute), kitchen, []store.Order{o})[1].ReadyAt
	if !first.Equal(later) {
		t.Errorf("ready time moved from %v to %v while waiting", first, later)
	}
}

func TestQueueCookingOrdersKeepTheirSlots(t *testing.T) {
	cooking := order(1, 10*time.Minute, 3*time.Minute, fries(1)) // 1 min left on the only fryer
	next := order(2, time.Minute, waiting, fries(1))

	got := Queue{}.Estimate(now, kitchen, []store.Order{cooking, next})
	wantReady(t, got, 1, 1*time.Minute)
	wantReady(t, got, 2, 5*time.Minute) // starts when the fryer frees up
}

func TestQueueOverdueGetsGraceMinute(t *testing.T) {
	late := order(1, 20*time.Minute, 10*time.Minute, burger(1)) // planned 8, it's been 10

	got := Queue{}.Estimate(now, kitchen, []store.Order{late})
	wantReady(t, got, 1, Grace)
	if !got[1].AlmostReady {
		t.Error("overdue order should be marked almost ready")
	}
}

func TestAdaptiveUsesLearnedSpeed(t *testing.T) {
	slowGrill := kitchen
	slowGrill.Stations = []store.Station{{ID: "grill", Capacity: 2, Factor: 1.5}, {ID: "fryer", Capacity: 1, Factor: 1}}

	got := Adaptive{}.Estimate(now, slowGrill, []store.Order{order(1, 0, waiting, burger(1))})
	wantReady(t, got, 1, 12*time.Minute) // 8 min * 1.5
}

func TestLearnMovesSlowlyAndIsCapped(t *testing.T) {
	plan := 8 * time.Minute

	f, n := Learn(1, 0, plan, 10*time.Minute) // 25% slower
	if !near(f, 1.05) || n != 1 {
		t.Errorf("one slow order: factor %.3f samples %d, want 1.05 and 1", f, n)
	}

	f, _ = Learn(1, 0, plan, 60*time.Minute) // a dropped plate, way off
	if !near(f, 1.2) {
		t.Errorf("outlier should be capped at 2x: factor %.3f, want 1.2", f)
	}

	f = 1
	for range 30 {
		f, _ = Learn(f, 0, plan, 10*time.Minute)
	}
	if !near(f, 1.25) {
		t.Errorf("after many 10 min burgers: factor %.3f, want ~1.25", f)
	}

	if f, n := Learn(1.1, 3, 0, time.Minute); f != 1.1 || n != 3 {
		t.Error("zero planned time should change nothing")
	}
}

func TestBottleneckPicksTheBusiestStation(t *testing.T) {
	id, planned := Bottleneck(kitchen, order(1, 0, waiting, burger(1), fries(3)))
	// fryer: 3 rounds * 4 = 12 min, grill: 8 min
	if id != "fryer" || planned != 12*time.Minute {
		t.Errorf("got %s %v, want fryer 12m", id, planned)
	}
}

func TestRegistry(t *testing.T) {
	for _, name := range Names() {
		s, ok := Get(name)
		if !ok || s.Name() != name {
			t.Errorf("strategy %q is registered under the wrong name", name)
		}
	}
	if _, ok := Get(Default); !ok {
		t.Error("default strategy must exist")
	}
	if _, ok := Get("nope"); ok {
		t.Error("unknown strategy should not be found")
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }
