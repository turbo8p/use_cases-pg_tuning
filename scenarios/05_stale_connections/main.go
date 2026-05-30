// Scenario 5: Stale connections — "connection reset by peer".
//
// This is a real production problem. It appears when ANYTHING between your
// app and PostgreSQL closes idle TCP connections without telling Go's pool.
//
// Common culprits and their default idle timeouts:
//
//   Network intermediary          Type                 Default idle timeout
//   ─────────────────────────────────────────────────────────────────────
//   AWS Application Load Balancer  Load balancer        60 s
//   Azure SQL Gateway             Database gateway     30 s  (hard limit)
//   GCP Cloud SQL Auth Proxy      Database proxy       600 s
//   AWS RDS Proxy                 Database proxy       configurable
//   PgBouncer                     Connection pooler    configurable
//   Corporate / cloud firewall    Stateful firewall    300–600 s
//   AWS NAT Gateway               NAT                  350 s
//   PostgreSQL idle_session_timeout (PG 14+)           disabled by default
//
// These are all different products, but they share one behaviour:
//   they DROP idle TCP connections after their timeout without sending any
//   notification to the Go application.
//
// Go's pool does not know the connection was dropped.
// The next query picks up the dead socket and tries to write on it.
// TCP RST comes back → "read tcp ...: connection reset by peer" (ECONNRESET).
//
// database/sql has a built-in single retry: it maps ECONNRESET to
// driver.ErrBadConn, discards the socket, and retries on a new connection —
// silently hiding the error. Part A uses db.Conn() to pin one physical
// socket so the retry cannot fire, making the error visible. This matches
// the real production pattern: transactions and prepared statements are also
// pinned to one connection and cannot be silently retried.
//
// ─────────────────────────────────────────────────────────────────────────────
// How this demo works
// ─────────────────────────────────────────────────────────────────────────────
//
// We run a local TCP proxy that forwards bytes between Go and PostgreSQL.
// The proxy is NOT a load balancer — it just simulates the one behaviour
// that all the intermediaries above share: dropping idle TCP connections.
//
//   [Go app] ──► [TCP proxy :5442] ──► [PostgreSQL :5440]
//
// After 3 seconds of idle time the proxy calls SetLinger(0) + Close()
// on the client-facing socket. SetLinger(0) sends TCP RST instead of FIN,
// giving the Go side the authentic "connection reset by peer" error.
//
// Part A — The problem: no lifetime limits → stale connection → error.
// Part B — The fix: ConnMaxIdleTime shorter than the proxy's idle timeout.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"sync"
	"time"

	"pg-tuning/internal/dbutil"

	_ "github.com/lib/pq"
)

// ─── constants ───────────────────────────────────────────────────────────────

const (
	proxyPort = 5442
	pgAddr    = "127.0.0.1:5440"
	proxyDSN  = "host=127.0.0.1 port=5442 dbname=demo user=demo password=demo sslmode=disable"

	// proxyIdleTimeout: how long the proxy waits before dropping an idle connection.
	// This simulates the idle timeout of whatever sits between your app and the DB.
	// Real values: AWS Application Load Balancer=60s, Azure SQL Gateway=30s, GCP Cloud SQL Proxy=600s.
	// We use 3s so the demo runs quickly.
	proxyIdleTimeout = 3 * time.Second
)

// ─────────────────────────────────────────────────────────────────────────────
// TCP proxy — simulates any intermediary with an idle-connection timeout
// ─────────────────────────────────────────────────────────────────────────────

type tcpProxy struct {
	listener    net.Listener
	idleTimeout time.Duration
	forceClosed int // connections RST-closed due to idle timeout (protected by mu)
	mu          sync.Mutex
}

func newProxy() *tcpProxy {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort))
	if err != nil {
		panic(fmt.Sprintf("proxy: cannot listen on port %d: %v", proxyPort, err))
	}
	p := &tcpProxy{listener: ln, idleTimeout: proxyIdleTimeout}
	go p.serve()
	return p
}

func (p *tcpProxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return // listener closed
		}
		go p.handleConn(client)
	}
}

func (p *tcpProxy) handleConn(client net.Conn) {
	server, err := net.Dial("tcp", pgAddr)
	if err != nil {
		client.Close()
		return
	}

	var (
		mu           sync.Mutex
		lastActivity = time.Now()
		done         bool
	)

	// closeAll shuts both sides down exactly once.
	// When cause=="idle" it sends TCP RST (SetLinger(0)) to the Go app,
	// producing the authentic "connection reset by peer" error.
	closeAll := func(cause string) {
		mu.Lock()
		defer mu.Unlock()
		if done {
			return
		}
		done = true
		if cause == "idle" {
			p.mu.Lock()
			p.forceClosed++
			p.mu.Unlock()
			fmt.Printf("  [proxy %s] idle %v reached → RST-closing (intermediary timeout)\n",
				now(), p.idleTimeout)
			// SetLinger(0) makes Close() send RST instead of the default FIN.
			// FIN = graceful close; RST = abrupt drop → "connection reset by peer".
			if tc, ok := client.(*net.TCPConn); ok {
				tc.SetLinger(0)
			}
		}
		client.Close()
		server.Close()
	}

	touch := func() {
		mu.Lock()
		lastActivity = time.Now()
		mu.Unlock()
	}

	// Forward bytes in both directions. Reset lastActivity on every chunk so
	// connections that are actively running queries are never timed out.
	forward := func(dst, src net.Conn, cause string) {
		buf := make([]byte, 32*1024)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				touch()
				dst.Write(buf[:n]) //nolint:errcheck
			}
			if err != nil {
				closeAll(cause)
				return
			}
		}
	}
	go forward(server, client, "client_gone")
	go forward(client, server, "server_gone")

	// Idle watchdog: check every 200 ms; drop connection if idle too long.
	for {
		time.Sleep(200 * time.Millisecond)
		mu.Lock()
		isDone := done
		idle := time.Since(lastActivity)
		mu.Unlock()
		if isDone {
			return
		}
		if idle >= p.idleTimeout {
			closeAll("idle")
			return
		}
	}
}

func (p *tcpProxy) stop() { p.listener.Close() }

func (p *tcpProxy) closedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.forceClosed
}

// ─────────────────────────────────────────────────────────────────────────────
// Scenarios
// ─────────────────────────────────────────────────────────────────────────────

func main() {
	runPartA()
	fmt.Println()
	runPartB()
}

func now() string { return time.Now().Format("15:04:05.000") }

// ─── Part A: the problem ──────────────────────────────────────────────────────

func runPartA() {
	dbutil.Header("Scenario 5A: THE PROBLEM — stale connection → connection reset by peer")

	fmt.Println()
	fmt.Println("Architecture:")
	fmt.Println("  [Go app] ──► [TCP proxy :5442] ──► [PostgreSQL :5440]")
	fmt.Println()
	fmt.Println("  The proxy simulates any intermediary with an idle-connection timeout.")
	fmt.Println("  (AWS Application Load Balancer, GCP Cloud SQL Proxy, RDS Proxy, firewall, NAT, etc.)")
	fmt.Println()
	fmt.Printf("  Proxy idle timeout : %v  (real AWS Application Load Balancer default = 60s)\n", proxyIdleTimeout)
	fmt.Println("  ConnMaxLifetime   : 0  (never recycle)")
	fmt.Println("  ConnMaxIdleTime   : 0  (never recycle)  ← the misconfiguration")
	fmt.Println()
	fmt.Println("Why db.Conn() instead of db.Exec():")
	fmt.Println("  database/sql has a built-in retry: when lib/pq returns driver.ErrBadConn")
	fmt.Println("  (which ECONNRESET maps to), db.Exec() silently retries on a new connection.")
	fmt.Println("  db.Conn() pins you to ONE physical TCP socket — no retry is possible,")
	fmt.Println("  so the error surfaces just as it does in transactions and prepared statements.")
	fmt.Println()
	fmt.Println("Expected timeline:")
	fmt.Printf("  t=0s   Query 1 runs → TCP connection goes idle\n")
	fmt.Printf("  t=%vs  Proxy RST-closes the idle connection\n", proxyIdleTimeout.Seconds())
	fmt.Printf("  t=5s   Query 2 writes to a dead socket → connection reset by peer\n")
	fmt.Println()

	proxy := newProxy()
	defer proxy.stop()

	db, err := sql.Open("postgres", proxyDSN)
	if err != nil {
		panic(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	// Intentionally NOT setting ConnMaxLifetime or ConnMaxIdleTime — this is the bug.
	// (SetMaxIdleConns is irrelevant here: db.Conn() holds the socket exclusively
	// and never returns it to the idle pool between queries.)

	// db.Conn() acquires ONE physical connection and holds it exclusively.
	// Queries on this handle go directly to that socket — database/sql cannot
	// swap in a fresh connection and retry behind our back.
	conn, err := db.Conn(context.Background())
	if err != nil {
		fmt.Printf("  Cannot connect through proxy: %v\n  Make sure pg-normal is running: make up\n", err)
		return
	}
	defer conn.Close()

	fmt.Printf("  [%s] Query 1 → ", now())
	if _, err := conn.ExecContext(context.Background(), "SELECT 1"); err != nil {
		fmt.Printf("UNEXPECTED ERROR: %v\n", err)
		return
	}
	fmt.Println("OK   (TCP connection is now idle — no data flowing through proxy)")

	sleepFor := proxyIdleTimeout + 2*time.Second
	fmt.Printf("  [%s] Sleeping %v (longer than proxy timeout)...\n", now(), sleepFor)
	time.Sleep(sleepFor)

	fmt.Printf("  [%s] Query 2 (writing to dead socket) → ", now())
	_, err = conn.ExecContext(context.Background(), "SELECT 1")
	if err != nil {
		fmt.Printf("ERROR\n")
		fmt.Printf("  [%s]   %v\n", now(), err)
	} else {
		fmt.Println("OK  (unexpected — connection survived)")
	}

	fmt.Println()
	dbutil.Divider()
	fmt.Printf("  Proxy force-closed %d connection(s) via RST\n", proxy.closedCount())
	fmt.Println()
	fmt.Println("Final pool stats:")
	dbutil.PrintStats(db)
	fmt.Println()
	fmt.Println("Key Takeaway:")
	fmt.Printf("  The proxy dropped the idle connection after %v — Go's pool didn't know.\n", proxyIdleTimeout)
	fmt.Println("  Reusing that dead socket caused 'connection reset by peer'.")
	fmt.Println("  In production this happens with AWS Application Load Balancer, RDS Proxy, GCP Cloud SQL")
	fmt.Println("  Proxy, Azure SQL Gateway, firewalls, NAT gateways, and more.")
	fmt.Println("  It's especially painful inside transactions and prepared statements,")
	fmt.Println("  where database/sql's silent retry cannot help you.")
}

// ─── Part B: the fix ─────────────────────────────────────────────────────────

func runPartB() {
	fixIdleTime := proxyIdleTimeout - 1*time.Second // 1 s buffer before proxy fires

	dbutil.Header("Scenario 5B: THE FIX — ConnMaxIdleTime shorter than intermediary timeout")

	fmt.Println()
	fmt.Println("Same proxy, same idle timeout. Now Go proactively closes idle connections.")
	fmt.Println()
	fmt.Printf("  Proxy idle timeout : %v\n", proxyIdleTimeout)
	fmt.Printf("  ConnMaxIdleTime    : %v  ← shorter than proxy timeout\n", fixIdleTime)
	fmt.Println()
	fmt.Println("Expected timeline:")
	fmt.Printf("  t=0s   Query 1 runs → connection idle in pool\n")
	fmt.Printf("  t=%vs  Go pool closes the connection (ConnMaxIdleTime)\n", fixIdleTime.Seconds())
	fmt.Printf("  t=%vs  Proxy would have dropped it — but it's already gone\n", proxyIdleTimeout.Seconds())
	fmt.Printf("  t=5s   Query 2 → pool opens fresh connection → no error\n")
	fmt.Println()

	proxy := newProxy()
	defer proxy.stop()

	db, err := sql.Open("postgres", proxyDSN)
	if err != nil {
		panic(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(fixIdleTime) // THE FIX

	if err := db.Ping(); err != nil {
		fmt.Printf("  Cannot connect through proxy: %v\n", err)
		return
	}

	fmt.Printf("  [%s] Query 1 → ", now())
	if _, err := db.Exec("SELECT 1"); err != nil {
		fmt.Printf("UNEXPECTED ERROR: %v\n", err)
		return
	}
	fmt.Println("OK   (connection is now idle in pool)")

	sleepFor := proxyIdleTimeout + 2*time.Second
	fmt.Printf("  [%s] Sleeping %v (watching pool stats every second)...\n", now(), sleepFor)
	fmt.Println()

	prevClosed := int64(0)
	for i := 1; i <= int(sleepFor.Seconds()); i++ {
		time.Sleep(time.Second)
		s := db.Stats()
		note := ""
		if s.MaxIdleTimeClosed > prevClosed {
			note = "  ← Go closed connection proactively (before proxy!)"
			prevClosed = s.MaxIdleTimeClosed
		}
		fmt.Printf("  [%s] t=+%ds  Idle=%-2d  MaxIdleTimeClosed=%d%s\n",
			now(), i, s.Idle, s.MaxIdleTimeClosed, note)
	}

	fmt.Println()
	fmt.Printf("  [%s] Query 2 → ", now())
	if _, err := db.Exec("SELECT 1"); err != nil {
		fmt.Printf("ERROR: %v\n", err)
	} else {
		fmt.Println("OK   (pool opened a fresh connection — no error!)")
	}

	fmt.Println()
	dbutil.Divider()
	fmt.Printf("  Proxy force-closed %d connection(s) via RST\n", proxy.closedCount())
	fmt.Println("  (0 = Go cleaned up first, proxy never had to fire)")
	fmt.Println()
	fmt.Println("Final pool stats:")
	dbutil.PrintStats(db)
	fmt.Println()
	fmt.Println("Key Takeaway:")
	fmt.Printf("  ConnMaxIdleTime=%v closed the connection at t≈%vs.\n", fixIdleTime, fixIdleTime.Seconds())
	fmt.Printf("  The proxy would have RST-closed it at t=%vs — 1 second later.\n", proxyIdleTimeout.Seconds())
	fmt.Println("  Go won the race → no stale connection → no error.")
	fmt.Println()
	fmt.Println("  Production rule:")
	fmt.Println("    db.SetConnMaxIdleTime(intermediaryTimeout - buffer)")
	fmt.Println("    db.SetConnMaxLifetime(intermediaryTimeout - buffer)")
	fmt.Println()
	fmt.Println("  Common values (subtract 5-10s as buffer):")
	fmt.Println("    AWS Application Load Balancer   60s  → ConnMaxIdleTime=55s")
	fmt.Println("    Azure SQL Gateway      30s  → ConnMaxIdleTime=25s")
	fmt.Println("    GCP Cloud SQL Proxy   600s  → ConnMaxIdleTime=590s")
	fmt.Println("    AWS RDS Proxy          configurable — check your setting")
	fmt.Println("    AWS NAT Gateway       350s  → ConnMaxIdleTime=340s")
	fmt.Println("    Corporate firewall    varies — ask your infra team")
}
