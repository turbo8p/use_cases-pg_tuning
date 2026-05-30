// Scenario 3: MaxIdleConns and ConnMaxIdleTime.
//
// Part A — Effect of MaxIdleConns on idle pool size:
//   Run 10 concurrent queries, then compare Idle count and MaxIdleClosed
//   between MaxIdleConns=2 and MaxIdleConns=10.
//
// Part B — ConnMaxIdleTime: idle connections are closed after a timeout:
//   Open 10 connections, set ConnMaxIdleTime=3s, watch Idle count drop
//   via db.Stats() as connections expire in the background.
//
// Stats explained:
//   db.Stats().Idle            — connections currently sitting in the pool (not in use)
//   db.Stats().MaxIdleClosed   — cumulative count closed because pool was full (> MaxIdleConns)
//   db.Stats().MaxIdleTimeClosed — cumulative count closed because they sat idle too long
package main

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	"pg-tuning/internal/dbutil"

	_ "github.com/lib/pq"
)

const dsn = "host=localhost port=5440 dbname=demo user=demo password=demo sslmode=disable"

const burstWorkers = 10

func main() {
	runPartA()
	runPartB()
}

// runPartA compares low vs high MaxIdleConns after a burst of queries.
func runPartA() {
	dbutil.Header("Scenario 3A: MaxIdleConns effect on idle pool size")

	fmt.Println()
	fmt.Printf("We fire %d concurrent queries, then inspect db.Stats()\n\n", burstWorkers)

	configs := []struct {
		label       string
		maxOpen     int
		maxIdle     int
		expectIdle  string
		expectClosed string
	}{
		{
			label:        "MaxIdleConns=2 (small idle pool)",
			maxOpen:      burstWorkers,
			maxIdle:      2,
			expectIdle:   "2",
			expectClosed: "~8 (excess connections discarded)",
		},
		{
			label:        "MaxIdleConns=10 (idle pool == burst size)",
			maxOpen:      burstWorkers,
			maxIdle:      burstWorkers,
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

		runBurst(db)

		// Give the pool a moment to settle (return connections to idle state)
		time.Sleep(200 * time.Millisecond)

		fmt.Println("  db.Stats() after burst:")
		dbutil.PrintStats(db)
		db.Close()
		fmt.Println()
	}

	fmt.Println("Key Takeaway:")
	fmt.Println("  MaxIdleConns caps how many connections stay in the pool after use.")
	fmt.Println("  Extra connections are CLOSED immediately, not recycled.")
	fmt.Println("  MaxIdleClosed counts how many were discarded this way.")
	fmt.Println("  Implication: if your burst size >> MaxIdleConns, the next burst")
	fmt.Println("  re-creates all those connections from scratch (TCP + TLS + auth).")
}

// runPartB shows ConnMaxIdleTime closing idle connections over time.
func runPartB() {
	dbutil.Header("Scenario 3B: ConnMaxIdleTime — idle connections expire")

	fmt.Println()
	fmt.Println("Config: MaxOpenConns=10, MaxIdleConns=10, ConnMaxIdleTime=3s")
	fmt.Println()
	fmt.Printf("Step 1: Fire %d concurrent queries to open 10 connections.\n", burstWorkers)
	fmt.Println("Step 2: Let them all finish — connections return to idle pool.")
	fmt.Println("Step 3: Watch db.Stats() every second. After 3s, Go closes them.")
	fmt.Println()

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		panic(err)
	}
	defer db.Close()

	db.SetMaxOpenConns(burstWorkers)
	db.SetMaxIdleConns(burstWorkers)
	db.SetConnMaxIdleTime(3 * time.Second) // idle connections closed after 3s

	runBurst(db)

	fmt.Println("  Burst complete. Connections are now idle. Watching pool every second...")

	// Poll db.Stats() for 8 seconds to observe connection expiry
	for i := 0; i <= 8; i++ {
		s := db.Stats()
		bar := makeBar(s.Idle, burstWorkers)
		fmt.Printf("  t=+%ds  Idle=%-2d %s  MaxIdleTimeClosed=%d\n",
			i, s.Idle, bar, s.MaxIdleTimeClosed)
		time.Sleep(time.Second)
	}

	fmt.Println()
	fmt.Println("Pool Stats (final):")
	dbutil.PrintStats(db)
	fmt.Println()
	fmt.Println("Key Takeaway:")
	fmt.Println("  ConnMaxIdleTime closes connections that have been idle too long.")
	fmt.Println("  MaxIdleTimeClosed counts how many were closed this way.")
	fmt.Println("  Use this to avoid holding stale connections to a server that may")
	fmt.Println("  have restarted or enforces its own idle timeouts.")
}

// runBurst fires burstWorkers concurrent fast queries to open maxOpen connections.
func runBurst(db *sql.DB) {
	var wg sync.WaitGroup
	for i := 0; i < burstWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Short sleep so all goroutines need concurrent connections
			db.Exec("SELECT pg_sleep(0.5)") //nolint:errcheck
		}()
	}
	wg.Wait()
}

func makeBar(n, max int) string {
	bar := ""
	for i := 0; i < max; i++ {
		if i < n {
			bar += "#"
		} else {
			bar += "."
		}
	}
	return "[" + bar + "]"
}
