# Matchbook

Matchbook is a small backend for a limit order book. It accepts limit and market
orders, matches them by price-time priority, reports trades, serves book
snapshots, and records every command in an append-only event log so the whole
market can be rebuilt and checked against the live state.

The initial release intentionally supports a compact public contract:

- orders are either `limit` or `market`, and every price and quantity is an
  integer in ticks, so no floating point value ever reaches the matching engine;
- a trade always executes at the resting (maker) side's price;
- price-time priority: the best price wins, and orders at the same price are
  filled oldest first;
- an incoming limit order fills as much as the book allows and rests with its
  remainder; a market order never rests and is rejected when the book cannot
  fill it completely;
- the best bid is always strictly below the best ask, because any executable
  pair would have been consumed on arrival;
- every order keeps `quantity == filled + remaining` at all observable times;
- every command (and every fill it causes) is appended to a JSONL event log, and
  replay rebuilds the book from that log only;
- duplicate commands with the same idempotency key return the original result.

## Requirements

- Go 1.21 or newer
- no third-party dependencies

## Run the service

```bash
go build -o matchbook .
./matchbook --host 127.0.0.1 --port 8080 --data ./data --symbols ACME:2
```

The process prints `Matchbook listening on http://127.0.0.1:8080` after it has
bound the port. It then serves the HTTP API until `SIGINT` or `SIGTERM`.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--host` | `127.0.0.1` | interface to bind |
| `--port` | `8080` | TCP port to bind |
| `--data` | `data` | directory holding `events.jsonl` and `idempotency.json` |
| `--symbols` | *(empty)* | comma separated `SYMBOL:PRICE_SCALE` list registered at startup; a symbol already in the log is skipped |

## Data model

Prices and quantities are integers everywhere in the API. `price_scale` fixes
the decimal point for display only:

```text
price (ticks) = 1005, price_scale = 2  ->  "10.05"
quantity      = 40                     ->  40 base units
```

Prices are never floats, so a price of `10.10` is stored as the integer `1010`
and repeated arithmetic on it is exact.

### Order

| Field | Type | Notes |
| --- | --- | --- |
| `id` | string | unique, at most 100 characters, supplied by the client or generated as `ord-<n>` |
| `symbol` | string | must be registered |
| `side` | `buy` \| `sell` | |
| `type` | `limit` \| `market` | |
| `limit_price` | integer ticks | required for `limit`, must be omitted for `market` |
| `quantity` | integer | `1 .. 1000000000` |

An order is created with status `pending`; the recorded events immediately move
it to `open`, `filled`, `cancelled`, or `rejected`.

### Order status machine

```text
pending --(order_accepted)--> open
open --(order_updated, remaining > 0)--> open
open --(order_filled, remaining == 0)--> filled      [terminal]
open --(order_cancelled)--> cancelled                [terminal]
pending --(order_rejected, no liquidity)--> rejected [terminal, market only]
```

A partially filled order that is later cancelled becomes `cancelled` with
`filled_quantity > 0`; a partially filled order that fills its remainder becomes
`filled`. `filled`, `cancelled`, and `rejected` are terminal: cancelling them is
a `conflict`, and they never rest in the book again.

### Trade

A trade pairs one resting (maker) order with one incoming (taker) order. Its
price is always the maker's price, and its quantity is
`min(taker remaining, maker remaining)` at that moment.

## HTTP API

All request and response bodies are JSON. Unknown fields are rejected, and
bodies larger than 1 MiB are rejected. `POST` requests that change state accept
an optional `Idempotency-Key` header: repeating a key with the same operation
returns the first response and changes nothing.

### Health

```http
GET /health
```

```json
{"status":"ok"}
```

### Register a symbol

```http
POST /symbols

{"symbol":"ACME","price_scale":2}
```

Returns HTTP 201:

```json
{"name":"ACME","price_scale":2}
```

`symbol` is 1–100 characters from `A-Z a-z 0-9 - _ . /`, and `price_scale` is an
integer between 0 and 8. Registering the same symbol twice is a `conflict`.

```http
GET /symbols
```

```json
{"symbols":[{"name":"ACME","price_scale":2}]}
```

Symbols are listed in lexicographic order.

### Place an order

```http
POST /orders
Idempotency-Key: order-request-1

{
  "id": "ask-1",
  "symbol": "ACME",
  "side": "sell",
  "type": "limit",
  "quantity": 40,
  "limit_price": 1010
}
```

Returns HTTP 201 with the order:

```json
{
  "id": "ask-1",
  "symbol": "ACME",
  "side": "sell",
  "type": "limit",
  "status": "open",
  "price_scale": 2,
  "limit_price": "10.10",
  "limit_price_ticks": 1010,
  "quantity": 40,
  "filled_quantity": 0,
  "remaining_quantity": 40,
  "trade_ids": [],
  "sequence": 2,
  "created_at": 1700000000000,
  "updated_at": 1700000000000,
  "last_updated_by": "created"
}
```

`id` may be omitted, in which case the service assigns `ord-<event sequence>`.
Supplying an id that already exists is a `conflict`.

Placing an order that crosses the book matches immediately and returns the
resulting state, for example a taker that consumes two price levels:

```json
{
  "id": "bid-1",
  "symbol": "ACME",
  "side": "buy",
  "type": "limit",
  "status": "filled",
  "price_scale": 2,
  "limit_price": "10.10",
  "limit_price_ticks": 1010,
  "quantity": 50,
  "filled_quantity": 50,
  "remaining_quantity": 0,
  "trade_ids": ["trade-1", "trade-2"],
  "sequence": 5,
  "created_at": 1700000000000,
  "updated_at": 1700000000000,
  "last_updated_by": "created"
}
```

Matching rules:

- a `buy` limit order matches asks priced at or below its limit; a `sell` limit
  order matches bids priced at or above its limit;
- the best price level trades first, and inside a level the oldest order trades
  first;
- each fill is capped by the remaining quantity of both orders;
- when the book can no longer fill a limit order, its remainder rests at its own
  limit price;
- a `market` order matches every level it can reach and never rests. If any
  quantity is left over, the whole order ends as `rejected` with
  `remaining_quantity > 0` and `status: "rejected"`; the quantity that already
  traded stays traded and remains visible through `/trades`.

A rejected market order's response:

```json
{
  "id": "mkt-2",
  "symbol": "ACME",
  "side": "buy",
  "type": "market",
  "status": "rejected",
  "price_scale": 0,
  "limit_price": null,
  "limit_price_ticks": null,
  "quantity": 5,
  "filled_quantity": 0,
  "remaining_quantity": 5,
  "trade_ids": [],
  "sequence": 12,
  "created_at": 1700000000000,
  "updated_at": 1700000000000,
  "last_updated_by": "created"
}
```

### Cancel an order

```http
DELETE /orders/ask-1
Idempotency-Key: cancel-1
```

Returns HTTP 200 with the cancelled order (`status: "cancelled"`,
`last_updated_by: "cancelled"`). The order leaves the book immediately, and any
quantity it had already traded stays traded.

Cancelling an order that is already `filled`, `cancelled`, or `rejected` is a
`conflict`; cancelling an unknown order is `not_found`.

### Read the book

```http
GET /book?symbol=ACME&depth=10
```

`depth` is optional, defaults to `10`, and must be between `1` and `100`.

```json
{
  "symbol": "ACME",
  "price_scale": 2,
  "sequence": 11,
  "depth": 10,
  "best_bid": null,
  "best_ask": "10.10",
  "last_trade_price": "10.10",
  "bid_level_count": 0,
  "ask_level_count": 1,
  "open_order_count": 2,
  "crossed": false,
  "bids": [],
  "asks": [{"price": 1010, "quantity": 70, "order_count": 2}]
}
```

`bids` are ordered best (highest) first and `asks` best (lowest) first; each
entry aggregates the resting quantity and order count of one price level.
`bid_level_count` and `ask_level_count` count every level, not just the returned
ones. `crossed` reports whether the book violates the no-cross invariant, which
a well formed book never does.

### Read the trade tape

```http
GET /trades?symbol=ACME&limit=100&offset=0
```

`limit` is optional (default `100`, maximum `1000`) and `offset` is optional
(default `0`).

```json
{
  "symbol": "ACME",
  "price_scale": 2,
  "total": 2,
  "offset": 0,
  "limit": 100,
  "trades": [
    {
      "id": "trade-1",
      "sequence": 1,
      "symbol": "ACME",
      "price_scale": 2,
      "price": "10.05",
      "price_ticks": 1005,
      "quantity": 40,
      "maker_order_id": "ask-3",
      "taker_order_id": "bid-1",
      "maker_side": "sell",
      "executed_at": 1700000000000
    }
  ]
}
```

Trades are returned in execution order.

### Replay

```http
POST /replay

{"symbol":"ACME"}
```

The body may be empty (replay every symbol) or contain exactly `symbol`. Replay
discards the live structures, rebuilds the order book from `events.jsonl` only,
and compares the result with the live book field by field:

```json
{
  "consistent": true,
  "live_sequence": 11,
  "rebuilt_sequence": 11,
  "symbol_count": 1,
  "books": [
    {
      "consistent": true,
      "symbol": "ACME",
      "event_count": 11,
      "book_matched": true,
      "orders_matched": true,
      "trades_matched": true,
      "bid_level_count": 0,
      "ask_level_count": 1,
      "open_order_count": 2,
      "rebuilt_open_order_count": 2,
      "trade_count": 2,
      "rebuilt_trade_count": 2
    }
  ]
}
```

`consistent` is true only when the event count, every book level, the open order
count, and the trade count all agree. Replay reads no clock and no live counter,
so it is a pure function of the log; the only clock the process owns is the one
that stamps `at` when an event is first recorded.

### Events

The event log is JSON Lines, one object per line, inside `--data`:

```json
{"sequence":4,"at":1700000000000,"type":"trade_executed","data":{"maker_order_id":"ask-3","price":1005,"quantity":40,"taker_order_id":"bid-1","trade_id":"trade-1"}}
```

| Type | Payload | Meaning |
| --- | --- | --- |
| `symbol_added` | `symbol`, `price_scale` | a symbol was registered |
| `order_accepted` | `order_id`, `symbol`, `side`, `type`, `quantity`, `limit_price` (limit only) | an order entered the engine |
| `trade_executed` | `trade_id`, `maker_order_id`, `taker_order_id`, `price`, `quantity` | one fill |
| `order_updated` | `order_id`, `filled_quantity`, `remaining_quantity`, `trade_id` | an order partially filled |
| `order_filled` | `order_id`, `filled_quantity`, `trade_id` | an order reached `filled` |
| `order_cancelled` | `order_id`, `filled_quantity` | an order was cancelled |
| `order_rejected` | `order_id`, `reason`, `requested_quantity`, `filled_quantity`, `unfilled_quantity` | a market order could not be filled |

The live engine applies every event through the same code path that replay uses
and never mutates an order directly, so `events.jsonl` is the single source of
truth: deleting it resets the market, and keeping it reproduces the market
exactly after a restart. `at` comes from the process clock and only records when
an event happened; matching never reads it.

## Errors

Errors use this shape:

```json
{"error":{"code":"validation_error","message":"human readable detail"}}
```

| Status | Code | When |
| --- | --- | --- |
| 400 | `validation_error` | malformed JSON, unknown field, missing field, wrong type, bad `Content-Type`, bad query parameter, body too large, invalid symbol/price/quantity |
| 404 | `not_found` | unknown route, unknown symbol, unknown order |
| 405 | `method_not_allowed` | a known route used with the wrong method; `Allow` lists the supported methods |
| 409 | `conflict` | duplicate symbol, duplicate order id, cancelling a terminal order, reusing an idempotency key for a different operation |
| 500 | `internal_error` | unexpected failure |

## Tests

```bash
go test ./...
```

The suite covers price-time priority, partial fills, market sweeps and rejects,
cancellation, quantity conservation, the no-cross invariant, event-log replay,
reload from disk, and every HTTP status and error code above.
