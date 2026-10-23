# Matching Engine

A limit order book matching engine written in Go, implementing price-time priority with partial fills and a concurrency-safe order book.

Standard library for everything except the PostgreSQL driver (`pgx`).

## Live Demo

A running instance is exposed over HTTPS:

```
https://liliang-matching.serveousercontent.com
```

```bash
BASE=https://liliang-matching.serveousercontent.com

curl $BASE/healthz
# {"status":"ok"}

curl -X POST $BASE/orders -d '{"side":"sell","price":100,"size":5}'
# {"trades":[]}

curl "$BASE/depth?n=5"
# {"bids":[],"asks":[{"price":100,"volume":5}]}

curl -X POST $BASE/orders -d '{"side":"buy","price":100,"size":3}'
# {"trades":[{"price":100,"size":3,"bid_order":2,"ask_order":1}]}
```

> The demo URL is a tunnel to a local process, so it is only reachable while that
> process is running — but the hostname is reserved, so it stays the same across
> reconnects and restarts. The commands above are the full API surface;
> everything can equally be reproduced locally with `go run ./cmd/api`.

The tunnel is maintained by [`scripts/tunnel.sh`](scripts/tunnel.sh), which survives
the frequent SSH drops by reconnecting and rewriting the current address to
`/tmp/matching-engine-tunnel.url`:

```bash
SUBDOMAIN=liliang-matching scripts/tunnel.sh   # reserved name, stable URL
cat /tmp/matching-engine-tunnel.url            # the address that is live right now
```

The reserved subdomain lives under `serveousercontent.com` and only works after
the host's SSH key is registered on the serveo console; without a reserved name
the hostname is random and changes on every reconnect.

## Features

- **Price-time priority** — best price wins; at the same price, the earliest order fills first (FIFO)
- **Partial fills** — an aggressive order sweeps multiple price levels until filled or no longer crossing
- **Dual-index order book** — an ordered slice for O(1) best-price lookup, plus a hash map for O(1) price-level lookup
- **Concurrency-safe** — all shared state is serialized through a mutex, verified under `go test -race`
- **Market depth snapshot** — query the top *n* levels of both sides at any time
- **Order cancellation** — cancel a resting order by ID in O(1) via an ID index; emptied price levels are removed
- **Order lifecycle queries** — completed and cancelled orders remain queryable, with `open` / `filled` / `cancelled` state plus filled and remaining size
- **HTTP API** — a thin `net/http` layer exposes submit / status / cancel / depth / trades as JSON, with request validation and typed error responses
- **Paginated trade history** — `/trades?limit=&offset=` returns a window plus the total count, with a hard cap of 1000 per page so one request cannot copy the entire log
- **Durable** — every order and cancellation is appended to a PostgreSQL event log before it is applied; on startup the log is replayed to rebuild the book and trade history
- **Full test suite** — 40 tests covering matching, priority, cancellation, status, book invariants, concurrency, recovery, paging, and the HTTP layer

## Quick Start

Requires Go 1.22+ (uses the built-in `min` function and method-based `ServeMux` patterns).

```bash
git clone https://github.com/liliang198909/matching-engine.git
cd matching-engine

go test ./...        # run the test suite
go test ./... -race  # verify concurrency safety

go run ./cmd/demo    # run the interactive demo
go run ./cmd/api     # start the HTTP server on :8080 (in-memory)
```

To persist orders, point `DATABASE_URL` at a PostgreSQL instance. The schema is created on startup, so an empty database is enough:

```bash
createdb matching
DATABASE_URL="postgres://$USER@127.0.0.1:5432/matching?sslmode=disable" \
  go run ./cmd/api
# 2026/10/23 15:40:42 restored 0 orders, rebuilt 0 trades
```

Without `DATABASE_URL` the service runs in memory only and logs a warning — handy for a quick look, wrong for anything real.

Demo output:

```
== 1. Sell orders submitted out of order — the book sorts by price ==
  Asks (ascending):
    100      x3
    101      x5
    102      x8
  Bids (descending):
    (empty)

== 2. A buy order takes the 100 level (price priority) ==
  filled 3 @ 100 (bid #4 vs ask #3)
  ...

== 3. A large buy order sweeps the 101 and 102 levels ==
  filled 5 @ 101 (bid #5 vs ask #1)
  filled 4 @ 102 (bid #5 vs ask #2)
  ...
```

## Deployment

The service ships as a multi-stage Docker image: the build stage compiles a static binary with `CGO_ENABLED=0`, the runtime stage keeps only that binary plus CA certificates and runs as an unprivileged user (UID 10001). No Go toolchain ends up in the final image.

`docker compose` also brings up PostgreSQL 16 with a named volume, and the API waits for its health check before starting:

```bash
docker compose up -d --build   # build and start api + db
docker compose ps              # both should report healthy
docker compose logs -f api     # tail logs
docker compose down            # stop (the pgdata volume keeps the order log)
```

The listen address resolves as `ADDR` → `PORT` → `:8080`, so the same image runs locally and on platforms that inject `$PORT` (Render, Koyeb, Heroku). A `/healthz` endpoint is exposed for load-balancer and container health probes.

Because the binary is fully static, it also runs without Docker at all — cross-compile and copy it to any Linux host:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w" -o matching-engine ./cmd/api
```

## Architecture

```
                       Orderbook
      ┌─────────────────────────────────────────┐
      │   Bids (price desc)  │  Asks (price asc) │
      │   ─────────────────  │  ─────────────────│
      │   102 ──┐            │            ┌── 103 │
      │   101 ──┼── map      │      map ──┼── 104 │
      │   100 ──┘            │            └── 105 │
      └──────────┬───────────┴───────────┬─────────┘
                 │                       │
                 ▼                       ▼
            Limit @ 102             Limit @ 103
            ┌──────────────┐        ┌──────────────┐
            │ Order A      │        │ Order C      │
            │ Order B      │        │ Order D      │
            └──────────────┘        └──────────────┘
              (FIFO order)            (FIFO order)
```

Three layers, each holding a pointer to the one below:

| Type | Responsibility |
|------|----------------|
| `Orderbook` | Holds both sides of the book, maintains price ordering |
| `Limit` | One price level; aggregates all orders at that price |
| `Order` | A single resting order (size, side, timestamp, back-reference) |

## Matching Flow

```
SubmitOrder(size, price, bid)
        │
        ▼
   ┌────────────────────────┐
   │ Match against the best │◄─── still crossing
   │ level on the opposite  │     and size remains
   │         side           │────────────┐
   └───────────┬────────────┘            │
               │                         │
               ▼                         │
        remaining size > 0 ? ────────────┘
          │              │
         yes             no
          │              │
          ▼              ▼
   rest on book        done
```

Aggressive orders take liquidity at the **passive (resting) price**, then the unfilled remainder is inserted into the book as a new resting order.

## API

```go
e := engine.NewEngine()

// Submit a limit order. Returns the trades generated by this call.
// Orders are auto-assigned incrementing IDs starting at 1.
trades := e.SubmitOrder(size, price, bid) // bid=true → buy, false → sell

// Top-n market depth snapshot for both sides.
bids, asks := e.Depth(5)

// Cancel a resting order by ID. Returns false if it is unknown or already filled.
cancelled := e.CancelOrder(42)

// Order lifecycle status, including orders that already left the book.
status, ok := e.Status(42) // ok=false if the order never existed

// Full trade history (copy).
all := e.Trades()

// One page of trade history, plus the total number of trades.
page, total := e.TradePage(0, 100) // offset, limit
```

```go
type Trade struct {
    Price    float64 // execution price
    Size     float64 // executed quantity
    BidOrder uint64  // buy-side order ID
    AskOrder uint64  // sell-side order ID
}
```

## HTTP API

The `api` package is a thin protocol layer over `engine`: it decodes JSON, validates, calls the engine, and encodes the result back. `engine` itself has no knowledge of HTTP.

| Method | Path | Purpose |
|--------|------|---------|
| `POST` | `/orders` | Submit a limit order; returns the trades it generated |
| `GET` | `/orders/{id}` | Order status: `open` / `filled` / `cancelled`, with filled and remaining size |
| `DELETE` | `/orders/{id}` | Cancel a resting order; `404` if it is unknown or already filled |
| `GET` | `/depth?n=5` | Top-*n* market depth for both sides |
| `GET` | `/trades?limit=&offset=` | Paginated trade history (default 100, max 1000), oldest first |
| `GET` | `/healthz` | Liveness probe for load balancers / container checks |

```bash
# Rest a sell order — nothing crosses, so trades is empty
curl -X POST localhost:8080/orders \
  -d '{"side":"sell","price":100,"size":5}'
# {"trades":[]}

# Rest another sell order
curl -X POST localhost:8080/orders \
  -d '{"side":"sell","price":102,"size":3}'

# Inspect the book
curl 'localhost:8080/depth?n=5'
# {"bids":[],"asks":[{"price":100,"volume":5},{"price":102,"volume":3}]}

# A large buy order sweeps both levels
curl -X POST localhost:8080/orders \
  -d '{"side":"buy","price":102,"size":8}'
# {"trades":[
#   {"price":100,"size":5,"bid_order":3,"ask_order":1},
#   {"price":102,"size":3,"bid_order":3,"ask_order":2}]}
```

Cancelling a resting order removes it from the book, and its price level disappears once the last order at that price is gone:

```bash
curl -X POST localhost:8080/orders -d '{"side":"sell","price":100,"size":5}'
# order 4 rests

curl -X DELETE localhost:8080/orders/4
# {"order_id":4,"cancelled":true}

curl -X DELETE localhost:8080/orders/4
# {"error":"order not found or already filled"}   HTTP 404
```

Every order stays queryable after it leaves the book, so a client can poll for the final outcome:

```bash
curl -X POST localhost:8080/orders -d '{"side":"sell","price":100,"size":5}'  # order 4
curl -X POST localhost:8080/orders -d '{"side":"buy","price":100,"size":2}'   # order 5

curl localhost:8080/orders/4
# {"order_id":4,"side":"sell","price":100,"size":5,"filled":2,"remaining":3,"state":"open"}

curl -X DELETE localhost:8080/orders/4
curl localhost:8080/orders/4
# {"order_id":4,"side":"sell","price":100,"size":5,"filled":2,"remaining":0,"state":"cancelled"}
```

Trade history is paged oldest-first, and each page reports the total so a client can compute the number of pages without guessing:

```bash
curl 'localhost:8080/trades?limit=1&offset=1'
# {"trades":[{"price":100,"size":2,"bid_order":6,"ask_order":4}],"total":2,"limit":1,"offset":1}

# An offset past the end is not an error — it is an empty page
curl 'localhost:8080/trades?offset=999'
# {"trades":[],"total":2,"limit":100,"offset":999}
```

Every rejection returns a JSON body with a single `error` field and an appropriate status code:

```bash
curl -X POST localhost:8080/orders -d '{"side":"hold","price":100,"size":1}'
# {"error":"side must be \"buy\" or \"sell\""}   HTTP 400

curl 'localhost:8080/depth?n=abc'
# {"error":"n must be a positive integer"}       HTTP 400

curl 'localhost:8080/trades?limit=5000'
# {"error":"limit must be an integer between 1 and 1000"}   HTTP 400

curl localhost:8080/orders
# Method Not Allowed                             HTTP 405
```

Design choices worth noting:

- **DTOs instead of direct serialization.** `engine.Trade` and `engine.PriceLevel` carry no `json` tags. Adding them would bind the domain model's field names to the wire protocol, so a later refactor of the engine would silently break API compatibility. The extra `tradeDTO` / `levelDTO` types keep the two layers independently evolvable.
- **No locks in the API layer.** `Engine` already serializes every mutation behind a mutex, so handlers can call it from concurrent requests without extra coordination.
- **No silent fallbacks on bad input.** `n=abc` returns 400 rather than quietly using the default. A client that sends garbage should learn about it immediately.
- **`ReadHeaderTimeout` is set explicitly.** The zero-value `http.Server` has no timeouts, letting a slow-header connection hold a goroutine indefinitely.

## Persistence

Every event — an order placed or an order cancelled — is appended to PostgreSQL *before* it is applied to the book. That event log is the only source of truth; trades are not stored separately.

```sql
CREATE TABLE events (
    id         BIGSERIAL PRIMARY KEY,
    kind       TEXT             NOT NULL CHECK (kind IN ('order', 'cancel')),
    side       TEXT             CHECK (side IN ('buy', 'sell')), -- order events only
    price      DOUBLE PRECISION CHECK (price > 0),               -- order events only
    size       DOUBLE PRECISION CHECK (size > 0),                -- order events only
    cancel_id  BIGINT,                                           -- cancel events only
    created_at TIMESTAMPTZ      NOT NULL DEFAULT now()
);
```

Both kinds share one sequence on purpose. Two separate tables would lose the true interleaving — order, cancel, order — and replay would then produce trades that never happened.

```
POST /orders                  DELETE /orders/{id}
    │                                  │
    ├─ INSERT order ────┬──────────────┴─ INSERT cancel
    │        │          │                    │
    │        └ id from sequence              └ cancel_id
    ▼                                        ▼
engine.SubmitWithID(...)             engine.CancelOrder(...)
```

On startup the log is replayed in event order through the same engine calls that serve live traffic:

```
events (id asc) ──► Replay ──► Orderbook + Trades
```

Why replay instead of a second trades table:

- **One source of truth.** Two tables can disagree, and a half-written pair is the kind of bug you find days later. Matching is deterministic, so the trade list is a pure function of the event log.
- **Crash consistency.** A `201` means the order is already durable, and a cancelled order stays cancelled across restarts. If the process dies after the insert but before matching, the next start applies it — no lost, duplicated, or resurrected orders.
- **ID continuity.** The database sequence assigns order IDs, so they keep increasing across restarts instead of resetting to 1 and colliding with history.

Known limits, stated on purpose: startup replay is O(n) and fine only while the log is small; the trade history and order-status map live in memory and grow with the log (a real deployment would page them from PostgreSQL); prices are `DOUBLE PRECISION` because the engine is `float64` (a production exchange would use fixed-point); and a single API replica owns the book, since matching is in-memory.

## Design Notes

**Why a slice *and* a map?**
A pure slice makes finding a price level O(n) on every insert — unacceptable at high order rates. A pure map has no ordering, so finding the best price would require a full scan on every match. The slice keeps prices sorted (best price is always `[0]`), while the map turns level lookup into O(1). Both structures hold pointers to the *same* `Limit` objects, so there is no data duplication.

**Why keep FIFO order on delete?**
The common trick is swap-with-last-then-truncate, which deletes in O(1). It is used here deliberately *not*: swapping moves a later order into the head position, silently breaking time priority. Since FIFO correctness is a core invariant of the exchange, the O(n) shift is the right trade — a level at a single price typically holds few orders, so the cost is bounded in practice.

**Why is the execution price the passive price?**
The resting order was in the book first and its limit price is the better one from the taker's perspective. Filling at the resting price is standard exchange behavior and is what guarantees the taker never pays worse than their limit.

**Why is the whole engine behind one mutex?**
Correctness first. A single lock keeps book invariants trivially safe under concurrent submissions and is verified with `-race`. Finer-grained schemes (per-side locks or a single-threaded matching loop with channel-fed commands) are the natural next step when throughput demands it — that is a performance change, not a correctness one.

**Why remove empty levels?**
A level that is fully consumed but left in the book would keep answering `BestBid`/`BestAsk` with zero volume, and stale map entries would leak memory over time. Levels are therefore removed as soon as they go empty.

## Tests

| Area | Coverage |
|------|----------|
| `Limit` | add, FIFO-preserving delete, deleting an unknown order, back-reference cleanup |
| `Orderbook` | price sorting on both sides, level reuse at the same price, best-price on empty book, level removal |
| Matching | no cross when prices do not overlap, full fill, partial fill (and remainder restocking) |
| Priority | price priority sweeping multiple levels, time priority at the same price |
| Both sides | sell orders crossing resting bids |
| Snapshot | depth truncation, `Depth(0)`, volume positivity |
| Concurrency | 100 goroutines submitting simultaneously, verified with `-race` |
| HTTP layer | order submission, JSON shape, input validation (malformed body, bad side, zero price, negative size), depth parameter validation, trade paging (window, total, out-of-range offset, limit cap), 405 on wrong method |

```bash
go test ./... -race
```

## Possible Extensions

- Market orders and cancel/replace
- Per-side locking or a single-threaded command loop
- Write-ahead log for crash recovery
- WebSocket market-data push (trades + depth)

## License

MIT
