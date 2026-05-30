# PostgreSQL Connection Pool — Hands-On Demo

This project shows **exactly** what happens when your connection pool is misconfigured.  
Every scenario runs real code against a real PostgreSQL container and prints real stats.

---

## Background: What Is a Connection Pool?

Opening a database connection is expensive — TCP handshake, authentication, memory allocation on both sides. It typically takes **5–20 ms**.

A **connection pool** keeps a set of connections open and reuses them across requests, so each request doesn't pay that cost.

Go's `database/sql` package has a built-in pool. You control it with four settings:

```go
db.SetMaxOpenConns(n)      // How many connections can be open at once (in-use + idle)
db.SetMaxIdleConns(n)      // How many idle connections to keep ready in the pool
db.SetConnMaxLifetime(d)   // Close a connection after it has been open this long
db.SetConnMaxIdleTime(d)   // Close a connection after it has been idle this long
```

You can inspect the current state any time with:

```go
stats := db.Stats()
stats.OpenConnections    // currently open (in-use + idle)
stats.InUse              // connections running a query right now
stats.Idle               // connections sitting in the pool, ready to use
stats.WaitCount          // how many goroutines had to wait for a free connection
stats.WaitDuration       // total time all goroutines spent waiting
stats.MaxIdleClosed      // connections closed because idle pool was full
stats.MaxIdleTimeClosed  // connections closed because they were idle too long
stats.MaxLifetimeClosed  // connections closed because they were too old
```

The two most common problems:
- **Pool too small** → goroutines queue up, response time increases, or requests fail
- **Pool too large** → too many connections on the server side, server rejects them

---

## Project Layout

```
.
├── docker-compose.yml               # Two PostgreSQL containers
├── Makefile                         # Shortcuts for every scenario
├── internal/dbutil/stats.go         # Shared helper to print db.Stats()
├── scenarios/
│   ├── 01_server_max_conn/          # Server-side limit hit
│   ├── 02_pool_exhaustion/          # App-side pool too small
│   ├── 03_idle_connections/         # MaxIdleConns + ConnMaxIdleTime
│   └── 04_timeouts/                 # All timeout types
└── loadtest/                        # Configurable load test
```

**Two database containers:**

| Container | Port | `max_connections` | Used for |
|---|---|---|---|
| `pg-normal` | 5440 | 200 | Scenarios 2, 3, 4, load test |
| `pg-limited` | 5441 | 10 | Scenario 1 (server limit demo) |

---

## Quick Start

```bash
# 1. Start the databases
make up && make wait

# 2. Run each scenario
make scenario1   # server limit
make scenario2   # pool exhaustion
make scenario3   # idle connections
make scenario4   # timeouts

# 3. Run the load test
make loadtest         # pool=5, workers=20 → contention
make loadtest-ok      # pool=20, workers=20 → healthy baseline
make loadtest-server  # 20 workers against pg-limited (server limit)
```

---

## Scenario 1 — PostgreSQL Server `max_connections` Limit

**File:** `scenarios/01_server_max_conn/`

**The setup:**
- `pg-limited` is configured with `max_connections=10`
- The app pool has **no limit** (`SetMaxOpenConns(0)`)
- 15 goroutines each run `SELECT pg_sleep(3)` — this holds the connection open for 3 seconds, forcing all 15 to need a connection at the same time

**What happens:**

```
[Worker  6] OK     finished in 3.0s
[Worker  1] OK     finished in 3.0s
...
[Worker  7] ERROR  pq: sorry, too many clients already
[Worker 11] ERROR  pq: sorry, too many clients already

Results: 10 succeeded, 5 failed
```

The server accepted 10 connections and hard-rejected the rest. The app pool setting doesn't matter here — the **server** is the wall.

**Key lesson:**  
`max_connections` on the PostgreSQL server is a hard limit. If all your app instances together open more connections than this, some will be rejected.  
Fix: raise `max_connections` in `postgresql.conf` (needs a server restart) **or** use a connection pooler like PgBouncer in front of PostgreSQL.

---

## Scenario 2 — Application Pool Too Small

**File:** `scenarios/02_pool_exhaustion/`

**The setup:**
- `pg-normal` has `max_connections=200` — plenty of room on the server
- The app sets `SetMaxOpenConns(3)` — only 3 connections allowed
- 9 goroutines each run a 2-second query

### Part A — Degradation (no timeout)

With no deadline set, Go's pool makes extra goroutines **wait** silently:

```
[Worker  1] OK    elapsed=2.01s   ← got a connection immediately
[Worker  2] OK    elapsed=2.01s
[Worker  3] OK    elapsed=2.01s
[Worker  4] OK    elapsed=4.02s   ← had to wait 2s for a free slot
[Worker  7] OK    elapsed=4.02s
...
Total wall time: 6.03s  (expected ~6s — 3 batches × 2s)

WaitCount    : 6       ← 6 goroutines waited
WaitDuration : 18.09s  ← total wait time across all goroutines
```

The app is alive but 3× slower. This is **degradation**.

### Part B — Error (with context deadline)

With a 3.5-second deadline, goroutines that can't finish within the deadline fail:

```
[Worker  1] OK     elapsed=2.00s   ← got connection immediately, finished in time
[Worker  4] OK     elapsed=2.01s
[Worker  7] OK     elapsed=2.01s
[Worker  5] ERROR  elapsed=3.50s   err=context deadline exceeded
[Worker  9] ERROR  elapsed=3.50s   err=context deadline exceeded
...
Results: 3 succeeded, 6 failed
```

Workers 1–3 got connections immediately and finished at t≈2s (within the 3.5s deadline).  
Workers 4–9 had to wait until t≈2s for a free slot, then needed another 2s for the query, but the deadline was t=3.5s — not enough time.

**Key lesson:**  
A pool that is too small causes either slow responses (degradation) or errors (when a deadline is set). Always set `SetMaxOpenConns` to match your expected concurrency, and always use a context with a deadline on DB calls.

---

## Scenario 3 — Idle Connections

**File:** `scenarios/03_idle_connections/`

### Part A — Effect of `MaxIdleConns`

After a burst of 10 concurrent queries, how many connections stay in the pool?

| Config | `Idle` after burst | `MaxIdleClosed` |
|---|---|---|
| `SetMaxIdleConns(2)` | **2** | **8** |
| `SetMaxIdleConns(10)` | **10** | **0** |

With `MaxIdleConns=2`, the pool discards 8 connections as soon as queries finish. The next burst has to re-open them (paying the connection cost again).

**Key lesson:**  
Set `MaxIdleConns` to a value close to your typical concurrency. Too low → you pay connection setup cost on every burst. Too high → you hold connections you don't need.

### Part B — `ConnMaxIdleTime`

Connections that sit idle too long are automatically closed:

```
Config: ConnMaxIdleTime=3s

t=+0s  Idle=10 [##########]  MaxIdleTimeClosed=0
t=+1s  Idle=10 [##########]  MaxIdleTimeClosed=0
t=+2s  Idle=10 [##########]  MaxIdleTimeClosed=0
t=+3s  Idle=5  [#####.....]  MaxIdleTimeClosed=5
t=+4s  Idle=0  [..........]  MaxIdleTimeClosed=10
```

After 3 seconds of idleness, all 10 connections are closed. `MaxIdleTimeClosed` counts how many were removed this way.

**Key lesson:**  
Use `SetConnMaxIdleTime` to avoid holding connections to a server that may have restarted, or to a managed database (like AWS RDS) that closes idle connections after a few minutes on its own.

### Part C — BENEFIT: Idle Connections Speed Up Queries

We run 20 sequential `SELECT 1` queries — a near-instant query — and measure each one individually. The only source of latency difference between the two configs is **connection setup overhead**.

```
--- Config A: MaxIdleConns=0 (pool closes connection after every query) ---
  query  1:  15.55 ms  [new conn]
  query  2:   7.38 ms  [new conn]
  query  3:   5.33 ms  [new conn]
  ...
  query 20:   4.37 ms  [new conn]   ← pays TCP + auth cost every single time

--- Config B: MaxIdleConns=1 (pool keeps one idle connection) ---
  query  1:   4.28 ms  [new conn]   ← first query still opens a connection
  query  2:   0.40 ms  [reused]     ← reuses the idle connection, no setup cost
  query  3:   0.33 ms  [reused]
  ...
  query 20:   0.41 ms  [reused]

Result:
  Config A  avg: 4.97 ms/query   (new connection every time)
  Config B  avg: 0.34 ms/query   (reusing idle connection)
  Idle pool is 14.8x faster

  MaxIdleClosed  A=20   B=0
  (Config A discarded every connection; Config B reused them)
```

Why is Config A slower on every single query? Each time, it goes through:
1. Open TCP socket to PostgreSQL
2. Send startup message
3. Authenticate (username + password check)
4. Run the query
5. Close the TCP socket

Config B only does steps 1–3 once (on query 1). Every query after that skips straight to step 4.

**Key lesson:**  
With `MaxIdleConns=0`, your app pays 5–15ms of connection setup overhead on **every single query**. A service doing 100 req/s would waste 500–1500ms of CPU time per second just on reconnecting. A properly sized idle pool is one of the cheapest performance wins you can get.

### Part D — COST: Too Many Idle Connections Waste Server Resources

PostgreSQL is different from many other databases: it spawns **one OS process per connection**, even if that connection is completely idle. This is called the process-per-connection model.

Each idle backend process:
- Holds ~5–10 MB of memory on the server
- Consumes one slot from `max_connections`
- Keeps file descriptors and shared memory structures allocated

We open 30 idle connections and ask PostgreSQL directly what it sees:

```
App pool after burst:
  Idle=30  InUse=0  OpenConnections=30

PostgreSQL pg_stat_activity — the server's view:
  state           count
  -----           -----
  idle            29   ← our pool connections doing nothing
  active           1   ← this pg_stat_activity query itself

Connection slot usage on the server:
  Used      : 30
  Max       : 200  (max_connections)
  Remaining : 170
```

Now imagine you have multiple instances of your app, each holding 30 idle connections:

```
Instances   Total idle conns   % of max_connections   Slots left for queries
---------   ----------------   --------------------   ----------------------
1           30                 15%                    170
3           90                 45%                    110
5           150                75%                    50
10          300                150%                   -100  ← SERVER FULL
```

With 10 instances × 30 idle connections = 300 connections, you have **exceeded max_connections=200**. New connections will be rejected with `sorry, too many clients already` — even though all 300 existing connections are just sitting idle doing nothing.

**Key lesson:**  
Idle connections are not free. They cost the PostgreSQL server memory and connection slots.  
The right `MaxIdleConns` is your **typical concurrent requests per instance** — not "as many as possible".

```
Good rule of thumb:
  MaxOpenConns = max_connections / number_of_app_instances
  MaxIdleConns = typical_concurrent_requests_per_instance

Example: max_connections=200, 5 instances, 10 concurrent requests each
  MaxOpenConns = 200 / 5 = 40
  MaxIdleConns = 10
  Total idle connections at rest = 5 × 10 = 50  (25% of server capacity)
```

---

## Scenario 4 — Timeout Parameters

**File:** `scenarios/04_timeouts/`

There are five different timeout mechanisms. They protect different phases.

### 4A — `connect_timeout` (DSN parameter)

Controls how long the **initial TCP + authentication handshake** may take.  
Without it, a connection to an unreachable host will hang for minutes.

```go
// In the DSN string
"host=192.0.2.1 port=5432 ... connect_timeout=2"
```

```
Connecting to 192.0.2.1 (unreachable)...
Error after 2.0s: dial tcp 192.0.2.1:5432: i/o timeout
```

Without `connect_timeout=2`, this would hang for ~2 minutes (OS default TCP timeout).

---

### 4B — `context.WithTimeout` (Go code, client-side)

Controls the **total time allowed for one DB operation** — including time spent waiting for a free pool slot plus the actual query execution.

```go
ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
defer cancel()

_, err := db.ExecContext(ctx, "SELECT pg_sleep(5)")
// Error after 2.00s: pq: canceling statement due to user request
```

This is what you should put on **every** DB call in your application.

---

### 4C — `statement_timeout` (PostgreSQL server-side)

A setting on the PostgreSQL side that kills any query running longer than N milliseconds. The server itself enforces it — your Go code doesn't need to do anything.

```go
db.Exec("SET statement_timeout = '2s'")
db.Exec("SELECT pg_sleep(5)")
// Error after 2.01s: pq: canceling statement due to statement timeout
```

Notice the error message is different from 4B:
- 4B (client cancel): `canceling statement due to user request`
- 4C (server kill): `canceling statement due to statement timeout`

You can set it globally in `postgresql.conf`, per role, or in the DSN:

```
host=... options=-c statement_timeout=5000
```

**Why have both 4B and 4C?**  
They are defense-in-depth. If your Go code crashes or forgets to cancel, the server still kills the runaway query.

---

### 4D — `db.SetConnMaxLifetime`

Forces connections to be closed and recreated after a set duration, even if they are healthy.

```
t=+ 0s  Open=1  MaxLifetimeClosed=0
t=+ 3s  Open=0  MaxLifetimeClosed=1   ← connection recycled
t=+ 4s  Open=1  MaxLifetimeClosed=1   ← new connection opened
t=+ 7s  Open=0  MaxLifetimeClosed=2   ← recycled again
```

**When to use it:**
- Your TLS certificates rotate and you want connections to pick up the new cert
- Your load balancer has a sticky-session timeout and closes connections after N minutes — set `ConnMaxLifetime` slightly shorter so Go closes first and avoids "broken pipe" errors

---

### 4E — `db.SetConnMaxIdleTime`

Closes connections that have been sitting in the pool unused for too long.  
(Same behaviour as Scenario 3B — shown here for a side-by-side comparison.)

```
t=+0s  Idle=5 [#####]  MaxIdleTimeClosed=0
t=+3s  Idle=0 [.....]  MaxIdleTimeClosed=5
```

---

## Load Test

**File:** `loadtest/`

A configurable CLI tool that runs concurrent workers against the database and prints a real-time stats table plus a final latency report.

```bash
go run ./loadtest/ -pool 5 -workers 20 -qtime 0.1 -dur 15s
```

| Flag | Default | Meaning |
|---|---|---|
| `-pool` | 5 | `SetMaxOpenConns` |
| `-idle` | same as pool | `SetMaxIdleConns` |
| `-workers` | 20 | concurrent goroutines |
| `-qtime` | 0.1 | simulated query time (`pg_sleep`) |
| `-dur` | 15s | test duration |
| `-db` | pg-normal DSN | connection string |

### Contended pool (`-pool 5 -workers 20`)

```
t(s)  RPS   Open  InUse  WaitCnt  WaitMs
0     45    5     5      60       10664     ← 15 goroutines always waiting
1     45    5     5      105      23350
...
p50 latency: 303 ms   p95: 1283 ms   p99: 2462 ms
WaitCount: 575   AvgWait: 317 ms per goroutine
>> Pool contention detected.
```

### Healthy pool (`-pool 20 -workers 20`)

```
t(s)  RPS   Open  InUse  WaitCnt  WaitMs
0     180   20    20     0        0         ← no waiting
1     181   20    20     0        0
...
p50 latency: 107 ms   p95: 112 ms   p99: 117 ms
WaitCount: 0
>> No pool contention. Pool size was sufficient for the workload.
```

Same workload, same server — just changing `pool` from 5 to 20 gave **4× more throughput** and **11× lower tail latency**.

---

## Recommended Pool Settings (Rule of Thumb)

```go
// MaxOpenConns: rough starting point = number of CPU cores on the DB server × 2
// Then tune up or down based on load test results and db.Stats().WaitCount
db.SetMaxOpenConns(25)

// MaxIdleConns: set equal to MaxOpenConns so connections are reused after a burst
db.SetMaxIdleConns(25)

// ConnMaxLifetime: slightly less than your load balancer's connection timeout
// A common LB timeout is 10 minutes, so set 9 minutes
db.SetConnMaxLifetime(9 * time.Minute)

// ConnMaxIdleTime: close connections unused for more than 5 minutes
// RDS/Aurora typically close idle connections after 8 minutes
db.SetConnMaxIdleTime(5 * time.Minute)
```

If `db.Stats().WaitCount` keeps growing under load, your pool is too small.  
If your server has too many connections, your pool (× number of app instances) is too large.
