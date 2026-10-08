package apps

import (
	"context"
	"embed"
	"errors"
	"fmt"

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

const cols = `id, workspace, name, description, source, shared, desired_state, created_by, created_at, updated_by, updated_at`

func scanApp(row pgx.Row) (App, error) {
	var a App
	var st string
	if err := row.Scan(&a.ID, &a.Workspace, &a.Name, &a.Description, &a.Source, &a.Shared, &st, &a.CreatedBy, &a.CreatedAt, &a.UpdatedBy, &a.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return App{}, ErrNotFound
		}
		return App{}, err
	}
	a.DesiredState = DesiredState(st)
	a.CreatedAt, a.UpdatedAt = a.CreatedAt.UTC(), a.UpdatedAt.UTC()
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
		a.Source = ""
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *PostgresStore) Get(ctx context.Context, workspace, id string) (App, error) {
	return scanApp(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM apps WHERE workspace = $1 AND id = $2`, workspace, id))
}

func (s *PostgresStore) Create(ctx context.Context, a App) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO apps (`+cols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		a.ID, a.Workspace, a.Name, a.Description, a.Source, a.Shared, string(a.DesiredState), a.CreatedBy, a.CreatedAt, a.UpdatedBy, a.UpdatedAt)
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
	return scanApp(s.pool.QueryRow(ctx, `UPDATE apps SET desired_state = $3, updated_by = $4
		WHERE workspace = $1 AND id = $2 RETURNING `+cols, workspace, id, string(st), by))
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

func (s *PostgresStore) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
