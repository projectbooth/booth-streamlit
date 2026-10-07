// Package api is booth-streamlit's HTTP surface. In the scaffold that is only the two health
// endpoints and the module's UI: the app-management API and the per-app reverse proxy arrive with
// the app model, and with them the X-Booth-Identity verification every non-health route needs
// (ADR 0069, core-platform-api.md "Auth enforcement"). Nothing served here today reads or writes
// user data, which is why nothing here is authenticated yet.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/projectbooth/booth-streamlit/internal/events"
)

// Pinger is the database check /healthz runs; *pgxpool.Pool satisfies it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// BusState reports the event-bus connection; *events.Bus satisfies it.
type BusState interface {
	State() events.State
}

// Deps is what the router needs.
type Deps struct {
	DB  Pinger
	Bus BusState
	// Web is the built UI (web/dist). Nil serves no UI, only the health endpoints.
	Web fs.FS
}

// dbPingTimeout bounds the /healthz database check, so a hung Postgres shows as unhealthy within
// one probe period instead of hanging the probe.
const dbPingTimeout = 2 * time.Second

// NewRouter builds the HTTP handler.
func NewRouter(deps Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger) // stdout only (ADR 0022)
	r.Use(middleware.Recoverer)

	// Liveness deliberately looks at nothing external: restarting the pod cannot fix a Postgres
	// or NATS outage, so neither should get it killed.
	r.Get("/livez", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/healthz", healthz(deps))

	if deps.Web != nil {
		r.NotFound(spa(deps.Web))
	}
	return r
}

// healthz is both the readiness probe and what booth-core polls (the manifest's
// healthCheckPath). The database is required: without it there are no app definitions to serve,
// so the module is unready (503). The event bus is not: running apps keep working without it and
// only catalog indexing stalls, so a bus that is still connecting is "degraded", not down.
func healthz(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := map[string]string{"status": "ok", "database": "ok", "eventBus": string(events.StateDisabled)}
		code := http.StatusOK

		ctx, cancel := context.WithTimeout(r.Context(), dbPingTimeout)
		defer cancel()
		if deps.DB == nil {
			body["database"], body["status"], code = "unconfigured", "unavailable", http.StatusServiceUnavailable
		} else if err := deps.DB.Ping(ctx); err != nil {
			body["database"], body["status"], code = "unreachable", "unavailable", http.StatusServiceUnavailable
		}

		if deps.Bus != nil {
			st := deps.Bus.State()
			body["eventBus"] = string(st)
			if st == events.StateConnecting && code == http.StatusOK {
				body["status"] = "degraded"
			}
		}
		writeJSON(w, code, body)
	}
}

// spa serves the built UI. Asset URLs are relative (vite `base: "./"`), because under iframe-proxy
// the browser sees this module at /iframe/streamlit/..., and booth-core forwards only the remainder
// of the path. An unknown path that looks like a page (no file extension) gets index.html so
// client-side routes survive a reload; an unknown asset is a real 404.
func spa(web fs.FS) http.HandlerFunc {
	files := http.FileServerFS(web)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			name = "."
		}
		if _, err := fs.Stat(web, name); err != nil {
			if !errors.Is(err, fs.ErrNotExist) || path.Ext(name) != "" {
				http.NotFound(w, r)
				return
			}
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		// index.html must never be cached: it names the hashed asset files of the current build.
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	}
}

// WebDir returns the UI directory as an fs.FS, or nil when dir is empty.
func WebDir(dir string) fs.FS {
	if dir == "" {
		return nil
	}
	return os.DirFS(dir)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
