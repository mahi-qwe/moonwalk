// Package store holds the data types and everything that talks to the database.
package store

import (
	"errors"
	"fmt"
	"time"
)

// Status is where an order is in the kitchen.
type Status string

const (
	StatusPlaced    Status = "placed"    // waiting in the queue
	StatusCooking   Status = "cooking"   // a chef started it
	StatusReady     Status = "ready"     // done, countdown is over
	StatusCancelled Status = "cancelled" // dropped, no longer takes kitchen time
)

// Active orders are the ones still using (or waiting for) kitchen time.
func (s Status) Active() bool { return s == StatusPlaced || s == StatusCooking }

// ErrNotFound is returned when a restaurant or order doesn't exist.
var ErrNotFound = errors.New("not found")

// ErrExists is returned when adding a restaurant whose id is taken.
var ErrExists = errors.New("already exists")

// Restaurant is one tenant: its own stations, menu and estimation strategy.
type Restaurant struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Strategy string     `json:"strategy"`
	Stations []Station  `json:"stations"`
	Menu     []MenuItem `json:"menu"`
}

// Station cooks one kind of dish. Capacity = dishes at once; Factor = learned speed (1.2 = 20% slower).
type Station struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Capacity int     `json:"capacity"`
	Factor   float64 `json:"factor"`
	Samples  int     `json:"samples"`
}

// MenuItem is a dish and how long it takes on its station.
type MenuItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	StationID   string `json:"station_id"`
	PrepSeconds int    `json:"prep_seconds"`
}

// Order is what one table asked for in one go.
type Order struct {
	ID               int64       `json:"id"`
	RestaurantID     string      `json:"restaurant_id"`
	Table            string      `json:"table"`
	Status           Status      `json:"status"`
	Items            []OrderItem `json:"items"`
	CreatedAt        time.Time   `json:"created_at"`
	StartedAt        *time.Time  `json:"started_at,omitempty"`
	ReadyAt          *time.Time  `json:"ready_at,omitempty"`
	EstimatedReadyAt *time.Time  `json:"estimated_ready_at,omitempty"`
	AlmostReady      bool        `json:"almost_ready"` // past its plan, showing the grace minute
}

// OrderItem snapshots the dish at order time, so menu edits don't affect it.
type OrderItem struct {
	MenuItemID  string `json:"menu_item_id"`
	Name        string `json:"name"`
	StationID   string `json:"station_id"`
	PrepSeconds int    `json:"prep_seconds"`
	Qty         int    `json:"qty"`
}

// EstimateLog is one estimate run for one order; Trigger says what caused it.
type EstimateLog struct {
	ID               int64     `json:"id"`
	OrderID          int64     `json:"order_id"`
	RestaurantID     string    `json:"restaurant_id"`
	ExecutedAt       time.Time `json:"executed_at"`
	Algorithm        string    `json:"algorithm"`
	Trigger          string    `json:"trigger"`
	OrderStatus      Status    `json:"order_status"`
	ElapsedSeconds   int       `json:"elapsed_seconds"`   // since the order was placed
	EstimatedSeconds int       `json:"estimated_seconds"` // placed -> estimated ready
	RemainingSeconds int       `json:"remaining_seconds"` // now -> estimated ready
	BacklogOrders    int       `json:"backlog_orders"`    // active orders at that moment
	ComputeNanos     int64     `json:"compute_nanos"`     // may read 0 on Windows (coarse clock)
}

// Validate checks the config. The strategy name is checked by the caller.
func (r *Restaurant) Validate() error {
	if r.ID == "" || r.Name == "" {
		return errors.New("restaurant needs an id and a name")
	}
	if len(r.Stations) == 0 || len(r.Menu) == 0 {
		return fmt.Errorf("restaurant %q needs at least one station and one menu item", r.ID)
	}
	stations := map[string]bool{}
	for _, s := range r.Stations {
		if s.ID == "" || s.Capacity <= 0 {
			return fmt.Errorf("station %q needs an id and capacity > 0", s.ID)
		}
		if stations[s.ID] {
			return fmt.Errorf("station %q is listed twice", s.ID)
		}
		stations[s.ID] = true
	}
	dishes := map[string]bool{}
	for _, m := range r.Menu {
		if m.ID == "" || m.PrepSeconds <= 0 {
			return fmt.Errorf("menu item %q needs an id and prep_seconds > 0", m.ID)
		}
		if !stations[m.StationID] {
			return fmt.Errorf("menu item %q uses unknown station %q", m.ID, m.StationID)
		}
		if dishes[m.ID] {
			return fmt.Errorf("menu item %q is listed twice", m.ID)
		}
		dishes[m.ID] = true
	}
	return nil
}
