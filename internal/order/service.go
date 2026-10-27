// Package order handles the order lifecycle and re-estimates after every change.
package order

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"moonwalk/internal/estimator"
	"moonwalk/internal/store"
)

// ErrBadTransition is a status change that isn't allowed (e.g. Ready before Start).
var ErrBadTransition = errors.New("order can't change to that status from its current one")

// InvalidError is bad caller input (HTTP 400).
type InvalidError struct{ Msg string }

func (e InvalidError) Error() string { return e.Msg }

func invalid(format string, args ...any) error { return InvalidError{fmt.Sprintf(format, args...)} }

// Item is one line of a new order.
type Item struct {
	MenuItemID string `json:"menu_item_id"`
	Qty        int    `json:"qty"`
}

// View is an order plus its seconds left right now.
type View struct {
	store.Order
	RemainingSeconds int `json:"remaining_seconds"`
}

// tableWindow hides earlier guests' orders from a table's page.
const tableWindow = 3 * time.Hour

const maxQty = 20 // per line; more is likely a typo

// Service is safe for concurrent use; changes are serialized per restaurant.
type Service struct {
	store *store.Store
	now   func() time.Time // fake clock in tests
	locks sync.Map         // restaurant id -> *sync.Mutex
}

func New(st *store.Store) *Service {
	return &Service{store: st, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) lock(restaurantID string) (unlock func()) {
	m, _ := s.locks.LoadOrStore(restaurantID, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// AddRestaurant onboards a restaurant (default strategy if none).
func (s *Service) AddRestaurant(ctx context.Context, r store.Restaurant) error {
	if r.Strategy == "" {
		r.Strategy = estimator.Default
	}
	if _, ok := estimator.Get(r.Strategy); !ok {
		return invalid("unknown strategy %q, pick one of %v", r.Strategy, estimator.Names())
	}
	if err := r.Validate(); err != nil {
		return InvalidError{err.Error()}
	}
	return s.store.AddRestaurant(ctx, r)
}

// Restaurant returns a restaurant with its menu and stations.
func (s *Service) Restaurant(ctx context.Context, restaurantID string) (store.Restaurant, error) {
	return s.store.GetRestaurant(ctx, restaurantID)
}

// SetStrategy switches the algorithm and re-estimates.
func (s *Service) SetStrategy(ctx context.Context, restaurantID, name string) error {
	if _, ok := estimator.Get(name); !ok {
		return invalid("unknown strategy %q, pick one of %v", name, estimator.Names())
	}
	defer s.lock(restaurantID)()
	if err := s.store.SetStrategy(ctx, restaurantID, name); err != nil {
		return err
	}
	return s.recompute(ctx, restaurantID, "strategy")
}

// Place adds an order and re-estimates the kitchen.
func (s *Service) Place(ctx context.Context, restaurantID, table string, items []Item) (View, error) {
	table = strings.TrimSpace(table)
	if table == "" {
		return View{}, invalid("table is required")
	}
	if len(items) == 0 {
		return View{}, invalid("order needs at least one item")
	}

	r, err := s.store.GetRestaurant(ctx, restaurantID)
	if err != nil {
		return View{}, err
	}
	menu := make(map[string]store.MenuItem, len(r.Menu))
	for _, m := range r.Menu {
		menu[m.ID] = m
	}

	o := store.Order{RestaurantID: restaurantID, Table: table, Status: store.StatusPlaced, CreatedAt: s.now()}
	for _, it := range items {
		m, ok := menu[it.MenuItemID]
		if !ok {
			return View{}, invalid("unknown menu item %q", it.MenuItemID)
		}
		if it.Qty < 1 || it.Qty > maxQty {
			return View{}, invalid("qty for %q must be between 1 and %d", it.MenuItemID, maxQty)
		}
		o.Items = append(o.Items, store.OrderItem{
			MenuItemID: m.ID, Name: m.Name, StationID: m.StationID, PrepSeconds: m.PrepSeconds, Qty: it.Qty,
		})
	}

	defer s.lock(restaurantID)()
	id, err := s.store.CreateOrder(ctx, o)
	if err != nil {
		return View{}, err
	}
	if err := s.recompute(ctx, restaurantID, "placed"); err != nil {
		return View{}, err
	}
	return s.view(ctx, restaurantID, id)
}

// Start means a chef picked the order up.
func (s *Service) Start(ctx context.Context, restaurantID string, id int64) (View, error) {
	return s.move(ctx, restaurantID, id, "started", func(o *store.Order, now time.Time) bool {
		if o.Status != store.StatusPlaced {
			return false
		}
		o.Status, o.StartedAt = store.StatusCooking, &now
		return true
	})
}

// Ready finishes an order; its real cook time trains the adaptive strategy.
func (s *Service) Ready(ctx context.Context, restaurantID string, id int64) (View, error) {
	return s.move(ctx, restaurantID, id, "ready", func(o *store.Order, now time.Time) bool {
		if o.Status != store.StatusCooking {
			return false
		}
		o.Status, o.ReadyAt = store.StatusReady, &now
		return true
	})
}

// Cancel frees the kitchen time an order was holding.
func (s *Service) Cancel(ctx context.Context, restaurantID string, id int64) (View, error) {
	return s.move(ctx, restaurantID, id, "cancelled", func(o *store.Order, _ time.Time) bool {
		if !o.Status.Active() {
			return false
		}
		o.Status = store.StatusCancelled
		return true
	})
}

// move applies a status change if allowed, then re-estimates.
func (s *Service) move(ctx context.Context, restaurantID string, id int64, trigger string,
	change func(o *store.Order, now time.Time) bool) (View, error) {

	defer s.lock(restaurantID)()
	o, err := s.store.GetOrder(ctx, restaurantID, id)
	if err != nil {
		return View{}, err
	}
	if !change(&o, s.now()) {
		return View{}, fmt.Errorf("%w (order is %s)", ErrBadTransition, o.Status)
	}
	if err := s.store.SaveStatus(ctx, o); err != nil {
		return View{}, err
	}
	if o.Status == store.StatusReady {
		s.learn(ctx, o)
	}
	if err := s.recompute(ctx, restaurantID, trigger); err != nil {
		return View{}, err
	}
	return s.view(ctx, restaurantID, id)
}

// learn updates the bottleneck station's speed. Failures are only logged.
func (s *Service) learn(ctx context.Context, o store.Order) {
	if o.StartedAt == nil || o.ReadyAt == nil {
		return
	}
	r, err := s.store.GetRestaurant(ctx, o.RestaurantID)
	if err != nil {
		slog.Warn("learn: load restaurant", "restaurant", o.RestaurantID, "err", err)
		return
	}
	stationID, planned := estimator.Bottleneck(r, o)
	for _, st := range r.Stations {
		if st.ID != stationID {
			continue
		}
		factor, samples := estimator.Learn(st.Factor, st.Samples, planned, o.ReadyAt.Sub(*o.StartedAt))
		if err := s.store.SetStationFactor(ctx, r.ID, st.ID, factor, samples); err != nil {
			slog.Warn("learn: save factor", "restaurant", r.ID, "station", st.ID, "err", err)
		}
	}
}

// recompute re-estimates and logs every active order. Caller holds the lock.
func (s *Service) recompute(ctx context.Context, restaurantID, trigger string) error {
	r, err := s.store.GetRestaurant(ctx, restaurantID)
	if err != nil {
		return err
	}
	active, err := s.store.ActiveOrders(ctx, restaurantID)
	if err != nil || len(active) == 0 {
		return err
	}
	strategy, ok := estimator.Get(r.Strategy)
	if !ok {
		strategy, _ = estimator.Get(estimator.Default)
	}

	now := s.now()
	began := time.Now()
	estimates := strategy.Estimate(now, r, active)
	took := time.Since(began).Nanoseconds()

	readyAt := make(map[int64]time.Time, len(active))
	almost := make(map[int64]bool, len(active))
	logs := make([]store.EstimateLog, 0, len(active))
	for _, o := range active {
		e := estimates[o.ID]
		readyAt[o.ID], almost[o.ID] = e.ReadyAt, e.AlmostReady
		logs = append(logs, logRow(o, e, strategy.Name(), trigger, now, len(active), took))
	}
	return s.store.SaveEstimates(ctx, logs, readyAt, almost)
}

func logRow(o store.Order, e estimator.Estimate, algorithm, trigger string, now time.Time, backlog int, nanos int64) store.EstimateLog {
	return store.EstimateLog{
		OrderID:          o.ID,
		RestaurantID:     o.RestaurantID,
		ExecutedAt:       now,
		Algorithm:        algorithm,
		Trigger:          trigger,
		OrderStatus:      o.Status,
		ElapsedSeconds:   int(now.Sub(o.CreatedAt).Seconds()),
		EstimatedSeconds: int(e.ReadyAt.Sub(o.CreatedAt).Seconds()),
		RemainingSeconds: max(int(e.ReadyAt.Sub(now).Seconds()), 0),
		BacklogOrders:    backlog,
		ComputeNanos:     nanos,
	}
}

// Compare runs any strategy on one order without changing the countdown (logged).
func (s *Service) Compare(ctx context.Context, restaurantID string, id int64, name string) (store.EstimateLog, error) {
	strategy, ok := estimator.Get(name)
	if !ok {
		return store.EstimateLog{}, invalid("unknown strategy %q, pick one of %v", name, estimator.Names())
	}

	defer s.lock(restaurantID)()
	r, err := s.store.GetRestaurant(ctx, restaurantID)
	if err != nil {
		return store.EstimateLog{}, err
	}
	o, err := s.store.GetOrder(ctx, restaurantID, id)
	if err != nil {
		return store.EstimateLog{}, err
	}
	if !o.Status.Active() {
		return store.EstimateLog{}, fmt.Errorf("%w: only waiting or cooking orders can be estimated", ErrBadTransition)
	}
	active, err := s.store.ActiveOrders(ctx, restaurantID)
	if err != nil {
		return store.EstimateLog{}, err
	}

	now := s.now()
	began := time.Now()
	e := strategy.Estimate(now, r, active)[id]
	row := logRow(o, e, strategy.Name(), "compare", now, len(active), time.Since(began).Nanoseconds())
	return row, s.store.AddLogs(ctx, []store.EstimateLog{row})
}

// TableOrders is the customer screen: the table's recent orders.
func (s *Service) TableOrders(ctx context.Context, restaurantID, table string) ([]View, error) {
	if _, err := s.store.GetRestaurant(ctx, restaurantID); err != nil {
		return nil, err
	}
	orders, err := s.store.TableOrders(ctx, restaurantID, table, s.now().Add(-tableWindow))
	return s.views(orders), err
}

// KitchenOrders is the staff screen: everything still to do.
func (s *Service) KitchenOrders(ctx context.Context, restaurantID string) ([]View, error) {
	if _, err := s.store.GetRestaurant(ctx, restaurantID); err != nil {
		return nil, err
	}
	orders, err := s.store.ActiveOrders(ctx, restaurantID)
	return s.views(orders), err
}

// Log returns every estimate run for one order.
func (s *Service) Log(ctx context.Context, restaurantID string, id int64) ([]store.EstimateLog, error) {
	if _, err := s.store.GetOrder(ctx, restaurantID, id); err != nil {
		return nil, err
	}
	return s.store.OrderLog(ctx, restaurantID, id)
}

func (s *Service) view(ctx context.Context, restaurantID string, id int64) (View, error) {
	o, err := s.store.GetOrder(ctx, restaurantID, id)
	if err != nil {
		return View{}, err
	}
	return s.views([]store.Order{o})[0], nil
}

func (s *Service) views(orders []store.Order) []View {
	now := s.now()
	out := make([]View, 0, len(orders))
	for _, o := range orders {
		v := View{Order: o}
		if o.Status.Active() && o.EstimatedReadyAt != nil {
			v.RemainingSeconds = max(int(o.EstimatedReadyAt.Sub(now).Seconds()), 0)
		}
		out = append(out, v)
	}
	return out
}

// Run re-estimates active orders and cleans old logs every tick until ctx ends.
func (s *Service) Run(ctx context.Context, tick, retention time.Duration) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tick(ctx, tick, retention)
		}
	}
}

// tick is one round; errors and panics are logged so the worker keeps going.
func (s *Service) tick(ctx context.Context, timeout, retention time.Duration) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("background tick panicked", "panic", p)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ids, err := s.store.RestaurantIDs(ctx)
	if err != nil {
		slog.Error("tick: list restaurants", "err", err)
		return
	}
	for _, id := range ids {
		unlock := s.lock(id)
		err := s.recompute(ctx, id, "tick")
		unlock()
		if err != nil {
			slog.Error("tick: recompute", "restaurant", id, "err", err)
		}
	}

	if n, err := s.store.DeleteLogsBefore(ctx, s.now().Add(-retention)); err != nil {
		slog.Error("tick: clean estimate log", "err", err)
	} else if n > 0 {
		slog.Info("cleaned old estimate log rows", "rows", n)
	}
}
