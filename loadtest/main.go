// Load test for PostgreSQL connection pool tuning.
//
// Usage:
//   go run ./loadtest [flags]
//
// Flags:
//   -pool    int    SetMaxOpenConns  (default 5)
//   -idle    int    SetMaxIdleConns  (default same as pool)
//   -workers int    concurrent goroutines sending queries (default 20)
//   -dur     dur    test duration (default 15s)
//   -qtime   float  simulated query time via pg_sleep (default 0.1s)
//   -db      string DSN (default: postgres-normal on port 5432)
//
// Suggested experiments:
//
//  1. Baseline (pool == workers, no contention):
//     go run ./loadtest -pool 20 -workers 20 -qtime 0.1
//
//  2. Pool too small (degradation — high WaitDuration):
//     go run ./loadtest -pool 3 -workers 20 -qtime 0.1
//
//  3. Pool too small + short queries (pool recycled fast, modest wait):
//     go run ./loadtest -pool 3 -workers 20 -qtime 0.01
//
//  4. Server limit hit (use postgres-limited on port 5433):
//     go run ./loadtest -pool 20 -workers 20 -qtime 0.5 -db "host=localhost port=5441 dbname=demo user=demo password=demo sslmode=disable"
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/lib/pq"
)

func main() {
	poolSize := flag.Int("pool", 5, "SetMaxOpenConns — max open connections in pool")
	idleSize := flag.Int("idle", -1, "SetMaxIdleConns (-1 = same as pool)")
	workers := flag.Int("workers", 20, "concurrent goroutines")
	dur := flag.Duration("dur", 15*time.Second, "test duration")
	qtime := flag.Float64("qtime", 0.1, "simulated query duration (pg_sleep, seconds)")
	dsn := flag.String("db", "host=localhost port=5440 dbname=demo user=demo password=demo sslmode=disable", "PostgreSQL DSN")
	flag.Parse()

	if *idleSize == -1 {
		*idleSize = *poolSize
	}

	db, err := sql.Open("postgres", *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sql.Open: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	db.SetMaxOpenConns(*poolSize)
	db.SetMaxIdleConns(*idleSize)

	if err := db.Ping(); err != nil {
		fmt.Fprintf(os.Stderr, "Cannot connect: %v\n", err)
		os.Exit(1)
	}

	printHeader(*poolSize, *idleSize, *workers, *dur, *qtime)

	// Counters
	var totalOps, totalErrors int64
	var latenciesMu sync.Mutex
	var latencies []float64

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Launch workers
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			query := fmt.Sprintf("SELECT pg_sleep(%g)", *qtime)
			for {
				select {
				case <-stop:
					return
				default:
				}
				t := time.Now()
				_, err := db.Exec(query)
				ms := time.Since(t).Seconds() * 1000

				if err != nil {
					atomic.AddInt64(&totalErrors, 1)
				} else {
					atomic.AddInt64(&totalOps, 1)
					latenciesMu.Lock()
					latencies = append(latencies, ms)
					latenciesMu.Unlock()
				}
			}
		}()
	}

	// Print stats every second
	ticker := time.NewTicker(time.Second)
	testStart := time.Now()
	prevOps := int64(0)
	prevErrors := int64(0)

	fmt.Printf("\n%-5s %-8s %-8s %-6s %-6s %-6s %-8s %-8s %-8s\n",
		"t(s)", "RPS", "Errors", "Open", "InUse", "Idle", "WaitCnt", "WaitMs", "MaxIdleClsd")
	fmt.Println("-----------------------------------------------------------------------")

	for elapsed := time.Duration(0); elapsed < *dur; elapsed = time.Since(testStart) {
		<-ticker.C

		ops := atomic.LoadInt64(&totalOps)
		errs := atomic.LoadInt64(&totalErrors)
		rps := ops - prevOps
		errDelta := errs - prevErrors
		prevOps = ops
		prevErrors = errs

		s := db.Stats()
		fmt.Printf("%-5.0f %-8d %-8d %-6d %-6d %-6d %-8d %-8.1f %-8d\n",
			elapsed.Seconds(),
			rps,
			errDelta,
			s.OpenConnections,
			s.InUse,
			s.Idle,
			s.WaitCount,
			float64(s.WaitDuration.Milliseconds()),
			s.MaxIdleClosed,
		)
	}

	close(stop)
	wg.Wait()
	ticker.Stop()

	// Final report
	printReport(db, totalOps, totalErrors, latencies, *dur)
}

func printHeader(pool, idle, workers int, dur time.Duration, qtime float64) {
	fmt.Println()
	fmt.Println("==============================================================")
	fmt.Println("  PostgreSQL Connection Pool Load Test")
	fmt.Println("==============================================================")
	fmt.Printf("  SetMaxOpenConns : %d\n", pool)
	fmt.Printf("  SetMaxIdleConns : %d\n", idle)
	fmt.Printf("  Workers         : %d\n", workers)
	fmt.Printf("  Duration        : %v\n", dur)
	fmt.Printf("  Query time      : %gs  (pg_sleep)\n", qtime)
	fmt.Println()
	fmt.Println("Theoretical max throughput:")
	maxThroughput := float64(pool) / qtime
	fmt.Printf("  pool / qtime = %d / %g = %.0f RPS\n", pool, qtime, maxThroughput)
	if float64(workers)*qtime > float64(pool)*qtime {
		fmt.Printf("  Workers (%d) > Pool (%d): expect queuing and elevated WaitDuration\n", workers, pool)
	}
}

func printReport(db *sql.DB, ops, errs int64, latencies []float64, dur time.Duration) {
	fmt.Println()
	fmt.Println("==============================================================")
	fmt.Println("  Final Report")
	fmt.Println("==============================================================")
	fmt.Printf("  Total successful ops : %d\n", ops)
	fmt.Printf("  Total errors         : %d\n", errs)
	fmt.Printf("  Avg RPS              : %.1f\n", float64(ops)/dur.Seconds())

	if len(latencies) > 0 {
		sort.Float64s(latencies)
		fmt.Printf("  Latency p50          : %.1f ms\n", percentile(latencies, 50))
		fmt.Printf("  Latency p95          : %.1f ms\n", percentile(latencies, 95))
		fmt.Printf("  Latency p99          : %.1f ms\n", percentile(latencies, 99))
		fmt.Printf("  Latency max          : %.1f ms\n", latencies[len(latencies)-1])
	}

	fmt.Println()
	fmt.Println("  Final Pool Stats:")
	s := db.Stats()
	fmt.Printf("    WaitCount        : %d (total goroutines that queued)\n", s.WaitCount)
	fmt.Printf("    WaitDuration     : %v (total time spent waiting)\n", s.WaitDuration)
	fmt.Printf("    MaxIdleClosed    : %d\n", s.MaxIdleClosed)
	fmt.Printf("    MaxLifetimeClosed: %d\n", s.MaxLifetimeClosed)
	fmt.Println()

	if s.WaitCount > 0 {
		avgWaitMs := float64(s.WaitDuration.Milliseconds()) / float64(s.WaitCount)
		fmt.Printf("  Avg wait per queued goroutine: %.1f ms\n", avgWaitMs)
		fmt.Println()
		fmt.Println("  >> Pool contention detected.")
		fmt.Println("     Consider increasing SetMaxOpenConns or reducing concurrent workers.")
	} else {
		fmt.Println("  >> No pool contention. Pool size was sufficient for the workload.")
	}
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
