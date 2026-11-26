# Write-transaction hold times on the SQLite build

Status: Measured — 2026-10-16

This report sizes the PostgreSQL write-serialization ruling, which takes one
global advisory lock per read-write transaction. It measures how long each
read-write transaction holds the SQLite writer lock, and how many statements
it runs, then estimates the PostgreSQL hold as the SQLite hold plus one network
round trip per statement.

## Method

`internal/engine/holdtime_probe_test.go` produced every figure here. Regenerate
the report with:

```sh
DOCKET_HOLDTIME_REPORT=$PWD/docs/design/hold-time-report.md go test ./internal/engine -run TestHoldTimeProbe -count=1
```

- One SQLite store per client count, holding one activated run of 96 issues
  on the engine's test workflow fixture. The issues are split evenly across the
  clients, so every client count does the same total work.
- Each client is its own `*sql.DB` on the store file, opened as `db.Open` opens a
  store: WAL, `_txlock=immediate`, one connection, `busy_timeout=5000`.
- Each client loops over its own `implement` steps: `claim` (`ClaimStep`),
  `heartbeat` (`HeartbeatStep`), `record` (`CompleteStep`'s stage 0 and 1) and
  `complete` (the saga resume that runs the step's five pass-through gates and
  routes it to `done`).
- A driver wrapper times every read-write transaction from the moment `BEGIN
  IMMEDIATE` returns until `COMMIT` returns, and counts the statements prepared
  inside it. Read-only transactions take no writer lock and are not counted.
  A transaction kind is the verb plus the transaction's ordinal within one call
  of that verb.
- Percentiles are nearest-rank.
- Estimated PostgreSQL utilization = commits/s × (p99 hold + statements/tx ×
  round trip) / 1000, with times in milliseconds. It is computed from the
  rounded figures printed in the same row, so any cell can be recomputed.
- SQLite utilization = total hold time of all read-write transactions / wall
  time. Above 0.9, SQLite's own writer lock capped demand, so commits/s at
  that client count understates the offered write rate.

Host: darwin/arm64, 18 CPUs, go1.26.9. Sweep wall time: 26s.

## Findings

- At a 0.2 ms round trip, the smallest client count whose estimated
  PostgreSQL utilization exceeds 0.5: 1 client (estimated utilization 2.343).
- At a 1 ms round trip, the smallest client count whose estimated
  PostgreSQL utilization exceeds 0.5: 1 client (estimated utilization 6.733).
- These clients loop with no work between verbs, so commits/s is the rate the
  store sustained, not a production demand. From the 1-client row (p99 hold
  7.581 ms, 33.40 statements/tx) and 15 commits per claim-to-done loop,
  estimated PostgreSQL utilization reaches 0.5 at:
  - 0.2 ms round trip: 35.1 commits/s, 2.34 step loops/s.
  - 1 ms round trip: 12.2 commits/s, 0.81 step loops/s.
- SQLite utilization exceeded 0.9 at no measured client count.
- Write statements run outside an explicit transaction: 0.

## Utilization

| Clients | Commits/s | p99 hold (ms) | Statements/tx | Est. PG util @ 0.2 ms RTT | Est. PG util @ 1 ms RTT | SQLite util | SQLite saturated |
|---|---|---|---|---|---|---|---|
| 1 | 164.3 | 7.581 | 33.40 | 2.343 | 6.733 | 0.157 | no |
| 2 | 274.6 | 7.890 | 33.40 | 4.001 | 11.338 | 0.268 | no |
| 4 | 384.1 | 8.966 | 33.40 | 6.010 | 16.273 | 0.402 | no |
| 8 | 396.5 | 11.241 | 33.40 | 7.106 | 17.700 | 0.500 | no |
| 16 | 346.9 | 15.309 | 33.40 | 7.628 | 16.897 | 0.583 | no |

## Transactions by kind

### 1 client

Wall time 8.764 s.

| Kind | Count | Rolled back | p50 hold (ms) | p99 hold (ms) | Statements/tx | Est. PG hold @ 0.2 ms RTT (ms) | Est. PG hold @ 1 ms RTT (ms) |
|---|---|---|---|---|---|---|---|
| claim tx1 | 96 | 0 | 5.225 | 8.709 | 224.00 | 53.509 | 232.709 |
| heartbeat tx1 | 96 | 0 | 0.076 | 0.378 | 2.00 | 0.778 | 2.378 |
| record tx1 | 96 | 0 | 0.177 | 0.585 | 8.00 | 2.185 | 8.585 |
| complete tx1 | 96 | 0 | 0.083 | 0.879 | 3.00 | 1.479 | 3.879 |
| complete tx2 | 96 | 0 | 0.152 | 0.509 | 6.00 | 1.709 | 6.509 |
| complete tx3 | 96 | 0 | 0.075 | 0.188 | 3.00 | 0.788 | 3.188 |
| complete tx4 | 96 | 0 | 0.131 | 0.296 | 6.00 | 1.496 | 6.296 |
| complete tx5 | 96 | 0 | 0.077 | 0.440 | 3.00 | 1.040 | 3.440 |
| complete tx6 | 96 | 0 | 0.135 | 0.943 | 6.00 | 2.143 | 6.943 |
| complete tx7 | 96 | 0 | 0.078 | 1.158 | 3.00 | 1.758 | 4.158 |
| complete tx8 | 96 | 0 | 0.136 | 0.556 | 6.00 | 1.756 | 6.556 |
| complete tx9 | 96 | 0 | 0.077 | 3.020 | 3.00 | 3.620 | 6.020 |
| complete tx10 | 96 | 0 | 0.132 | 2.638 | 6.00 | 3.838 | 8.638 |
| complete tx11 | 96 | 0 | 0.052 | 0.094 | 1.00 | 0.294 | 1.094 |
| complete tx12 | 96 | 0 | 7.269 | 11.828 | 221.00 | 56.028 | 232.828 |
| all | 1440 | 0 | 0.117 | 7.581 | 33.40 | 14.261 | 40.981 |

### 2 clients

Wall time 5.245 s.

| Kind | Count | Rolled back | p50 hold (ms) | p99 hold (ms) | Statements/tx | Est. PG hold @ 0.2 ms RTT (ms) | Est. PG hold @ 1 ms RTT (ms) |
|---|---|---|---|---|---|---|---|
| claim tx1 | 96 | 0 | 5.481 | 7.961 | 224.00 | 52.761 | 231.961 |
| heartbeat tx1 | 96 | 0 | 0.069 | 0.173 | 2.00 | 0.573 | 2.173 |
| record tx1 | 96 | 0 | 0.180 | 0.563 | 8.00 | 2.163 | 8.563 |
| complete tx1 | 96 | 0 | 0.094 | 0.257 | 3.00 | 0.857 | 3.257 |
| complete tx2 | 96 | 0 | 0.157 | 0.564 | 6.00 | 1.764 | 6.564 |
| complete tx3 | 96 | 0 | 0.083 | 1.606 | 3.00 | 2.206 | 4.606 |
| complete tx4 | 96 | 0 | 0.139 | 0.397 | 6.00 | 1.597 | 6.397 |
| complete tx5 | 96 | 0 | 0.079 | 0.241 | 3.00 | 0.841 | 3.241 |
| complete tx6 | 96 | 0 | 0.136 | 0.313 | 6.00 | 1.513 | 6.313 |
| complete tx7 | 96 | 0 | 0.080 | 0.411 | 3.00 | 1.012 | 3.412 |
| complete tx8 | 96 | 0 | 0.135 | 0.359 | 6.00 | 1.559 | 6.359 |
| complete tx9 | 96 | 0 | 0.082 | 0.430 | 3.00 | 1.030 | 3.430 |
| complete tx10 | 96 | 0 | 0.133 | 0.360 | 6.00 | 1.560 | 6.360 |
| complete tx11 | 96 | 0 | 0.052 | 0.484 | 1.00 | 0.684 | 1.484 |
| complete tx12 | 96 | 0 | 7.517 | 8.695 | 221.00 | 52.895 | 229.695 |
| all | 1440 | 0 | 0.126 | 7.890 | 33.40 | 14.570 | 41.290 |

### 4 clients

Wall time 3.749 s.

| Kind | Count | Rolled back | p50 hold (ms) | p99 hold (ms) | Statements/tx | Est. PG hold @ 0.2 ms RTT (ms) | Est. PG hold @ 1 ms RTT (ms) |
|---|---|---|---|---|---|---|---|
| claim tx1 | 96 | 0 | 5.715 | 7.491 | 224.00 | 52.291 | 231.491 |
| heartbeat tx1 | 96 | 0 | 0.072 | 0.362 | 2.00 | 0.762 | 2.362 |
| record tx1 | 96 | 0 | 0.204 | 0.679 | 8.00 | 2.279 | 8.679 |
| complete tx1 | 96 | 0 | 0.092 | 0.493 | 3.00 | 1.093 | 3.493 |
| complete tx2 | 96 | 0 | 0.155 | 0.914 | 6.00 | 2.114 | 6.914 |
| complete tx3 | 96 | 0 | 0.093 | 0.272 | 3.00 | 0.872 | 3.272 |
| complete tx4 | 96 | 0 | 0.141 | 0.473 | 6.00 | 1.673 | 6.473 |
| complete tx5 | 96 | 0 | 0.082 | 0.314 | 3.00 | 0.914 | 3.314 |
| complete tx6 | 96 | 0 | 0.141 | 0.430 | 6.00 | 1.630 | 6.430 |
| complete tx7 | 96 | 0 | 0.090 | 0.460 | 3.00 | 1.060 | 3.460 |
| complete tx8 | 96 | 0 | 0.142 | 0.568 | 6.00 | 1.768 | 6.568 |
| complete tx9 | 96 | 0 | 0.090 | 1.128 | 3.00 | 1.728 | 4.128 |
| complete tx10 | 96 | 0 | 0.143 | 0.605 | 6.00 | 1.805 | 6.605 |
| complete tx11 | 96 | 0 | 0.053 | 0.383 | 1.00 | 0.583 | 1.383 |
| complete tx12 | 96 | 0 | 8.062 | 9.794 | 221.00 | 53.994 | 230.794 |
| all | 1440 | 0 | 0.131 | 8.966 | 33.40 | 15.646 | 42.366 |

### 8 clients

Wall time 3.632 s.

| Kind | Count | Rolled back | p50 hold (ms) | p99 hold (ms) | Statements/tx | Est. PG hold @ 0.2 ms RTT (ms) | Est. PG hold @ 1 ms RTT (ms) |
|---|---|---|---|---|---|---|---|
| claim tx1 | 96 | 0 | 6.437 | 9.878 | 224.00 | 54.678 | 233.878 |
| heartbeat tx1 | 96 | 0 | 0.079 | 0.507 | 2.00 | 0.907 | 2.507 |
| record tx1 | 96 | 0 | 0.223 | 1.383 | 8.00 | 2.982 | 9.383 |
| complete tx1 | 96 | 0 | 0.117 | 0.519 | 3.00 | 1.119 | 3.519 |
| complete tx2 | 96 | 0 | 0.178 | 1.505 | 6.00 | 2.705 | 7.505 |
| complete tx3 | 96 | 0 | 0.114 | 1.168 | 3.00 | 1.768 | 4.168 |
| complete tx4 | 96 | 0 | 0.170 | 1.135 | 6.00 | 2.335 | 7.135 |
| complete tx5 | 96 | 0 | 0.120 | 0.706 | 3.00 | 1.306 | 3.706 |
| complete tx6 | 96 | 0 | 0.174 | 0.682 | 6.00 | 1.882 | 6.682 |
| complete tx7 | 96 | 0 | 0.116 | 1.120 | 3.00 | 1.720 | 4.120 |
| complete tx8 | 96 | 0 | 0.171 | 1.029 | 6.00 | 2.229 | 7.029 |
| complete tx9 | 96 | 0 | 0.126 | 0.779 | 3.00 | 1.379 | 3.779 |
| complete tx10 | 96 | 0 | 0.174 | 1.479 | 6.00 | 2.679 | 7.479 |
| complete tx11 | 96 | 0 | 0.079 | 1.077 | 1.00 | 1.277 | 2.077 |
| complete tx12 | 96 | 0 | 9.206 | 23.311 | 221.00 | 67.511 | 244.311 |
| all | 1440 | 0 | 0.165 | 11.241 | 33.40 | 17.921 | 44.641 |

### 16 clients

Wall time 4.151 s.

| Kind | Count | Rolled back | p50 hold (ms) | p99 hold (ms) | Statements/tx | Est. PG hold @ 0.2 ms RTT (ms) | Est. PG hold @ 1 ms RTT (ms) |
|---|---|---|---|---|---|---|---|
| claim tx1 | 96 | 0 | 7.352 | 18.850 | 224.00 | 63.650 | 242.850 |
| heartbeat tx1 | 96 | 0 | 0.149 | 4.311 | 2.00 | 4.711 | 6.311 |
| record tx1 | 96 | 0 | 0.441 | 5.756 | 8.00 | 7.356 | 13.756 |
| complete tx1 | 96 | 0 | 0.220 | 4.073 | 3.00 | 4.673 | 7.073 |
| complete tx2 | 96 | 0 | 0.259 | 5.526 | 6.00 | 6.726 | 11.526 |
| complete tx3 | 96 | 0 | 0.245 | 3.827 | 3.00 | 4.427 | 6.827 |
| complete tx4 | 96 | 0 | 0.326 | 4.557 | 6.00 | 5.757 | 10.557 |
| complete tx5 | 96 | 0 | 0.224 | 1.861 | 3.00 | 2.461 | 4.861 |
| complete tx6 | 96 | 0 | 0.347 | 2.712 | 6.00 | 3.912 | 8.712 |
| complete tx7 | 96 | 0 | 0.254 | 5.736 | 3.00 | 6.336 | 8.736 |
| complete tx8 | 96 | 0 | 0.369 | 1.572 | 6.00 | 2.772 | 7.572 |
| complete tx9 | 96 | 0 | 0.250 | 4.931 | 3.00 | 5.531 | 7.931 |
| complete tx10 | 96 | 0 | 0.350 | 2.185 | 6.00 | 3.385 | 8.185 |
| complete tx11 | 96 | 0 | 0.123 | 4.224 | 1.00 | 4.424 | 5.224 |
| complete tx12 | 96 | 0 | 11.312 | 21.697 | 221.00 | 65.897 | 242.697 |
| all | 1440 | 0 | 0.304 | 15.309 | 33.40 | 21.989 | 48.709 |

## Limits

- The estimate adds round trips only for the statements counted here. `BEGIN`,
  the advisory-lock call and `COMMIT` add about three more per transaction on
  PostgreSQL, and the formula leaves them out.
- SQLite statement time is not PostgreSQL statement time; the SQLite hold
  stands in for the server-side work.
- Every operation uses one fixed clock value, the workflow's gates are
  pass-through stubs, and the diff is a stub, so no subprocess runs inside or
  between transactions. Real gates run outside transactions, so they lower the
  offered write rate per client without changing hold times.
- One run size, 96 issues. The probe does not measure how hold times and
  statement counts change with run size.
- SQLite's busy handler sleeps between lock retries, so the lock can sit idle
  while writers wait. Commits/s can stop scaling before SQLite utilization
  reaches 0.9.
- The clients are goroutines in one process, not separate processes, so
  they share one Go runtime and garbage collector.
- One host, one sweep. Figures vary between runs and machines.
