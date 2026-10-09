package apps

import (
	"context"
	"errors"
	"os"
	"reflect"
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
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS app_events_outbox, app_ownership_changes, apps; DELETE FROM streamlit_schema_migrations WHERE component = 'apps'`); err != nil {
		if _, err2 := pool.Exec(ctx, `DROP TABLE IF EXISTS app_events_outbox, app_ownership_changes, apps`); err2 != nil {
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
	return App{ID: id, Workspace: ws, Name: name, Source: "src-" + id, Sources: []string{}, Shared: shared, DesiredState: Stopped, GateBearer: "bearer-" + id,
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
			if all[0].GateBearer != "" {
				t.Error("List exposes the gate bearer")
			}
			every, err := s.ListAll(ctx)
			if err != nil || len(every) != 3 || every[0].ID != "a1" || every[2].Workspace != "other" || every[0].GateBearer != "bearer-a1" || every[0].Source != "src-a1" {
				t.Fatalf("ListAll: %+v %v (want all workspaces by id, with source and bearer)", every, err)
			}
			if err := s.SetSuspended(ctx, "acme", "a2", true); err != nil {
				t.Fatal(err)
			}
			if got, _ := s.Get(ctx, "acme", "a2"); !got.Suspended {
				t.Error("SetSuspended(true) did not stick")
			}
			if err := s.SetSuspended(ctx, "acme", "a3", true); !errors.Is(err, ErrNotFound) {
				t.Errorf("SetSuspended across workspaces: %v", err)
			}
			// An owner's Start or Stop clears suspension.
			if r, _ := s.SetDesiredState(ctx, "acme", "a2", Running, "o"); r.Suspended {
				t.Error("SetDesiredState left the app suspended")
			}
			if sh, _ := s.List(ctx, "acme", true); len(sh) != 1 || sh[0].ID != "a2" {
				t.Errorf("List(acme, sharedOnly) = %+v", sh)
			}

			got, err := s.Get(ctx, "acme", "a1")
			if err != nil || !reflect.DeepEqual(got, app("a1", "acme", "beta", false)) {
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
			if after, _ := s.Get(ctx, "acme", "a1"); !reflect.DeepEqual(after, upd) {
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

func TestStores_DataAccess(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			a := app("d1", "acme", "x", true)
			a.Owner = "alice"
			b := app("d2", "other", "y", true)
			b.Owner = "bob"
			for _, x := range []App{a, b} {
				if err := s.Create(ctx, x); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := s.GetByBearer(ctx, "bearer-d2"); err != nil || got.ID != "d2" || got.Workspace != "other" {
				t.Fatalf("GetByBearer: %+v %v", got, err)
			}
			for _, bad := range []string{"", "bearer-", "BEARER-D1", "bearer-d1 "} {
				if _, err := s.GetByBearer(ctx, bad); !errors.Is(err, ErrNotFound) {
					t.Errorf("GetByBearer(%q): %v", bad, err)
				}
			}

			at := time.Date(2026, 10, 8, 13, 0, 0, 0, time.UTC)
			p, err := s.SetDataPaused(ctx, "acme", "d1", "owner gone", at, true)
			if err != nil || p.DataPausedReason != "owner gone" || p.DataPausedAt == nil || !p.DataPausedAt.Equal(at) || p.DataEpoch != 1 {
				t.Fatalf("SetDataPaused: %+v %v", p, err)
			}
			if _, err := s.SetDataPaused(ctx, "acme", "d2", "x", at, false); !errors.Is(err, ErrNotFound) {
				t.Errorf("SetDataPaused across workspaces: %v", err)
			}

			prev, err := s.TakeOwnership(ctx, "acme", "d1", "carol", "owner gone", at)
			if err != nil || prev != "alice" {
				t.Fatalf("TakeOwnership: %q %v", prev, err)
			}
			got, _ := s.Get(ctx, "acme", "d1")
			if got.Owner != "carol" || got.DataPausedReason != "" || got.DataPausedAt != nil {
				t.Errorf("after take-over: %+v", got)
			}
			if _, err := s.TakeOwnership(ctx, "acme", "d2", "carol", "", at); !errors.Is(err, ErrNotFound) {
				t.Errorf("TakeOwnership across workspaces: %v", err)
			}
			ch, err := s.OwnershipChanges(ctx, "acme", "d1")
			if err != nil || len(ch) != 1 || ch[0].PreviousOwner != "alice" || ch[0].NewOwner != "carol" || ch[0].Reason != "owner gone" || !ch[0].At.Equal(at) {
				t.Errorf("OwnershipChanges: %+v %v", ch, err)
			}
		})
	}
}
