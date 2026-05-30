// Scenario 4: Timeout parameters — five distinct timeout mechanisms.
//
// 4A connect_timeout (DSN):
//   Limits how long the TCP+auth handshake may take.
//   If the server is unreachable, the driver fails after N seconds
//   instead of hanging indefinitely.
//
// 4B Context query timeout (Go side):
//   context.WithTimeout wraps any db call.
//   The pool respects the context both while waiting for a free connection
//   AND while the query is executing.
//
// 4C PostgreSQL statement_timeout (server side):
//   SET statement_timeout = '2s' tells PostgreSQL to abort any query
//   that runs longer than 2 seconds.
//   Error: "ERROR: canceling statement due to statement timeout"
//
// 4D db.SetConnMaxLifetime:
//   Connections are closed and recreated after this duration, even if healthy.
//   Visible via db.Stats().MaxLifetimeClosed.
//
// 4E db.SetConnMaxIdleTime:
//   Connections that sit idle longer than this are closed.
//   Visible via db.Stats().MaxIdleTimeClosed.
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

const normalDSN = "host=localhost port=5440 dbname=demo user=demo password=demo sslmode=disable"

func main() {
	demo4A()
	demo4B()
	demo4C()
	demo4D()
	demo4E()
}

// ─── 4A: connect_timeout ────────────────────────────────────────────────────

func demo4A() {
	dbutil.Header("4A: connect_timeout in DSN")

	fmt.Println()
	fmt.Println("Scenario: try to open a connection to an IP that never responds.")
	fmt.Println("  Without connect_timeout: hangs for OS default (~2 min).")
	fmt.Println("  With    connect_timeout=2: returns an error after ~2 seconds.")
	fmt.Println()

	// 192.0.2.1 is TEST-NET (RFC 5737) — routable but no host answers, causing a real timeout.
	badDSN := "host=192.0.2.1 port=5440 dbname=demo user=demo password=demo sslmode=disable connect_timeout=2"

	fmt.Println("Connecting to 192.0.2.1 (TEST-NET, RFC 5737 — unreachable) with connect_timeout=2...")
	start := time.Now()
	db, _ := sql.Open("postgres", badDSN)
	defer db.Close()

	err := db.Ping()
	elapsed := time.Since(start)

	if err != nil {
		fmt.Printf("  Error after %.1fs: %v\n", elapsed.Seconds(), err)
		fmt.Println("  -> connect_timeout=2 fired — prevented an indefinite hang.")
	} else {
		fmt.Println("  Ping succeeded (unexpected for a black-hole address).")
	}

	fmt.Println()
	fmt.Println("Key Takeaway:")
	fmt.Println("  Set connect_timeout in the DSN to bound the dial phase.")
	fmt.Println(`  URL format:     postgres://user:pass@host/db?connect_timeout=5`)
	fmt.Println(`  Keyword format: host=... connect_timeout=5   (unit is seconds, integer only)`)
}

// ─── 4B: context query timeout ──────────────────────────────────────────────

func demo4B() {
	dbutil.Header("4B: context.WithTimeout for query deadline (Go side)")

	fmt.Println()
	fmt.Println("Run a 5-second query with a 2-second context deadline.")
	fmt.Println("Go cancels the query at the context deadline.")
	fmt.Println()

	db, err := dbutil.OpenDB(normalDSN)
	if err != nil {
		fmt.Printf("Cannot connect: %v\n", err)
		return
	}
	defer db.Close()

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	fmt.Println("Running: SELECT pg_sleep(5)  [with 2s deadline]")
	_, err = db.ExecContext(ctx, "SELECT pg_sleep(5)")
	elapsed := time.Since(start)

	if err != nil {
		fmt.Printf("  Error after %.2fs: %v\n", elapsed.Seconds(), err)
	} else {
		fmt.Printf("  Unexpected success after %.2fs\n", elapsed.Seconds())
	}

	fmt.Println()
	fmt.Println("Key Takeaway:")
	fmt.Println("  Always pass a context with a deadline to QueryContext / ExecContext.")
	fmt.Println("  Go driver sends a query cancel signal to PostgreSQL when the context expires.")
	fmt.Println("  The deadline covers BOTH waiting for a free pool slot AND query execution.")
}

// ─── 4C: PostgreSQL statement_timeout ───────────────────────────────────────

func demo4C() {
	dbutil.Header("4C: PostgreSQL statement_timeout (server-side enforcement)")

	fmt.Println()
	fmt.Println("SET statement_timeout = '2s' in the session.")
	fmt.Println("Then run a 5-second query. PostgreSQL cancels it server-side.")
	fmt.Println()

	db, err := dbutil.OpenDB(normalDSN)
	if err != nil {
		fmt.Printf("Cannot connect: %v\n", err)
		return
	}
	defer db.Close()

	if _, err := db.Exec("SET statement_timeout = '2s'"); err != nil {
		fmt.Printf("SET statement_timeout failed: %v\n", err)
		return
	}
	fmt.Println("Session: SET statement_timeout = '2s'")

	start := time.Now()
	fmt.Println("Running: SELECT pg_sleep(5)")
	_, err = db.Exec("SELECT pg_sleep(5)")
	elapsed := time.Since(start)

	if err != nil {
		fmt.Printf("  Error after %.2fs: %v\n", elapsed.Seconds(), err)
		fmt.Println("  -> PostgreSQL killed the query server-side.")
	} else {
		fmt.Printf("  Unexpected success after %.2fs\n", elapsed.Seconds())
	}

	fmt.Println()
	fmt.Println("Key Takeaway:")
	fmt.Println("  statement_timeout is enforced by PostgreSQL regardless of Go context.")
	fmt.Println("  Defense-in-depth: even if Go code forgets to set a deadline,")
	fmt.Println("  runaway queries are still bounded.")
	fmt.Println("  Set it globally:   postgresql.conf  statement_timeout = '10s'")
	fmt.Println("  Set it per-role:   ALTER ROLE app SET statement_timeout = '5s'")
	fmt.Println(`  Set it in DSN:     "host=... options=-c statement_timeout=5000"`)
}

// ─── 4D: ConnMaxLifetime ─────────────────────────────────────────────────────

func demo4D() {
	dbutil.Header("4D: db.SetConnMaxLifetime — recycle long-lived connections")

	fmt.Println()
	fmt.Println("Config: MaxOpenConns=5, ConnMaxLifetime=3s")
	fmt.Println("We keep the pool warm and watch MaxLifetimeClosed increase.")
	fmt.Println()

	db, err := sql.Open("postgres", normalDSN)
	if err != nil {
		panic(err)
	}
	defer db.Close()

	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(3 * time.Second)

	// Warm the pool
	for i := 0; i < 5; i++ {
		db.Ping() //nolint:errcheck
	}

	fmt.Println("Polling db.Stats() every second for 10 seconds...")
	fmt.Println()

	for i := 0; i <= 10; i++ {
		s := db.Stats()
		fmt.Printf("  t=+%2ds  Open=%-2d  InUse=%-2d  Idle=%-2d  MaxLifetimeClosed=%d\n",
			i, s.OpenConnections, s.InUse, s.Idle, s.MaxLifetimeClosed)
		go db.Exec("SELECT 1") //nolint:errcheck
		time.Sleep(time.Second)
	}

	fmt.Println()
	fmt.Println("Key Takeaway:")
	fmt.Println("  SetConnMaxLifetime(d) forces connections to be closed after d, even if healthy.")
	fmt.Println("  MaxLifetimeClosed tracks recycled connections.")
	fmt.Println("  Use case: TLS cert rotation, or avoiding LB sticky-session issues.")
	fmt.Println("  Tip: set it slightly less than the server's own idle timeout to avoid")
	fmt.Println("  'broken pipe' errors when the server closes the connection first.")
}

// ─── 4E: ConnMaxIdleTime ─────────────────────────────────────────────────────

func demo4E() {
	dbutil.Header("4E: db.SetConnMaxIdleTime — close stale idle connections")

	fmt.Println()
	fmt.Println("Config: MaxOpenConns=5, MaxIdleConns=5, ConnMaxIdleTime=3s")
	fmt.Println()
	fmt.Println("Step 1: Open 5 connections via a burst of queries.")
	fmt.Println("Step 2: Stop querying — connections become idle.")
	fmt.Println("Step 3: After 3s of idleness, Go's pool manager closes them.")
	fmt.Println()

	db, err := sql.Open("postgres", normalDSN)
	if err != nil {
		panic(err)
	}
	defer db.Close()

	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(5)
	db.SetConnMaxIdleTime(3 * time.Second)

	// Burst to open 5 connections
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db.Exec("SELECT pg_sleep(0.5)") //nolint:errcheck
		}()
	}
	wg.Wait()
	fmt.Println("Burst complete. All 5 connections are now idle.")
	fmt.Println()

	for i := 0; i <= 8; i++ {
		s := db.Stats()
		bar := makeBar(s.Idle, 5)
		fmt.Printf("  t=+%ds  Idle=%-2d %s  MaxIdleTimeClosed=%d\n",
			i, s.Idle, bar, s.MaxIdleTimeClosed)
		time.Sleep(time.Second)
	}

	fmt.Println()
	fmt.Println("Key Takeaway:")
	fmt.Println("  SetConnMaxIdleTime(d) closes connections idle for longer than d.")
	fmt.Println("  MaxIdleTimeClosed tracks how many were removed this way.")
	fmt.Println("  Use case: avoid holding connections to managed databases (RDS/Aurora)")
	fmt.Println("  that close idle connections on their own after a few minutes.")
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
