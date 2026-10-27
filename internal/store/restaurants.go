package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// LoadSeed reads the restaurants a server starts with from a JSON file.
func LoadSeed(path string) ([]Restaurant, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read seed: %w", err)
	}
	var rs []Restaurant
	if err := json.Unmarshal(data, &rs); err != nil {
		return nil, fmt.Errorf("parse seed %s: %w", path, err)
	}
	return rs, nil
}

// AddRestaurant saves a restaurant with its stations and menu, all or nothing.
func (s *Store) AddRestaurant(ctx context.Context, r Restaurant) error {
	if err := r.Validate(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit

	var taken bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM restaurants WHERE id = ?)`, r.ID).Scan(&taken); err != nil {
		return err
	}
	if taken {
		return fmt.Errorf("restaurant %q: %w", r.ID, ErrExists)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO restaurants (id, name, strategy) VALUES (?, ?, ?)`,
		r.ID, r.Name, r.Strategy); err != nil {
		return fmt.Errorf("add restaurant %q: %w", r.ID, err)
	}
	for _, st := range r.Stations {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO stations (restaurant_id, id, name, capacity) VALUES (?, ?, ?, ?)`,
			r.ID, st.ID, st.Name, st.Capacity); err != nil {
			return fmt.Errorf("add station %q: %w", st.ID, err)
		}
	}
	for _, m := range r.Menu {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO menu_items (restaurant_id, id, name, station_id, prep_seconds) VALUES (?, ?, ?, ?, ?)`,
			r.ID, m.ID, m.Name, m.StationID, m.PrepSeconds); err != nil {
			return fmt.Errorf("add menu item %q: %w", m.ID, err)
		}
	}
	return tx.Commit()
}

// GetRestaurant returns a restaurant with its stations and menu.
func (s *Store) GetRestaurant(ctx context.Context, id string) (Restaurant, error) {
	r := Restaurant{ID: id}
	err := s.db.QueryRowContext(ctx,
		`SELECT name, strategy FROM restaurants WHERE id = ?`, id).Scan(&r.Name, &r.Strategy)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}

	if r.Stations, err = s.stations(ctx, id); err != nil {
		return r, err
	}
	r.Menu, err = s.menu(ctx, id)
	return r, err
}

func (s *Store) stations(ctx context.Context, restaurantID string) ([]Station, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, capacity, factor, samples FROM stations WHERE restaurant_id = ? ORDER BY id`,
		restaurantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Station
	for rows.Next() {
		var st Station
		if err := rows.Scan(&st.ID, &st.Name, &st.Capacity, &st.Factor, &st.Samples); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *Store) menu(ctx context.Context, restaurantID string) ([]MenuItem, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, station_id, prep_seconds FROM menu_items WHERE restaurant_id = ? ORDER BY name`,
		restaurantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MenuItem
	for rows.Next() {
		var m MenuItem
		if err := rows.Scan(&m.ID, &m.Name, &m.StationID, &m.PrepSeconds); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RestaurantIDs lists all restaurant ids.
func (s *Store) RestaurantIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM restaurants ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SetStrategy switches which algorithm a restaurant uses from now on.
func (s *Store) SetStrategy(ctx context.Context, restaurantID, strategy string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE restaurants SET strategy = ? WHERE id = ?`, strategy, restaurantID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetStationFactor saves a station's learned speed.
func (s *Store) SetStationFactor(ctx context.Context, restaurantID, stationID string, factor float64, samples int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE stations SET factor = ?, samples = ? WHERE restaurant_id = ? AND id = ?`,
		factor, samples, restaurantID, stationID)
	return err
}
