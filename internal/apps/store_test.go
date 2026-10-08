package apps

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/projectbooth/booth-streamlit/internal/db"
)

// stores returns every Store implementation to run the same cases against: memory always, and the
// real Postgres from hack/docker-compose.emulators.yml when it is configured (CI requires it).
func stores(t *testing.T) map[string]Store {
	t.Helper()
	out := map[string]Store{"memory": NewMemoryStore()}
	dsn := os.Getenv("BOOTH_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("BOOTH_TEST_REQUIRE_EMULATORS") == "1" {
			t.Fatal("BOOTH_TEST_POSTGRES_DSN is not set but BOOTH_TEST_REQUIRE_EMULATORS=1")
		}
		return out
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	// A clean slate per test run; migrations re-apply.
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS apps; DELETE FROM streamlit_schema_migrations WHERE component = 'apps'`); err != nil {
		if _, err2 := pool.Exec(ctx, `DROP TABLE IF EXISTS apps`); err2 != nil {
			t.Fatal(err2)
		}
	}
	pg, err := NewPostgresStore(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	// Running migrations twice is a no-op.
	if _, err := NewPostgresStore(ctx, pool); err != nil {
		t.Fatalf("second migration run: %v", err)
	}
	out["postgres"] = pg
	return out
}

func app(id, ws, name string, shared bool) App {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	return App{ID: id, Workspace: ws, Name: name, Source: "src-" + id, Shared: shared, DesiredState: Stopped,
		CreatedBy: "o", CreatedAt: now, UpdatedBy: "o", UpdatedAt: now}
}

func TestStores(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			for _, a := range []App{app("a1", "acme", "beta", false), app("a2", "acme", "Alpha", true), app("a3", "other", "gamma", true)} {
				if err := s.Create(ctx, a); err != nil {
					t.Fatal(err)
				}
			}

			all, err := s.List(ctx, "acme", false)
			if err != nil || len(all) != 2 || all[0].Name != "Alpha" || all[1].Name != "beta" || all[0].Source != "" {
				t.Fatalf("List(acme): %+v %v (want Alpha, beta by case-insensitive name, no source)", all, err)
			}
			if sh, _ := s.List(ctx, "acme", true); len(sh) != 1 || sh[0].ID != "a2" {
				t.Errorf("List(acme, sharedOnly) = %+v", sh)
			}

			got, err := s.Get(ctx, "acme", "a1")
			if err != nil || got != app("a1", "acme", "beta", false) {
				t.Errorf("Get = %+v %v", got, err)
			}
			// Workspace scoping: another workspace's id is not found, for every operation.
			if _, err := s.Get(ctx, "acme", "a3"); !errors.Is(err, ErrNotFound) {
				t.Errorf("Get across workspaces: %v", err)
			}
			if err := s.Update(ctx, app("a3", "acme", "x", true)); !errors.Is(err, ErrNotFound) {
				t.Errorf("Update across workspaces: %v", err)
			}
			if _, err := s.SetDesiredState(ctx, "acme", "a3", Running, "x"); !errors.Is(err, ErrNotFound) {
				t.Errorf("SetDesiredState across workspaces: %v", err)
			}
			if err := s.Delete(ctx, "acme", "a3"); !errors.Is(err, ErrNotFound) {
				t.Errorf("Delete across workspaces: %v", err)
			}

			upd := got
			upd.Name, upd.Source, upd.Shared, upd.UpdatedBy = "beta2", "new", true, "o2"
			upd.UpdatedAt = got.UpdatedAt.Add(time.Hour)
			if err := s.Update(ctx, upd); err != nil {
				t.Fatal(err)
			}
			if after, _ := s.Get(ctx, "acme", "a1"); after != upd {
				t.Errorf("after Update: %+v, want %+v", after, upd)
			}
			if r, err := s.SetDesiredState(ctx, "acme", "a1", Running, "o3"); err != nil || r.DesiredState != Running || r.UpdatedBy != "o3" {
				t.Errorf("SetDesiredState: %+v %v", r, err)
			}
			if err := s.Delete(ctx, "acme", "a1"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Get(ctx, "acme", "a1"); !errors.Is(err, ErrNotFound) {
				t.Errorf("Get after Delete: %v", err)
			}
			if err := s.Ping(ctx); err != nil {
				t.Error(err)
			}
		})
	}
}
