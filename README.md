# MoonWalk – order countdown service

A Go + Gin backend that tells restaurant customers when each of their orders
will be ready, and shows it as a live countdown. It works out the wait from the
real kitchen: which station cooks each dish, how many it can cook at once, what's
already in the queue, and how fast each station has really been lately.

One server can serve many restaurants, each with its own stations, menu and strategy.

## Run

Needs Go 1.24+. No database to install; everything runs in memory.

```
go run ./cmd/server
```

Then open these side by side:

- Customer (table 7): http://localhost:8080/r/moonwalk/table/7
- Kitchen (staff):    http://localhost:8080/r/moonwalk/kitchen

Flags (all optional):

| Flag             | Default            | What it does                                   |
|------------------|--------------------|------------------------------------------------|
| `-port`          | `8080`             | HTTP port                                      |
| `-seed`          | `restaurants.json` | Restaurants to load at startup (`""` = none)   |
| `-tick`          | `30s`              | How often active orders are re-estimated       |
| `-log-retention` | `24h`              | How long estimate log rows are kept            |

Tests: `go test ./...`

## How the estimate works

**What the kitchen model knows**

| Factor    | Meaning                                                       |
|-----------|---------------------------------------------------------------|
| Prep time | How long a dish takes (burger = 8 min)                        |
| Routing   | Which station cooks it (burger → grill)                       |
| Capacity  | How many a station cooks at once (grill × 2)                  |
| Backlog   | What's already waiting or cooking                             |
| Parallel  | Stations work at the same time (burger + fries = 8 min, not 12) |
| History   | How long dishes really took, learned per station              |

**Strategies.** Each restaurant picks one; you can switch it any time.

- `simple`: adds up the prep times and ignores the queue. A baseline to compare against.
- `queue`: simulates the kitchen line. Every station has slots; cooking orders keep
  theirs; waiting orders go first come, first served, each dish taking the slot that
  frees up first. An order is ready when its slowest dish is done.
- `adaptive` (default, our own): `queue`, but each station's times are stretched by
  what it really did. When an order is marked Ready, its real cook time is compared
  with the plan on its busiest station, and that station's speed factor moves 20%
  toward it. One order can count for at most 2× slower or faster, so a dropped
  plate can't wreck it.

**When it runs:** after every change (order placed, started, ready, cancelled,
strategy switched) and every 30s, so dishes running late show "almost ready"
(+1 min) instead of a countdown stuck at 00:00.

## What gets stored

Every estimate run writes one row per active order into `estimate_log`:
time, algorithm, trigger (what caused it), order status, seconds elapsed,
estimated and remaining, backlog size, and how long the algorithm took.
Orders keep their own placed/started/ready timestamps. Log rows older than
`-log-retention` are deleted so memory stays bounded.

## API

All under `/api/restaurants/:rid`:

| Method & path                         | What it does                                    |
|---------------------------------------|-------------------------------------------------|
| `GET  /`                              | Restaurant, stations (with learned speed), menu |
| `PUT  /strategy`                      | `{"strategy":"queue"}`                          |
| `POST /orders`                        | `{"table":"7","items":[{"menu_item_id":"lunar-burger","qty":2}]}` |
| `GET  /orders`                        | Active orders (kitchen screen)                  |
| `GET  /tables/:table/orders`          | A table's orders from the last 3h (customer screen) |
| `POST /orders/:oid/start`             | placed → cooking                                |
| `POST /orders/:oid/ready`             | cooking → ready (also teaches `adaptive`)       |
| `POST /orders/:oid/cancel`            | placed/cooking → cancelled                      |
| `GET  /orders/:oid/estimate?strategy=simple` | What another strategy says now (logged, doesn't change the countdown) |
| `GET  /orders/:oid/log`               | Every estimate run for the order                |

Also: `POST /api/restaurants` (add a restaurant, same shape as `restaurants.json`),
`GET /api/strategies`, `GET /health`.

Errors are JSON `{"error": "..."}`: 400 bad input, 404 not found,
409 wrong step (e.g. Ready before Start) or duplicate restaurant, 503 timeout, 500 anything else.

## Adding a restaurant

Add it to `restaurants.json` or `POST` it to `/api/restaurants`:

```json
{
  "id": "mars-diner", "name": "Mars Diner", "strategy": "queue",
  "stations": [{ "id": "wok", "name": "Wok", "capacity": 3 }],
  "menu": [{ "id": "red-noodles", "name": "Red Planet Noodles", "station_id": "wok", "prep_seconds": 360 }]
}
```

`strategy` is optional (defaults to `adaptive`). The config is checked on load:
every dish needs a known station, capacity and prep time must be positive.

## Built to run for a long time

- **One DB connection for the whole life of the app.** An in-memory SQLite DB lives
  only as long as its connection, so the pool is pinned to one that never expires.
- **Every request has a 5s deadline and a 1 MB body cap.** The HTTP server also has
  read/write/idle timeouts.
- **A panic in a request or in the background worker is logged, not fatal.**
- **One change at a time per restaurant,** so estimates always see the real queue;
  restaurants don't block each other.
- **Old log rows are cleaned up** on every tick.
- **Ctrl+C shuts down cleanly:** finish in-flight requests, stop the worker, close the DB.

## Code map

```
cmd/server/          startup, flags, shutdown
internal/store/      types + SQLite (schema, queries)
internal/estimator/  the three strategies and the learning rule
internal/order/      order lifecycle, re-estimating, logging, background worker
internal/api/        Gin routes and error mapping
web/                 customer + kitchen pages (embedded in the binary)
```

Tests cover the critical parts: estimator math (`internal/estimator`), order
rules and logging (`internal/order`), and the HTTP flow end to end (`internal/api`).

## Known limits

- Data is in memory: a restart starts fresh (by design for this exercise).
- "Start" applies to a whole order, not to single dishes.
- Learning uses the whole order's cook time and credits it to its busiest station.
  Good enough to catch a slow station; not exact per dish.
- No login. The table number in the URL is the customer, like a QR code on the table.
