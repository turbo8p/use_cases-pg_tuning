// Scenario 1: PostgreSQL server-level max_connections limit.
//
// The postgres-limited container is configured with max_connections=10.
// We set the application pool limit very high so the SERVER is the only bottleneck.
// We spawn 15 goroutines each holding a connection open with pg_sleep(3).
//
// Expected result: up to 10 succeed, the rest fail with
//   "pq: sorry, too many clients already"
package main

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	"pg-tuning/internal/dbutil"

	_ "github.com/lib/pq"
)

// postgres-limited: max_connections=10
const dsn = "host=localhost port=5441 dbname=demo user=demo password=demo sslmode=disable"

const numWorkers = 15

func main() {
	dbutil.Header("Scenario 1: PostgreSQL Server max_connections=10 vs 15 workers")

	fmt.Println()
	fmt.Println("Config:")
	fmt.Println("  postgres max_connections = 10   (server-side hard limit)")
	fmt.Println("  app SetMaxOpenConns      = 0    (unlimited — server is the only constraint)")
	fmt.Printf("  concurrent workers       = %d\n", numWorkers)
	fmt.Println()

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		fmt.Printf("sql.Open failed: %v\n", err)
		return
	}
	defer db.Close()

	// Remove all app-level limits so only the PostgreSQL server limits us.
	db.SetMaxOpenConns(0) // 0 = unlimited
	db.SetMaxIdleConns(0)

	// Verify connection before starting
	if err := db.Ping(); err != nil {
		fmt.Printf("Cannot reach postgres-limited (port 5433): %v\n", err)
		fmt.Println("  -> Make sure containers are running: make up")
		return
	}

	type result struct {
		workerID int
		ok       bool
		err      error
		elapsed  time.Duration
	}

	results := make(chan result, numWorkers)
	var wg sync.WaitGroup

	start := time.Now()
	fmt.Printf("Launching %d goroutines simultaneously...\n\n", numWorkers)

	for i := 1; i <= numWorkers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			t := time.Now()
			// pg_sleep(3) keeps the connection open for 3 seconds,
			// forcing all goroutines to need simultaneous connections.
			_, err := db.Exec("SELECT pg_sleep(3)")
			results <- result{
				workerID: id,
				ok:       err == nil,
				err:      err,
				elapsed:  time.Since(t),
			}
		}(i)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	successCount, errorCount := 0, 0
	for r := range results {
		if r.ok {
			successCount++
			fmt.Printf("  [Worker %2d] OK     finished in %.1fs\n", r.workerID, r.elapsed.Seconds())
		} else {
			errorCount++
			fmt.Printf("  [Worker %2d] ERROR  %v\n", r.workerID, r.err)
		}
	}

	fmt.Println()
	dbutil.Divider()
	fmt.Printf("Results: %d succeeded, %d failed (total time: %.1fs)\n",
		successCount, errorCount, time.Since(start).Seconds())
	fmt.Println()
	fmt.Println("Pool Stats (after all workers finished):")
	dbutil.PrintStats(db)
	fmt.Println()
	fmt.Println("Key Takeaway:")
	fmt.Println("  PostgreSQL rejected connections beyond max_connections=10.")
	fmt.Println("  The error 'sorry, too many clients already' is a HARD server-level rejection.")
	fmt.Println("  Fix: increase max_connections in postgresql.conf (requires restart)")
	fmt.Println("       or reduce total concurrent connections across all app instances.")
}
