# Flash-Sale Reservation System: Design Doc


## Project Overview

### Objective
A backend ticket reservation system for limited inventory, loadtested by an open-loop bot swarm, with published measurements of how different concurrency strategies behave.

**The Invariant (Success Condition):** For each tier of tickets, the number of units committed (units sold + active holds) never exceeds the tier's total stock.
* Units within a tier are considered to be identical, so each tier is an independent counter.
* Oversell is measured by an independent auditor that queries the database directly. It is never derived from the service's own counters.

### Definition of Done
1. A naive service implementation that visibly oversells under load.
2. An improved service that does not oversell even across repeated runs.
3. A concurrent bot swarm that can run 3-5 configurable behavior profiles.
4. A measured comparison of at least three concurrency strategies, reporting oversells, throughput, and p99 latency per strategy.
5. Complete documentation, including: README, design documents, and a discussion of results.

### Cut List
The following features are considered **out of scope** for this project:
* Accounts and auth
* A mock payment system
* Individually labelled units
* Multiple events in one run
* Multiple units ordered in one request
* Custom storefront UI

## The Ticket Service

### API
Each version of the reservation service must support the following API endpoints:
| Endpoint | Request Body | Response Body |
| --- | --- | --- |
| `GET /tickets/tier/{id}` | none | `GetTierResponse` |
| `POST /tickets/tier/{id}/hold` | `HoldTicketRequest` | `HoldTicketResponse` |
| `POST /tickets/checkout` | `CheckoutTicketRequest` | `CheckoutTicketResponse` |

Where the request and response schema are detailed as follows:
```
GetTierResponse: {
    tier_id: int,
    total_stock: int,
    available: int,
    active_holds: int,
    name: optional[str]
}
```
```
HoldTicketRequest: {
    idempotency_id: uuid,
    bot_id: optional[uuid]
}
```
```
HoldTicketResponse: {
    hold_id: uuid,
    tier_id: int,
    expires_at: timestamp
}
```
```
CheckoutTicketRequest: {
    hold_id: uuid
}
```
```
CheckoutTicketResponse: {
    order_id: uuid
}
```
The service must also support `GET` endpoints for a basic healthcheck + for metrics.

### Errors
A non-exhaustive list of errors the service may produce are as follows:
| Status | Message |
| --- | --- |
| `404` | `tier_not_found` |
| `404` | `hold_not_found` |
| `409` | `tier_sold_out` |
| `410` | `hold_expired` |
| `503` | `service_overloaded` |

### Database Schema
The service must contain at least the following three tables:
* `tiers(id, total_stock, available, name, created_at)`
* `holds(id, tier_id, state, created_at, expires_at, idempotency_id)`
* `orders(id, hold_id, tier_id, created_at)`

Where `holds` must be **unique** on `(tier_id, idempotency_id)`, and `orders` must be **unique** on `(hold_id)`.

The following indexes are part of the schema, and every service version must use exactly this set so that benchmarks are comparable:

- `holds_committed_idx`: Partial index on `holds(tier_id) WHERE state IN ('active','converted')`, which keeps the auditor's cost independent of run length.
- `holds_expiry_idx`: Partial index on `holds(expires_at) WHERE state = 'active'`, which does the same for the expiry job.

The unique constraints above add their own indexes.

### Hold States

`holds.state` takes one of three values:

| State | Meaning | Counts toward invariant | Moves to |
| --- | --- | --- | --- |
| `active` | Hold issued, not yet checked out, `expires_at` in the future | Yes | `converted`, `expired` |
| `converted` | Checked out; an `orders` row exists | Yes | none (terminal) |
| `expired` | `expires_at` passed without checkout; unit returned to the tier | No | none (terminal) |

A hold is created as `active`. A checkout on a `converted` hold returns the existing order, and a repeated hold request with the same `idempotency_id` returns the existing hold. Otherwise, checkout succeeds only if the hold is `active` and `now < expires_at`; a checkout on an `expired` hold, or on an `active` hold past its `expires_at`, returns `410 hold_expired`.

### Hold Expiry

- TTL: a server config, default 60 seconds.
- Clock: the database's `now()` is the only clock. `expires_at` is set as `now() + TTL` at insert, and checkout compares against `now()` in the same statement.
- Mechanism: a background job runs every second and, in one statement, sets `active` holds with `expires_at <= now()` to `expired` and adds those units back to `tiers.available`. The naive service may omit this job.
- `available`: a convenience counter, equal to `total_stock - (active + converted)` when the system is consistent. It is never used for the oversell metric.

### The Auditor

A separate process with its own read-only database role. It queries Postgres directly once per second and exports one Prometheus gauge per tier. It never calls the service and never reads `tiers.available`.

```sql
SELECT t.id, COALESCE(c.n, 0) - t.total_stock
FROM tiers t
LEFT JOIN (
  SELECT tier_id, count(*) AS n
  FROM holds
  WHERE state IN ('active','converted')
  GROUP BY tier_id
) c ON c.tier_id = t.id
```

A value above zero is an invariant violation. A hold whose `expires_at` has passed but whose state is still `active` (the expiry job hasn't run yet) counts as committed, since its unit has not been returned.

## The Bot Swarm

Each bot "instance" in the swarm must, at minimum, fulfill the following requirements:
* Send well-formed requests to and receive responses from the ticketing service, abiding by the API specs above.
* Gracefully handle each of the service errors specified above.

Additionally, the bot swarm must have the following properties:
* Open loop: each bot "spawn" is scheduled from a clock at a configured rate. Each request gets its own `goroutine`, and the swarm never waits for a response before scheduling the next arrival.
* Latency measured from the intended send time, not the actual send time, to avoid coordinated omission.
* Outputs: Prometheus metrics (requests by status code and error message, latency histogram, in-flight count), plus a raw per-request CSV or JSONL for benchmarks.
* Self-check: a metric for swarm scheduling lag, to show the generator was not the bottleneck.

### Bot Profiles
The basic bot profile is the **"Buyer"**, which places a hold and then checks out after a few seconds' wait.

Eventually, the swarm must support additional behavior profiles, namely:
* **Retrier**: Retries on timeout, with exponential backoff and jitter. Reuses the same `idempotency_id`.
* **Duplicate-Sender**: Deliberately sends the same hold or checkout twice. Asserts it gets the same response back.
* **Abandoner**: Holds and never checks out.

### Bot Config
Configuration for the bot swarm is configured via a YAML file, which follows the following **example** schema:
```yaml
bot_swarm:
    target_url: http://localhost:4000
    rate_profile:  // rps - arrivals per second
        - {duration_s: 30, start_rps: 50, end_rps: 50}
        - {duration_s: 60, start_rps: 50, end_rps: 500}
    tier_weights: {
        // relative odds per tier_id
        1: 100,
        2: 5000,
        ...
    },
    bot_profiles: [
        {
            name: "buyer",
            weight: 20,
            checkout_delay_s: 3
        },
    ]
    request_timeout_s: 5
    rng_seed: 123456  // controls jitter, profile choice, tier choice
```