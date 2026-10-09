package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/projectbooth/booth-streamlit/internal/apps"
	"github.com/projectbooth/booth-streamlit/internal/events"
)

type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

type fakeBus events.State

func (f fakeBus) State() events.State { return events.State(f) }

func get(t *testing.T, h http.Handler, target string) (*httptest.ResponseRecorder, map[string]string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	var body map[string]string
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decoding %s: %v\n%s", target, err, rec.Body)
		}
	}
	return rec, body
}

func TestHealthz(t *testing.T) {
	cases := []struct {
		name               string
		deps               Deps
		code               int
		status, db, evtBus string
	}{
		{"all up", Deps{DB: fakeDB{}, Bus: fakeBus(events.StateConnected)}, 200, "ok", "ok", "connected"},
		{"bus off on purpose", Deps{DB: fakeDB{}, Bus: fakeBus(events.StateDisabled)}, 200, "ok", "ok", "disabled"},
		{"no bus wired at all", Deps{DB: fakeDB{}}, 200, "ok", "ok", "disabled"},
		// Apps keep running without the bus, so it degrades rather than takes the module down.
		{"bus still connecting", Deps{DB: fakeDB{}, Bus: fakeBus(events.StateConnecting)}, 200, "degraded", "ok", "connecting"},
		{"database down", Deps{DB: fakeDB{errors.New("boom")}, Bus: fakeBus(events.StateConnected)}, 503, "unavailable", "unreachable", "connected"},
		{"database down and bus connecting", Deps{DB: fakeDB{errors.New("boom")}, Bus: fakeBus(events.StateConnecting)}, 503, "unavailable", "unreachable", "connecting"},
		{"no database", Deps{}, 503, "unavailable", "unconfigured", "disabled"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, body := get(t, NewRouter(c.deps), "/healthz")
			if rec.Code != c.code || body["status"] != c.status || body["database"] != c.db || body["eventBus"] != c.evtBus {
				t.Errorf("got %d %v, want %d status=%s database=%s eventBus=%s", rec.Code, body, c.code, c.status, c.db, c.evtBus)
			}
		})
	}
}

// Liveness must not depend on the database: restarting the pod can't fix Postgres.
func TestLivez_IgnoresTheDatabase(t *testing.T) {
	rec, body := get(t, NewRouter(Deps{DB: fakeDB{errors.New("boom")}}), "/livez")
	if rec.Code != 200 || body["status"] != "ok" {
		t.Fatalf("got %d %v", rec.Code, body)
	}
}

func TestSPA(t *testing.T) {
	web := fstest.MapFS{
		"index.html":         {Data: []byte("<!doctype html><title>Streamlit</title>")},
		"assets/index-a1.js": {Data: []byte("console.log(1)")},
	}
	h := NewRouter(Deps{DB: fakeDB{}, Web: web})

	rec, _ := get(t, h, "/")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<title>Streamlit</title>") {
		t.Errorf("/ = %d %q", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("index.html Cache-Control = %q, want no-cache", rec.Header().Get("Cache-Control"))
	}

	rec, _ = get(t, h, "/assets/index-a1.js")
	if rec.Code != 200 || rec.Body.String() != "console.log(1)" {
		t.Errorf("asset = %d %q", rec.Code, rec.Body)
	}

	// A client-side route survives a reload.
	rec, _ = get(t, h, "/apps/42")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<title>Streamlit</title>") {
		t.Errorf("/apps/42 = %d %q, want index.html", rec.Code, rec.Body)
	}

	// A missing asset is a real 404, not index.html served as JavaScript.
	if rec, _ = get(t, h, "/assets/missing.js"); rec.Code != 404 {
		t.Errorf("missing asset = %d, want 404", rec.Code)
	}

	// Path traversal stays inside the UI directory.
	if rec, _ = get(t, h, "/../../etc/passwd"); strings.Contains(rec.Body.String(), "root:") {
		t.Error("path traversal escaped the web root")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST / = %d, want 405", rec.Code)
	}
}

func TestNoWebDirServesNoUI(t *testing.T) {
	if rec, _ := get(t, NewRouter(Deps{DB: fakeDB{}}), "/"); rec.Code != 404 {
		t.Errorf("/ with no UI = %d, want 404", rec.Code)
	}
	if WebDir("") != nil {
		t.Error("WebDir(\"\") should be nil")
	}
}

type fakeOutbox apps.OutboxStats

func (f fakeOutbox) OutboxStats(context.Context) (apps.OutboxStats, error) {
	return apps.OutboxStats(f), nil
}

// Dashboard events: pending ones are reported; a failed one (given up on after its attempts) makes
// the module degraded, with the error, so an operator sees it.
func TestHealthz_Outbox(t *testing.T) {
	_, body := get(t, NewRouter(Deps{DB: fakeDB{}, Bus: fakeBus(events.StateConnected), Outbox: fakeOutbox{Pending: 2}}), "/healthz")
	if body["status"] != "ok" || body["eventsPending"] != "2" || body["eventsFailed"] != "0" {
		t.Errorf("pending: %v", body)
	}
	rec, body := get(t, NewRouter(Deps{DB: fakeDB{}, Bus: fakeBus(events.StateConnected), Outbox: fakeOutbox{Failed: 1, LastError: "nats: maximum payload exceeded"}}), "/healthz")
	if rec.Code != 200 || body["status"] != "degraded" || body["eventsFailed"] != "1" || body["eventsLastError"] != "nats: maximum payload exceeded" {
		t.Errorf("failed: %d %v", rec.Code, body)
	}
}
