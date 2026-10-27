package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// CreateOrder saves a new order with its dishes and returns its id.
func (s *Store) CreateOrder(ctx context.Context, o Order) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO orders (restaurant_id, table_no, status, created_at) VALUES (?, ?, ?, ?)`,
		o.RestaurantID, o.Table, o.Status, toMillis(o.CreatedAt))
	if err != nil {
		return 0, fmt.Errorf("add order: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, it := range o.Items {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO order_items (order_id, menu_item_id, name, station_id, prep_seconds, qty) VALUES (?, ?, ?, ?, ?, ?)`,
			id, it.MenuItemID, it.Name, it.StationID, it.PrepSeconds, it.Qty); err != nil {
			return 0, fmt.Errorf("add order item: %w", err)
		}
	}
	return id, tx.Commit()
}

// GetOrder returns one order of a restaurant, with its dishes.
func (s *Store) GetOrder(ctx context.Context, restaurantID string, id int64) (Order, error) {
	orders, err := s.orders(ctx, `restaurant_id = ? AND id = ?`, restaurantID, id)
	if err != nil {
		return Order{}, err
	}
	if len(orders) == 0 {
		return Order{}, ErrNotFound
	}
	return orders[0], nil
}

// ActiveOrders returns everything still waiting or cooking, oldest first.
func (s *Store) ActiveOrders(ctx context.Context, restaurantID string) ([]Order, error) {
	return s.orders(ctx, `restaurant_id = ? AND status IN ('placed', 'cooking')`, restaurantID)
}

// TableOrders returns a table's orders placed since a given time.
func (s *Store) TableOrders(ctx context.Context, restaurantID, table string, since time.Time) ([]Order, error) {
	return s.orders(ctx, `restaurant_id = ? AND table_no = ? AND created_at >= ?`,
		restaurantID, table, toMillis(since))
}

// SaveStatus stores a status change and its timestamps.
func (s *Store) SaveStatus(ctx context.Context, o Order) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE orders SET status = ?, started_at = ?, ready_at = ? WHERE id = ?`,
		o.Status, optMillis(o.StartedAt), optMillis(o.ReadyAt), o.ID)
	return err
}

// SaveEstimates updates estimates and appends log rows in one transaction.
func (s *Store) SaveEstimates(ctx context.Context, logs []EstimateLog, readyAt map[int64]time.Time, almost map[int64]bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for id, t := range readyAt {
		if _, err := tx.ExecContext(ctx,
			`UPDATE orders SET estimated_ready_at = ?, almost_ready = ? WHERE id = ?`,
			toMillis(t), almost[id], id); err != nil {
			return fmt.Errorf("save estimate: %w", err)
		}
	}
	if err := insertLogs(ctx, tx, logs); err != nil {
		return err
	}
	return tx.Commit()
}

// AddLogs appends log rows only (strategy comparisons).
func (s *Store) AddLogs(ctx context.Context, logs []EstimateLog) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := insertLogs(ctx, tx, logs); err != nil {
		return err
	}
	return tx.Commit()
}

func insertLogs(ctx context.Context, tx *sql.Tx, logs []EstimateLog) error {
	for _, l := range logs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO estimate_log (order_id, restaurant_id, executed_at, algorithm, trigger_name, order_status,
				elapsed_seconds, estimated_seconds, remaining_seconds, backlog_orders, compute_nanos)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			l.OrderID, l.RestaurantID, toMillis(l.ExecutedAt), l.Algorithm, l.Trigger, l.OrderStatus,
			l.ElapsedSeconds, l.EstimatedSeconds, l.RemainingSeconds, l.BacklogOrders, l.ComputeNanos); err != nil {
			return fmt.Errorf("add estimate log: %w", err)
		}
	}
	return nil
}

// OrderLog returns every estimate run for an order, oldest first.
func (s *Store) OrderLog(ctx context.Context, restaurantID string, orderID int64) ([]EstimateLog, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, order_id, restaurant_id, executed_at, algorithm, trigger_name, order_status,
			elapsed_seconds, estimated_seconds, remaining_seconds, backlog_orders, compute_nanos
		FROM estimate_log WHERE restaurant_id = ? AND order_id = ? ORDER BY id`, restaurantID, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []EstimateLog
	for rows.Next() {
		var l EstimateLog
		var at int64
		if err := rows.Scan(&l.ID, &l.OrderID, &l.RestaurantID, &at, &l.Algorithm, &l.Trigger, &l.OrderStatus,
			&l.ElapsedSeconds, &l.EstimatedSeconds, &l.RemainingSeconds, &l.BacklogOrders, &l.ComputeNanos); err != nil {
			return nil, err
		}
		l.ExecutedAt = fromMillis(at)
		out = append(out, l)
	}
	return out, rows.Err()
}

// DeleteLogsBefore drops old log rows to bound memory.
func (s *Store) DeleteLogsBefore(ctx context.Context, t time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM estimate_log WHERE executed_at < ?`, toMillis(t))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// orders loads matching orders, then their dishes in a second query (see Close).
func (s *Store) orders(ctx context.Context, where string, args ...any) ([]Order, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, restaurant_id, table_no, status, created_at, started_at, ready_at, estimated_ready_at, almost_ready
		FROM orders WHERE `+where+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	var orders []Order
	byID := map[int64]int{}
	for rows.Next() {
		var o Order
		var created int64
		var started, ready, estimated sql.NullInt64
		if err := rows.Scan(&o.ID, &o.RestaurantID, &o.Table, &o.Status, &created,
			&started, &ready, &estimated, &o.AlmostReady); err != nil {
			rows.Close()
			return nil, err
		}
		o.CreatedAt = fromMillis(created)
		o.StartedAt, o.ReadyAt, o.EstimatedReadyAt = optTime(started), optTime(ready), optTime(estimated)
		byID[o.ID] = len(orders)
		orders = append(orders, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(orders) == 0 {
		return nil, nil
	}

	items, err := s.db.QueryContext(ctx, `
		SELECT order_id, menu_item_id, name, station_id, prep_seconds, qty
		FROM order_items WHERE order_id IN (SELECT id FROM orders WHERE `+where+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer items.Close()
	for items.Next() {
		var orderID int64
		var it OrderItem
		if err := items.Scan(&orderID, &it.MenuItemID, &it.Name, &it.StationID, &it.PrepSeconds, &it.Qty); err != nil {
			return nil, err
		}
		if i, ok := byID[orderID]; ok {
			orders[i].Items = append(orders[i].Items, it)
		}
	}
	return orders, items.Err()
}
