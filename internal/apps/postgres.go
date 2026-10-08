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

const cols = `id, workspace, name, description, source, shared, desired_state, suspended, gate_bearer, owner, data_paused_reason, data_paused_at, data_epoch, created_by, created_at, updated_by, updated_at`

func scanApp(row pgx.Row) (App, error) {
	var a App
	var st string
	if err := row.Scan(&a.ID, &a.Workspace, &a.Name, &a.Description, &a.Source, &a.Shared, &st, &a.Suspended, &a.GateBearer, &a.Owner, &a.DataPausedReason, &a.DataPausedAt, &a.DataEpoch, &a.CreatedBy, &a.CreatedAt, &a.UpdatedBy, &a.UpdatedAt); err != nil {
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
		a.Source, a.GateBearer = "", ""
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *PostgresStore) Get(ctx context.Context, workspace, id string) (App, error) {
	return scanApp(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM apps WHERE workspace = $1 AND id = $2`, workspace, id))
}

func (s *PostgresStore) Create(ctx context.Context, a App) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO apps (`+cols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		a.ID, a.Workspace, a.Name, a.Description, a.Source, a.Shared, string(a.DesiredState), a.Suspended, a.GateBearer,
		a.Owner, a.DataPausedReason, a.DataPausedAt, a.DataEpoch, a.CreatedBy, a.CreatedAt, a.UpdatedBy, a.UpdatedAt)
	if err != nil {
		return fmt.Errorf("creating app: %w", err)
	}
	return nil
}

func (s *PostgresStore) Update(ctx context.Context, a App) error {
	tag, err := s.pool.Exec(ctx, `UPDATE apps SET name = $3, description = $4, source = $5, shared = $6, updated_by = $7, updated_at = $8
		WHERE workspace = $1 AND id = $2`, a.Workspace, a.ID, a.Name, a.Description, a.Source, a.Shared, a.UpdatedBy, a.UpdatedAt)
	if err != nil {
		return fmt.Errorf("updating app: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
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
	tag, err := s.pool.Exec(ctx, `DELETE FROM apps WHERE workspace = $1 AND id = $2`, workspace, id)
	if err != nil {
		return fmt.Errorf("deleting app: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
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
	var prev string
	err = tx.QueryRow(ctx, `SELECT owner FROM apps WHERE workspace = $1 AND id = $2 FOR UPDATE`, workspace, id).Scan(&prev)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("reading owner: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE apps SET owner = $3, data_paused_reason = '', data_paused_at = NULL WHERE workspace = $1 AND id = $2`, workspace, id, newOwner); err != nil {
		return "", fmt.Errorf("setting owner: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO app_ownership_changes (app_id, workspace, previous_owner, new_owner, reason, at) VALUES ($1,$2,$3,$4,$5,$6)`,
		id, workspace, prev, newOwner, reason, at); err != nil {
		return "", fmt.Errorf("recording the change: %w", err)
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
