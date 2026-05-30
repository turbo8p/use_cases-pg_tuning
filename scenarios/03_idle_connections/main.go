// Scenario 3: MaxIdleConns — the good side and the bad side.
//
// Part A — Effect of MaxIdleConns on idle pool size:
//   After a burst, compare how many connections stay in the pool
//   between MaxIdleConns=2 and MaxIdleConns=10.
//
// Part B — ConnMaxIdleTime: idle connections expire automatically.
//
// Part C — BENEFIT: idle connections reduce per-query latency.
//   Config A: MaxIdleConns=0 — pool closes the connection after every query.
//             Next query must open a new TCP connection → pays setup cost every time.
//   Config B: MaxIdleConns=1 — pool keeps one idle connection.
//             Queries after the first reuse the open connection → near-zero setup cost.
//
// Part D — COST: too many idle connections waste server resources.
//   PostgreSQL uses one OS process per connection, even idle ones.
//   We open 30 idle connections and look at pg_stat_activity to see the server's view.
//   Then we extrapolate what happens when multiple app instances do the same thing.
package main

import (
	"database/sql"
	"fmt"
	"sort"
	"sync"
	"time"

	"pg-tuning/internal/dbutil"

	_ "github.com/lib/pq"
)

const dsn = "host=localhost port=5440 dbname=demo user=demo password=demo sslmode=disable"

func main() {
	runPartA()
	runPartB()
	runPartC()
	runPartD()
}

// ─── Part A: MaxIdleConns effect on pool size ────────────────────────────────

func runPartA() {
	dbutil.Header("Scenario 3A: MaxIdleConns — how many idle connections stay in the pool")

	fmt.Println()
	fmt.Printf("We fire 10 concurrent queries, then inspect db.Stats()\n\n")

	configs := []struct {
		label        string
		maxOpen      int
		maxIdle      int
		expectIdle   string
		expectClosed string
	}{
		{
			label:        "MaxIdleConns=2 (small idle pool)",
			maxOpen:      10,
			maxIdle:      2,
			expectIdle:   "2",
			expectClosed: "~8 (excess connections discarded immediately)",
		},
		{
			label:        "MaxIdleConns=10 (idle pool matches burst size)",
			maxOpen:      10,
			maxIdle:      10,
			expectIdle:   "10",
			expectClosed: "0 (all connections kept for reuse)",
		},
	}

	for _, cfg := range configs {
		dbutil.Divider()
		fmt.Printf("Config: %s\n", cfg.label)
		fmt.Printf("  Expected Idle after burst : %s\n", cfg.expectIdle)
		fmt.Printf("  Expected MaxIdleClosed    : %s\n\n", cfg.expectClosed)

		db, err := sql.Open("postgres", dsn)
		if err != nil {
			panic(err)
		}
		db.SetMaxOpenConns(cfg.maxOpen)
		db.SetMaxIdleConns(cfg.maxIdle)

		runBurst(db, 10)
		time.Sleep(200 * time.Millisecond) // let pool settle

		fmt.Println("  db.Stats() after burst:")
		dbutil.PrintStats(db)
		db.Close()
		fmt.Println()
	}

	fmt.Println("Key Takeaway:")
	fmt.Println("  MaxIdleConns caps how many connections stay in the pool after use.")
	fmt.Println("  Extra connections are closed immediately — MaxIdleClosed counts them.")
	fmt.Println("  If MaxIdleConns < your burst size, the next burst pays connection setup cost again.")
}

// ─── Part B: ConnMaxIdleTime ──────────────────────────────────────────────────

func runPartB() {
	dbutil.Header("Scenario 3B: ConnMaxIdleTime — idle connections expire automatically")

	fmt.Println()
	fmt.Println("Config: MaxOpenConns=10, MaxIdleConns=10, ConnMaxIdleTime=3s")
	fmt.Println()
	fmt.Println("Step 1: Fire 10 concurrent queries to open 10 connections.")
	fmt.Println("Step 2: Stop querying — connections return to idle pool.")
	fmt.Println("Step 3: After 3s of idleness, Go's pool manager closes them.")
	fmt.Println()

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		panic(err)
	}
	defer db.Close()

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(10)
	db.SetConnMaxIdleTime(3 * time.Second)

	runBurst(db, 10)

	fmt.Println("  Burst complete. Connections are now idle. Watching pool every second...")
	fmt.Println()

	for i := 0; i <= 8; i++ {
		s := db.Stats()
		bar := makeBar(s.Idle, 10)
		fmt.Printf("  t=+%ds  Idle=%-2d %s  MaxIdleTimeClosed=%d\n",
			i, s.Idle, bar, s.MaxIdleTimeClosed)
		time.Sleep(time.Second)
	}

	fmt.Println()
	fmt.Println("Pool Stats (final):")
	dbutil.PrintStats(db)
	fmt.Println()
	fmt.Println("Key Takeaway:")
	fmt.Println("  ConnMaxIdleTime closes connections idle longer than d.")
	fmt.Println("  MaxIdleTimeClosed tracks how many were removed this way.")
	fmt.Println("  Use this to avoid holding stale connections to a server that restarted")
	fmt.Println("  or that enforces its own idle timeout (RDS, Aurora, Cloud SQL...).")
}

// ─── Part C: BENEFIT — idle connections reduce query latency ─────────────────

func runPartC() {
	dbutil.Header("Scenario 3C: BENEFIT — Idle Connections Reduce Per-Query Latency")

	fmt.Println()
	fmt.Println("We run 20 sequential queries (SELECT 1) and measure each one.")
	fmt.Println("SELECT 1 is near-instant — the timing difference is purely from")
	fmt.Println("connection setup: TCP handshake + PostgreSQL authentication.")
	fmt.Println()
	fmt.Println("  Config A: MaxIdleConns=0 — pool CLOSES the connection after every query.")
	fmt.Println("            Every query opens a brand-new TCP connection.")
	fmt.Println()
	fmt.Println("  Config B: MaxIdleConns=1 — pool KEEPS one idle connection.")
	fmt.Println("            After the first query, the connection is reused from pool.")
	fmt.Println()

	const n = 20

	// Config A: no idle connections — reconnect every time
	fmt.Println("--- Config A: MaxIdleConns=0 (new connection every query) ---")
	dbA, err := sql.Open("postgres", dsn)
	if err != nil {
		panic(err)
	}
	dbA.SetMaxOpenConns(1)
	dbA.SetMaxIdleConns(0) // close connection after every query
	timesA := measureQueries(dbA, n, "new conn ")
	statsA := dbA.Stats()
	dbA.Close()

	fmt.Println()
	fmt.Println("--- Config B: MaxIdleConns=1 (reuse idle connection) ---")
	dbB, err := sql.Open("postgres", dsn)
	if err != nil {
		panic(err)
	}
	dbB.SetMaxOpenConns(1)
	dbB.SetMaxIdleConns(1) // keep one idle connection
	timesB := measureQueries(dbB, n, "")
	statsB := dbB.Stats()
	dbB.Close()

	// Summary
	avgA := average(timesA)
	avgB := average(timesB)

	fmt.Println()
	dbutil.Divider()
	fmt.Println("Result:")
	fmt.Printf("  Config A  avg latency: %6.2f ms  (new connection every time)\n", avgA)
	fmt.Printf("  Config B  avg latency: %6.2f ms  (reusing idle connection)\n", avgB)
	if avgB > 0 {
		fmt.Printf("  Idle pool is %.1fx faster\n", avgA/avgB)
	}
	fmt.Println()
	fmt.Println("  db.Stats() — Config A vs Config B:")
	fmt.Printf("    MaxIdleClosed  A=%-4d  B=%-4d\n", statsA.MaxIdleClosed, statsB.MaxIdleClosed)
	fmt.Printf("    (Config A discarded every connection; Config B reused them)\n")
	fmt.Println()
	fmt.Println("Key Takeaway:")
	fmt.Println("  With MaxIdleConns=0, your app pays TCP + auth overhead on EVERY query.")
	fmt.Println("  A properly sized idle pool eliminates that cost after the initial warmup.")
	fmt.Println("  For a service handling 100 req/s, the difference can be tens of seconds")
	fmt.Println("  of wasted CPU time per second just on connection setup.")
}

// measureQueries runs n sequential queries and returns the latency of each in ms.
func measureQueries(db *sql.DB, n int, firstLabel string) []float64 {
	times := make([]float64, n)
	for i := 0; i < n; i++ {
		t := time.Now()
		db.Exec("SELECT 1") //nolint:errcheck
		ms := time.Since(t).Seconds() * 1000
		times[i] = ms

		label := "reused  "
		if i == 0 {
			label = "new conn"
			if firstLabel != "" {
				label = firstLabel
			}
		}
		if firstLabel != "" {
			label = firstLabel // config A is always "new conn"
		}
		fmt.Printf("  query %2d: %6.2f ms  [%s]\n", i+1, ms, label)
	}
	return times
}

// ─── Part D: COST — too many idle connections waste server resources ──────────

func runPartD() {
	dbutil.Header("Scenario 3D: COST — Too Many Idle Connections Waste Server Resources")

	fmt.Println()
	fmt.Println("PostgreSQL uses one OS process (backend) per connection — even idle ones.")
	fmt.Println("Each idle backend:")
	fmt.Println("  - Holds a process in memory (~5–10 MB per connection)")
	fmt.Println("  - Consumes one slot from max_connections")
	fmt.Println("  - Keeps file descriptors and shared memory structures open")
	fmt.Println()
	fmt.Println("We open 30 idle connections and look at the server's own view.")
	fmt.Println()

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		panic(err)
	}
	defer db.Close()

	db.SetMaxOpenConns(30)
	db.SetMaxIdleConns(30)

	// Burst to open 30 connections, then let them sit idle
	runBurst(db, 30)
	time.Sleep(300 * time.Millisecond) // let connections return to idle state

	appStats := db.Stats()
	fmt.Printf("App pool after burst:\n")
	fmt.Printf("  Idle=%d  InUse=%d  OpenConnections=%d\n\n",
		appStats.Idle, appStats.InUse, appStats.OpenConnections)

	// Query PostgreSQL's own view of connections
	fmt.Println("PostgreSQL pg_stat_activity — the server's view:")
	fmt.Printf("  %-30s %s\n", "state", "count")
	fmt.Printf("  %-30s %s\n", "-----", "-----")

	rows, err := db.Query(`
		SELECT COALESCE(state, 'background') AS state, count(*) AS cnt
		FROM pg_stat_activity
		WHERE datname = current_database()
		GROUP BY state
		ORDER BY cnt DESC
	`)
	if err != nil {
		fmt.Printf("  pg_stat_activity query failed: %v\n", err)
	} else {
		defer rows.Close()
		for rows.Next() {
			var state string
			var cnt int
			rows.Scan(&state, &cnt) //nolint:errcheck
			marker := ""
			if state == "idle" {
				marker = " ← our idle pool connections"
			}
			if state == "active" {
				marker = " ← this pg_stat_activity query"
			}
			fmt.Printf("  %-30s %d%s\n", state, cnt, marker)
		}
	}

	// Read max_connections from the server
	var maxConn int
	db.QueryRow("SELECT setting::int FROM pg_settings WHERE name = 'max_connections'").Scan(&maxConn) //nolint:errcheck

	var usedConn int
	db.QueryRow("SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()").Scan(&usedConn) //nolint:errcheck

	fmt.Println()
	fmt.Printf("Connection slot usage on the server:\n")
	fmt.Printf("  Used      : %d\n", usedConn)
	fmt.Printf("  Max       : %d  (max_connections)\n", maxConn)
	fmt.Printf("  Remaining : %d\n", maxConn-usedConn)
	fmt.Println()

	// Scaling simulation
	idlePerInstance := 30
	fmt.Printf("Scaling simulation (each app instance holds %d idle connections):\n\n", idlePerInstance)
	fmt.Printf("  %-12s  %-20s  %-20s  %s\n", "Instances", "Total idle conns", "% of max_connections", "Slots left for queries")
	fmt.Printf("  %-12s  %-20s  %-20s  %s\n", "---------", "----------------", "--------------------", "---------------------")

	for _, instances := range []int{1, 3, 5, 10} {
		total := instances * idlePerInstance
		pct := float64(total) / float64(maxConn) * 100
		remaining := maxConn - total
		warning := ""
		if remaining <= 0 {
			warning = " ← SERVER FULL"
		} else if pct > 75 {
			warning = " ← dangerously high"
		}
		fmt.Printf("  %-12d  %-20d  %-19.0f%%  %d%s\n", instances, total, pct, remaining, warning)
	}

	fmt.Println()
	fmt.Println("Pool Stats (app side):")
	dbutil.PrintStats(db)
	fmt.Println()
	fmt.Println("Key Takeaway:")
	fmt.Println("  Idle connections look free to your app but cost the server memory and slots.")
	fmt.Println("  The right MaxIdleConns = your typical concurrent requests per instance,")
	fmt.Println("  NOT 'as high as possible'.")
	fmt.Println()
	fmt.Println("  Good rule of thumb:")
	fmt.Printf("    MaxOpenConns = max_connections / number_of_app_instances\n")
	fmt.Printf("    MaxIdleConns = typical_concurrent_requests_per_instance\n")
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// runBurst fires n concurrent goroutines each running a short query (0.5s)
// to force the pool to open n simultaneous connections.
func runBurst(db *sql.DB, n int) {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db.Exec("SELECT pg_sleep(0.5)") //nolint:errcheck
		}()
	}
	wg.Wait()
}

func average(ms []float64) float64 {
	if len(ms) == 0 {
		return 0
	}
	sorted := make([]float64, len(ms))
	copy(sorted, ms)
	sort.Float64s(sorted)
	// Use median-based average: drop the top 10% to avoid outliers
	cutoff := len(sorted) * 9 / 10
	sum := 0.0
	for _, v := range sorted[:cutoff] {
		sum += v
	}
	return sum / float64(cutoff)
}

func makeBar(n, max int) string {
	s := ""
	for i := 0; i < max; i++ {
		if i < n {
			s += "#"
		} else {
			s += "."
		}
	}
	return "[" + s + "]"
}
