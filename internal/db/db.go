// Package db opens booth-streamlit's connection pool against this module's own database on the
// shared PostgreSQL cluster (ADR 0014/0053). Schema and migrations arrive with the first real
// table (app definitions); the scaffold only needs a verified connection for /healthz.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Open connects to dsn and verifies the connection, riding out a refused or unreachable first
// connection for up to DefaultMaxElapsed (wait.go, ARCHITECTURE.md item 66 (c)).
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	return open(ctx, dsn, DefaultMaxElapsed)
}

func open(ctx context.Context, dsn string, maxElapsed time.Duration) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn) // parses the DSN; connects lazily
	if err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	if err := Retry(ctx, "pinging postgres", maxElapsed, func(actx context.Context) error { return pool.Ping(actx) }); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}
