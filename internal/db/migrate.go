package db

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationLockID identifies booth-streamlit's migration lock in Postgres's advisory-lock
// keyspace, so replicas starting together serialize instead of racing.
const migrationLockID int64 = 0x626f6f7473746c74 // "bootstlt"

// Migrate applies a component's pending migrations from dir within fsys, in filename order, each
// exactly once, in one transaction under an advisory lock. Filenames start with a numeric version
// ("0001_x.sql"). Same scheme as booth-catalog's internal/db.
func Migrate(ctx context.Context, pool *pgxpool.Pool, component string, fsys fs.FS, dir string) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return fmt.Errorf("reading %s migrations: %w", component, err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting %s migration transaction: %w", component, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("taking migration lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS streamlit_schema_migrations (
		component  TEXT        NOT NULL,
		version    INT         NOT NULL,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (component, version)
	)`); err != nil {
		return fmt.Errorf("creating migrations table: %w", err)
	}
	for _, name := range names {
		version, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("%s migration %q: filename must start with a numeric version", component, name)
		}
		var applied bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM streamlit_schema_migrations WHERE component = $1 AND version = $2)`, component, version).Scan(&applied); err != nil {
			return fmt.Errorf("checking %s migration %s: %w", component, name, err)
		}
		if applied {
			continue
		}
		sql, err := fs.ReadFile(fsys, dir+"/"+name)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("applying %s migration %s: %w", component, name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO streamlit_schema_migrations (component, version) VALUES ($1, $2)`, component, version); err != nil {
			return fmt.Errorf("recording %s migration %s: %w", component, name, err)
		}
	}
	return tx.Commit(ctx)
}
