package dbutil

import (
	"database/sql"
	"fmt"
)

// PrintStats prints all pool statistics from db.Stats() in a readable format.
func PrintStats(db *sql.DB) {
	s := db.Stats()
	fmt.Printf("  MaxOpenConnections : %d  (limit set by SetMaxOpenConns)\n", s.MaxOpenConnections)
	fmt.Printf("  OpenConnections    : %d  (in-use + idle)\n", s.OpenConnections)
	fmt.Printf("  InUse              : %d  (connections executing queries)\n", s.InUse)
	fmt.Printf("  Idle               : %d  (connections sitting in pool)\n", s.Idle)
	fmt.Printf("  WaitCount          : %d  (total goroutines that waited for a connection)\n", s.WaitCount)
	fmt.Printf("  WaitDuration       : %v  (total time spent waiting)\n", s.WaitDuration)
	fmt.Printf("  MaxIdleClosed      : %d  (closed because exceeded MaxIdleConns)\n", s.MaxIdleClosed)
	fmt.Printf("  MaxIdleTimeClosed  : %d  (closed because exceeded ConnMaxIdleTime)\n", s.MaxIdleTimeClosed)
	fmt.Printf("  MaxLifetimeClosed  : %d  (closed because exceeded ConnMaxLifetime)\n", s.MaxLifetimeClosed)
}

// OpenDB opens a connection to PostgreSQL and verifies connectivity.
func OpenDB(dsn string) (*sql.DB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("db.Ping: %w", err)
	}
	return db, nil
}

func Divider() {
	fmt.Println("----------------------------------------------------------")
}

func Header(title string) {
	fmt.Println()
	fmt.Println("==========================================================")
	fmt.Printf("  %s\n", title)
	fmt.Println("==========================================================")
}
