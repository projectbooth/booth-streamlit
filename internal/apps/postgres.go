package apps

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/projectbooth/booth-streamlit/internal/db"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// PostgresStore is the Store on this module's own database (ADR 0014/0053).
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore applies this package's migrations and returns a store over pool.
func NewPostgresStore(ctx context.Context, pool *pgxpool.Pool) (*PostgresStore, error) {
	if err := db.Migrate(ctx, pool, "apps", migrationFS, "migrations"); err != nil {
		return nil, err
	}
	return &PostgresStore{pool: pool}, nil
}

const cols = `id, workspace, name, description, source, requirements, sources, shared, desired_state, suspended, gate_bearer, owner, data_paused_reason, data_paused_at, data_epoch, created_by, created_at, updated_by, updated_at`

func scanApp(row pgx.Row) (App, error) {
	var a App
	var st string
	if err := row.Scan(&a.ID, &a.Workspace, &a.Name, &a.Description, &a.Source, &a.Requirements, &a.Sources, &a.Shared, &st, &a.Suspended, &a.GateBearer, &a.Owner, &a.DataPausedReason, &a.DataPausedAt, &a.DataEpoch, &a.CreatedBy, &a.CreatedAt, &a.UpdatedBy, &a.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return App{}, ErrNotFound
		}
		return App{}, err
	}
	a.DesiredState = DesiredState(st)
	a.CreatedAt, a.UpdatedAt = a.CreatedAt.UTC(), a.UpdatedAt.UTC()
	if a.DataPausedAt != nil {
		t := a.DataPausedAt.UTC()
		a.DataPausedAt = &t
	}
	return a, nil
}

func (s *PostgresStore) List(ctx context.Context, workspace string, sharedOnly bool) ([]App, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+cols+` FROM apps
		WHERE workspace = $1 AND (shared OR NOT $2) ORDER BY lower(name), id`, workspace, sharedOnly)
	if err != nil {
		return nil, fmt.Errorf("listing apps: %w", err)
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		a.Source, a.Requirements, a.GateBearer = "", "", ""
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *PostgresStore) Get(ctx context.Context, workspace, id string) (App, error) {
	return scanApp(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM apps WHERE workspace = $1 AND id = $2`, workspace, id))
}

// inTx runs fn in a transaction, committing only if it succeeds.
func (s *PostgresStore) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// lockApp reads an app for update inside tx: the change's events are computed from it, so two
// concurrent changes to one app are serialized and each event describes the state it committed.
func lockApp(ctx context.Context, tx pgx.Tx, workspace, id string) (App, error) {
	return scanApp(tx.QueryRow(ctx, `SELECT `+cols+` FROM apps WHERE workspace = $1 AND id = $2 FOR UPDATE`, workspace, id))
}

// writeEvents adds events to the outbox in tx (the same transaction as the change).
func writeEvents(ctx context.Context, tx pgx.Tx, evs []DashboardEvent) error {
	for _, e := range evs {
		// created_at is publishedAt, compared last-writer-wins by booth-catalog: strictly after this
		// app's previous event even within one clock tick (the row lock orders the transactions).
		if _, err := tx.Exec(ctx, `INSERT INTO app_events_outbox (workspace, app_id, event_type, data, created_at) VALUES ($1,$2,$3,$4,
			GREATEST(clock_timestamp(), (SELECT max(created_at) + interval '1 microsecond' FROM app_events_outbox WHERE workspace = $1 AND app_id = $2)))`,
			e.Workspace, e.AppID, e.Type, []byte(e.Data)); err != nil {
			return fmt.Errorf("writing the %s event: %w", e.Type, err)
		}
	}
	return nil
}

func (s *PostgresStore) Create(ctx context.Context, a App) error {
	if a.Sources == nil {
		a.Sources = []string{}
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO apps (`+cols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
			a.ID, a.Workspace, a.Name, a.Description, a.Source, a.Requirements, a.Sources, a.Shared, string(a.DesiredState), a.Suspended, a.GateBearer,
			a.Owner, a.DataPausedReason, a.DataPausedAt, a.DataEpoch, a.CreatedBy, a.CreatedAt, a.UpdatedBy, a.UpdatedAt)
		if err != nil {
			return fmt.Errorf("creating app: %w", err)
		}
		return writeEvents(ctx, tx, dashboardEvents(nil, &a))
	})
}

func (s *PostgresStore) Update(ctx context.Context, a App) error {
	if a.Sources == nil {
		a.Sources = []string{}
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		before, err := lockApp(ctx, tx, a.Workspace, a.ID)
		if err != nil {
			return err
		}
		after := before
		after.Name, after.Description, after.Source, after.Requirements, after.Sources, after.Shared = a.Name, a.Description, a.Source, a.Requirements, a.Sources, a.Shared
		after.UpdatedBy, after.UpdatedAt = a.UpdatedBy, a.UpdatedAt
		if _, err := tx.Exec(ctx, `UPDATE apps SET name = $3, description = $4, source = $5, shared = $6, updated_by = $7, updated_at = $8, requirements = $9, sources = $10
			WHERE workspace = $1 AND id = $2`, a.Workspace, a.ID, a.Name, a.Description, a.Source, a.Shared, a.UpdatedBy, a.UpdatedAt, a.Requirements, a.Sources); err != nil {
			return fmt.Errorf("updating app: %w", err)
		}
		return writeEvents(ctx, tx, dashboardEvents(&before, &after))
	})
}

func (s *PostgresStore) SetDesiredState(ctx context.Context, workspace, id string, st DesiredState, by string) (App, error) {
	return scanApp(s.pool.QueryRow(ctx, `UPDATE apps SET desired_state = $3, updated_by = $4, suspended = FALSE
		WHERE workspace = $1 AND id = $2 RETURNING `+cols, workspace, id, string(st), by))
}

func (s *PostgresStore) SetSuspended(ctx context.Context, workspace, id string, suspended bool) error {
	tag, err := s.pool.Exec(ctx, `UPDATE apps SET suspended = $3 WHERE workspace = $1 AND id = $2`, workspace, id, suspended)
	if err != nil {
		return fmt.Errorf("setting suspended: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) ListAll(ctx context.Context) ([]App, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+cols+` FROM apps ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("listing all apps: %w", err)
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *PostgresStore) Delete(ctx context.Context, workspace, id string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		before, err := lockApp(ctx, tx, workspace, id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM apps WHERE workspace = $1 AND id = $2`, workspace, id); err != nil {
			return fmt.Errorf("deleting app: %w", err)
		}
		return writeEvents(ctx, tx, dashboardEvents(&before, nil))
	})
}

func (s *PostgresStore) GetByBearer(ctx context.Context, bearer string) (App, error) {
	if bearer == "" {
		return App{}, ErrNotFound
	}
	return scanApp(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM apps WHERE gate_bearer = $1`, bearer))
}

func (s *PostgresStore) SetDataPaused(ctx context.Context, workspace, id, reason string, at time.Time, bumpEpoch bool) (App, error) {
	var pausedAt *time.Time
	if reason != "" {
		pausedAt = &at
	}
	bump := 0
	if bumpEpoch {
		bump = 1
	}
	return scanApp(s.pool.QueryRow(ctx, `UPDATE apps SET data_paused_reason = $3, data_paused_at = $4, data_epoch = data_epoch + $5
		WHERE workspace = $1 AND id = $2 RETURNING `+cols, workspace, id, reason, pausedAt, bump))
}

func (s *PostgresStore) TakeOwnership(ctx context.Context, workspace, id, newOwner, reason string, at time.Time) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op
	before, err := lockApp(ctx, tx, workspace, id)
	if err != nil {
		return "", err
	}
	prev := before.Owner
	if _, err := tx.Exec(ctx, `UPDATE apps SET owner = $3, data_paused_reason = '', data_paused_at = NULL WHERE workspace = $1 AND id = $2`, workspace, id, newOwner); err != nil {
		return "", fmt.Errorf("setting owner: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO app_ownership_changes (app_id, workspace, previous_owner, new_owner, reason, at) VALUES ($1,$2,$3,$4,$5,$6)`,
		id, workspace, prev, newOwner, reason, at); err != nil {
		return "", fmt.Errorf("recording the change: %w", err)
	}
	after := before
	after.Owner = newOwner
	if err := writeEvents(ctx, tx, dashboardEvents(&before, &after)); err != nil {
		return "", err
	}
	return prev, tx.Commit(ctx)
}

func (s *PostgresStore) OwnershipChanges(ctx context.Context, workspace, id string) ([]OwnershipChange, error) {
	rows, err := s.pool.Query(ctx, `SELECT app_id, workspace, previous_owner, new_owner, reason, at FROM app_ownership_changes
		WHERE workspace = $1 AND app_id = $2 ORDER BY at`, workspace, id)
	if err != nil {
		return nil, fmt.Errorf("listing ownership changes: %w", err)
	}
	defer rows.Close()
	var out []OwnershipChange
	for rows.Next() {
		var c OwnershipChange
		if err := rows.Scan(&c.AppID, &c.Workspace, &c.PreviousOwner, &c.NewOwner, &c.Reason, &c.At); err != nil {
			return nil, err
		}
		c.At = c.At.UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *PostgresStore) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// PublishNext hands the oldest due outbox row to publish, holding its row lock (SKIP LOCKED, so
// several backends never publish one row at once). Success marks it published; a failure records
// the attempt and backs off exponentially (2s doubling, capped at 5 minutes), and the
// maxAttempts-th failure marks it failed for good. found is false when nothing is due.
func (s *PostgresStore) PublishNext(ctx context.Context, maxAttempts int, publish func(OutboxRow) error) (found bool, err error) {
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var r OutboxRow
		var data []byte
		err := tx.QueryRow(ctx, `SELECT id, workspace, app_id, event_type, data, created_at, attempts FROM app_events_outbox
			WHERE published_at IS NULL AND NOT failed AND next_attempt_at <= clock_timestamp()
			ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&r.ID, &r.Workspace, &r.AppID, &r.Type, &data, &r.CreatedAt, &r.Attempts)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the outbox: %w", err)
		}
		found = true
		r.Data = data
		if perr := publish(r); perr != nil {
			_, err = tx.Exec(ctx, `UPDATE app_events_outbox SET attempts = attempts + 1, last_error = $2,
				failed = attempts + 1 >= $3,
				next_attempt_at = clock_timestamp() + make_interval(secs => least(power(2, attempts + 1), 300))
				WHERE id = $1`, r.ID, truncate(perr.Error(), 1000), maxAttempts)
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE app_events_outbox SET published_at = clock_timestamp(), attempts = attempts + 1, last_error = '' WHERE id = $1`, r.ID)
		return err
	})
	return found, err
}

// OutboxStats counts what hasn't been published.
func (s *PostgresStore) OutboxStats(ctx context.Context) (OutboxStats, error) {
	var st OutboxStats
	err := s.pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE published_at IS NULL AND NOT failed),
		count(*) FILTER (WHERE failed),
		coalesce((SELECT last_error FROM app_events_outbox WHERE published_at IS NULL AND last_error <> '' ORDER BY id DESC LIMIT 1), '')
		FROM app_events_outbox`).Scan(&st.Pending, &st.Failed, &st.LastError)
	return st, err
}

// PruneOutbox deletes rows published more than olderThan ago. Failed rows are kept.
func (s *PostgresStore) PruneOutbox(ctx context.Context, olderThan time.Duration) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM app_events_outbox WHERE published_at < clock_timestamp() - make_interval(secs => $1)`, olderThan.Seconds())
	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
