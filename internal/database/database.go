package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	// Blank import: we never call pgx directly here. Importing it runs its init(),
	// which registers the "pgx" driver with database/sql.
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Open creates a PostgreSQL connection pool and checks it with a ping.
// The URL contains the password, so it is never logged or put into errors here.
func Open(ctx context.Context, url string) (*sql.DB, error) {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return nil, fmt.Errorf("database: open: %w", err)
	}

	// Pool limits. database/sql opens connections on demand up to these numbers.
	db.SetMaxOpenConns(10)                  // never more than 10 connections to Postgres
	db.SetMaxIdleConns(10)                  // keep up to 10 open between requests, ready to reuse
	db.SetConnMaxIdleTime(5 * time.Minute)  // close connections idle for 5 minutes
	db.SetConnMaxLifetime(30 * time.Minute) // replace every connection after 30 minutes

	// sql.Open does not connect. Ping does, so a bad URL or a stopped database fails here.
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("database: ping: %w", err)
	}
	return db, nil
}
