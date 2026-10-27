package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure Go, no CGO
)

// Store wraps the app's single DB handle.
type Store struct {
	db *sql.DB
}

// Open creates the in-memory DB. Each ":memory:" connection is a separate DB,
// so the pool is pinned to one connection that never expires.
func Open(ctx context.Context) (*Store, error) {
	db, err := sql.Open("sqlite", "file::memory:?_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close is called once, on shutdown. With one connection, close any open
// *sql.Rows before the next query or it waits forever.
func (s *Store) Close() error { return s.db.Close() }

// Times are unix milliseconds.
const schema = `
CREATE TABLE restaurants (
	id       TEXT PRIMARY KEY,
	name     TEXT NOT NULL,
	strategy TEXT NOT NULL
);

CREATE TABLE stations (
	restaurant_id TEXT NOT NULL REFERENCES restaurants(id),
	id            TEXT NOT NULL,
	name          TEXT NOT NULL,
	capacity      INTEGER NOT NULL CHECK (capacity > 0),
	factor        REAL NOT NULL DEFAULT 1.0,
	samples       INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (restaurant_id, id)
);

CREATE TABLE menu_items (
	restaurant_id TEXT NOT NULL,
	id            TEXT NOT NULL,
	name          TEXT NOT NULL,
	station_id    TEXT NOT NULL,
	prep_seconds  INTEGER NOT NULL CHECK (prep_seconds > 0),
	PRIMARY KEY (restaurant_id, id),
	FOREIGN KEY (restaurant_id, station_id) REFERENCES stations(restaurant_id, id)
);

CREATE TABLE orders (
	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	restaurant_id      TEXT NOT NULL REFERENCES restaurants(id),
	table_no           TEXT NOT NULL,
	status             TEXT NOT NULL,
	created_at         INTEGER NOT NULL,
	started_at         INTEGER,
	ready_at           INTEGER,
	estimated_ready_at INTEGER,
	almost_ready       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX orders_by_status ON orders(restaurant_id, status);
CREATE INDEX orders_by_table  ON orders(restaurant_id, table_no);

CREATE TABLE order_items (
	order_id     INTEGER NOT NULL REFERENCES orders(id),
	menu_item_id TEXT NOT NULL,
	name         TEXT NOT NULL,
	station_id   TEXT NOT NULL,
	prep_seconds INTEGER NOT NULL,
	qty          INTEGER NOT NULL CHECK (qty > 0)
);
CREATE INDEX order_items_by_order ON order_items(order_id);

CREATE TABLE estimate_log (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	order_id          INTEGER NOT NULL,
	restaurant_id     TEXT NOT NULL,
	executed_at       INTEGER NOT NULL,
	algorithm         TEXT NOT NULL,
	trigger_name      TEXT NOT NULL,
	order_status      TEXT NOT NULL,
	elapsed_seconds   INTEGER NOT NULL,
	estimated_seconds INTEGER NOT NULL,
	remaining_seconds INTEGER NOT NULL,
	backlog_orders    INTEGER NOT NULL,
	compute_nanos    INTEGER NOT NULL
);
CREATE INDEX estimate_log_by_order ON estimate_log(order_id);
CREATE INDEX estimate_log_by_time  ON estimate_log(executed_at);
`

func toMillis(t time.Time) int64 { return t.UnixMilli() }

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// optMillis / optTime handle the nullable timestamp columns.
func optMillis(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixMilli(), Valid: true}
}

func optTime(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := fromMillis(n.Int64)
	return &t
}
