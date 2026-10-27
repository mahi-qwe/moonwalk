package order

import (
	"context"
	"errors"
	"testing"
	"time"

	"moonwalk/internal/store"
)

// One grill that fits a single burger (8 min), so the queue is easy to follow.
func newService(t *testing.T) (*Service, *time.Time) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	svc := New(st)
	clock := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return clock }

	err = svc.AddRestaurant(ctx, store.Restaurant{
		ID: "r", Name: "Test", Strategy: "adaptive",
		Stations: []store.Station{{ID: "grill", Name: "Grill", Capacity: 1}},
		Menu:     []store.MenuItem{{ID: "burger", Name: "Burger", StationID: "grill", PrepSeconds: 480}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc, &clock
}

func place(t *testing.T, svc *Service, table string) View {
	t.Helper()
	v, err := svc.Place(context.Background(), "r", table, []Item{{MenuItemID: "burger", Qty: 1}})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestPlaceEstimatesAndLogs(t *testing.T) {
	svc, _ := newService(t)
	v := place(t, svc, "7")

	if v.Status != store.StatusPlaced || v.RemainingSeconds != 480 {
		t.Fatalf("got status %s, %ds left; want placed, 480s", v.Status, v.RemainingSeconds)
	}

	logs, err := svc.Log(context.Background(), "r", v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("want 1 log row, got %d", len(logs))
	}
	l := logs[0]
	if l.Algorithm != "adaptive" || l.Trigger != "placed" || l.EstimatedSeconds != 480 || l.OrderStatus != store.StatusPlaced {
		t.Errorf("unexpected log row: %+v", l)
	}
}

func TestCancelShortensTheWaitBehind(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	first := place(t, svc, "1")
	second := place(t, svc, "2")
	if second.RemainingSeconds != 960 {
		t.Fatalf("second burger waits for the first: want 960s, got %d", second.RemainingSeconds)
	}

	if _, err := svc.Cancel(ctx, "r", first.ID); err != nil {
		t.Fatal(err)
	}
	views, err := svc.TableOrders(ctx, "r", "2")
	if err != nil {
		t.Fatal(err)
	}
	if views[0].RemainingSeconds != 480 {
		t.Errorf("after cancel, want 480s left, got %d", views[0].RemainingSeconds)
	}
}

func TestStatusRules(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	id := place(t, svc, "1").ID

	if _, err := svc.Ready(ctx, "r", id); !errors.Is(err, ErrBadTransition) {
		t.Errorf("ready before start: want ErrBadTransition, got %v", err)
	}
	if _, err := svc.Start(ctx, "r", id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(ctx, "r", id); !errors.Is(err, ErrBadTransition) {
		t.Errorf("start twice: want ErrBadTransition, got %v", err)
	}
	if _, err := svc.Ready(ctx, "r", id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Cancel(ctx, "r", id); !errors.Is(err, ErrBadTransition) {
		t.Errorf("cancel after ready: want ErrBadTransition, got %v", err)
	}
	if _, err := svc.Start(ctx, "r", 999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown order: want ErrNotFound, got %v", err)
	}
}

func TestReadyTeachesTheStation(t *testing.T) {
	svc, clock := newService(t)
	ctx := context.Background()
	id := place(t, svc, "1").ID

	svc.Start(ctx, "r", id)
	*clock = clock.Add(10 * time.Minute) // menu says 8
	if _, err := svc.Ready(ctx, "r", id); err != nil {
		t.Fatal(err)
	}

	r, _ := svc.Restaurant(ctx, "r")
	if g := r.Stations[0]; g.Samples != 1 || g.Factor < 1.04 || g.Factor > 1.06 {
		t.Errorf("grill after a 10 min burger: factor %.3f samples %d, want ~1.05 and 1", g.Factor, g.Samples)
	}

	// The next burger is now planned a bit longer than the menu says.
	if next := place(t, svc, "2"); next.RemainingSeconds != 504 {
		t.Errorf("want 504s (480 * 1.05), got %d", next.RemainingSeconds)
	}
}

func TestBadInputIsRejected(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	var inv InvalidError

	cases := map[string][]Item{
		"no items":     nil,
		"unknown dish": {{MenuItemID: "pizza", Qty: 1}},
		"zero qty":     {{MenuItemID: "burger", Qty: 0}},
	}
	for name, items := range cases {
		if _, err := svc.Place(ctx, "r", "1", items); !errors.As(err, &inv) {
			t.Errorf("%s: want InvalidError, got %v", name, err)
		}
	}
	if _, err := svc.Place(ctx, "r", " ", []Item{{MenuItemID: "burger", Qty: 1}}); !errors.As(err, &inv) {
		t.Errorf("blank table: want InvalidError, got %v", err)
	}
	if err := svc.SetStrategy(ctx, "r", "magic"); !errors.As(err, &inv) {
		t.Errorf("unknown strategy: want InvalidError, got %v", err)
	}
}

func TestTickCleansOldLogs(t *testing.T) {
	svc, clock := newService(t)
	ctx := context.Background()
	id := place(t, svc, "1").ID

	*clock = clock.Add(25 * time.Hour)
	svc.tick(ctx, time.Second, 24*time.Hour)

	logs, _ := svc.Log(ctx, "r", id)
	if len(logs) != 1 || logs[0].Trigger != "tick" {
		t.Errorf("want only the fresh tick row left, got %+v", logs)
	}
}
