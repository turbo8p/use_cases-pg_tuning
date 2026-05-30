// Scenario 2: Application-level pool exhaustion.
//
// The server has max_connections=200 (plenty of room).
// We limit the app pool to SetMaxOpenConns(3).
// We launch 9 goroutines each running a 2-second query.
//
// Part A — No timeout (Degradation):
//   All 9 goroutines eventually succeed, but they queue up in batches of 3.
//   Total wall time ≈ 3 × 2s = 6s instead of 2s.
//   WaitCount and WaitDuration in db.Stats() show the contention.
//
// Part B — Context timeout (Error):
//   Same setup but each goroutine has a 3.5s context deadline.
//   Workers 1-3 get connections immediately, finish at t≈2s (SUCCESS).
//   Workers 4-9 must wait until t≈2s for connections; then need 2s more,
//   but deadline expires at t=3.5s → context deadline exceeded (ERROR).
package main

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"pg-tuning/internal/dbutil"

	_ "github.com/lib/pq"
)

const dsn = "host=localhost port=5440 dbname=demo user=demo password=demo sslmode=disable"

const (
	poolSize   = 3
	numWorkers = 9
	queryTime  = 2 // seconds (pg_sleep argument)
)

func main() {
	runPartA()
	runPartB()
}

func runPartA() {
	dbutil.Header("Scenario 2A: Pool Exhaustion → DEGRADATION (no timeout)")

	fmt.Println()
	fmt.Printf("Config: SetMaxOpenConns=%d  |  workers=%d  |  query=%ds  |  no timeout\n\n",
		poolSize, numWorkers, queryTime)
	fmt.Println("Expected behaviour:")
	fmt.Printf("  Workers run in batches of %d. Total time ≈ %d × %ds = %ds.\n",
		poolSize, (numWorkers+poolSize-1)/poolSize, queryTime, ((numWorkers+poolSize-1)/poolSize)*queryTime)
	fmt.Println("  Requests are slow (degraded), but they all eventually succeed.")
	fmt.Println()

	db := mustOpenDB(poolSize)
	defer db.Close()

	done := make(chan struct{})
	// Print live pool stats every 500ms while workers run
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(500 * time.Millisecond):
				s := db.Stats()
				fmt.Printf("  [stats] InUse=%-2d Idle=%-2d WaitCount=%-2d WaitDuration=%v\n",
					s.InUse, s.Idle, s.WaitCount, s.WaitDuration.Round(time.Millisecond))
			}
		}
	}()

	type result struct {
		workerID int
		ok       bool
		waitedMS int64
		elapsed  time.Duration
	}

	results := make(chan result, numWorkers)
	var wg sync.WaitGroup
	start := time.Now()

	for i := 1; i <= numWorkers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			t := time.Now()
			query := fmt.Sprintf("SELECT pg_sleep(%d)", queryTime)
			_, err := db.Exec(query)
			elapsed := time.Since(t)
			results <- result{
				workerID: id,
				ok:       err == nil,
				elapsed:  elapsed,
			}
		}(i)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var res []result
	for r := range results {
		res = append(res, r)
	}
	close(done) // stop stats goroutine

	// Sort results by worker ID for clean output
	for _, r := range res {
		status := "OK  "
		if !r.ok {
			status = "FAIL"
		}
		fmt.Printf("  [Worker %2d] %s  elapsed=%.2fs\n", r.workerID, status, r.elapsed.Seconds())
	}

	fmt.Println()
	dbutil.Divider()
	fmt.Printf("Total wall time: %.2fs  (with pool=3 and query=2s, expected ~%ds)\n",
		time.Since(start).Seconds(), ((numWorkers+poolSize-1)/poolSize)*queryTime)
	fmt.Println()
	fmt.Println("Pool Stats (final):")
	dbutil.PrintStats(db)
	fmt.Println()
	fmt.Println("Key Takeaway:")
	fmt.Println("  WaitCount shows how many goroutines had to wait for a free connection.")
	fmt.Println("  WaitDuration is the total time all goroutines spent blocked.")
	fmt.Println("  The app is alive but SLOW — this is connection pool degradation.")
}

func runPartB() {
	dbutil.Header("Scenario 2B: Pool Exhaustion → ERROR (3.5s context deadline)")

	fmt.Println()
	fmt.Printf("Config: SetMaxOpenConns=%d  |  workers=%d  |  query=%ds  |  deadline=3.5s\n\n",
		poolSize, numWorkers, queryTime)
	fmt.Println("Expected behaviour:")
	fmt.Println("  Workers 1-3  : get connections immediately, finish at t≈2s  → SUCCESS")
	fmt.Println("  Workers 4-9  : wait until t≈2s, then need 2s more query time,")
	fmt.Println("                 but deadline is t=3.5s → context deadline exceeded → ERROR")
	fmt.Println()

	db := mustOpenDB(poolSize)
	defer db.Close()

	type result struct {
		workerID int
		ok       bool
		err      error
		elapsed  time.Duration
	}

	results := make(chan result, numWorkers)
	var wg sync.WaitGroup
	start := time.Now()

	for i := 1; i <= numWorkers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			// Each goroutine gets its own deadline from the shared start time
			ctx, cancel := context.WithDeadline(context.Background(), start.Add(3500*time.Millisecond))
			defer cancel()

			t := time.Now()
			query := fmt.Sprintf("SELECT pg_sleep(%d)", queryTime)
			_, err := db.ExecContext(ctx, query)
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
			fmt.Printf("  [Worker %2d] OK     elapsed=%.2fs\n", r.workerID, r.elapsed.Seconds())
		} else {
			errorCount++
			fmt.Printf("  [Worker %2d] ERROR  elapsed=%.2fs  err=%v\n", r.workerID, r.elapsed.Seconds(), r.err)
		}
	}

	fmt.Println()
	dbutil.Divider()
	fmt.Printf("Results: %d succeeded, %d failed\n", successCount, errorCount)
	fmt.Println()
	fmt.Println("Pool Stats (final):")
	dbutil.PrintStats(db)
	fmt.Println()
	fmt.Println("Key Takeaway:")
	fmt.Println("  'context deadline exceeded' fires when the goroutine EITHER:")
	fmt.Println("    (a) waits too long for a connection from the pool, OR")
	fmt.Println("    (b) holds a connection but the query takes too long.")
	fmt.Println("  Context deadlines protect callers from unbounded waits.")
	fmt.Println("  Set SetMaxOpenConns higher (or use a larger pool) to let more workers proceed.")
}

func mustOpenDB(maxOpen int) *sql.DB {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		panic(err)
	}
	if err := db.Ping(); err != nil {
		panic(fmt.Sprintf("Cannot connect to postgres-normal (port 5432): %v\n  -> make up", err))
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxOpen)
	return db
}
