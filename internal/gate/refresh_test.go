package gate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestRefresher_WritesRemovesAndReports(t *testing.T) {
	var mode atomic.Value
	mode.Store("ok")
	var sawBearer atomic.Value
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawBearer.Store(r.Header.Get("Authorization"))
		if mode.Load() == "paused" {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"error":"data_access_paused","reason":"owner gone"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "tok-1", "expiresAt": time.Now().Add(10 * time.Minute)})
	}))
	defer backend.Close()

	file := filepath.Join(t.TempDir(), "token")
	r := &Refresher{URL: backend.URL, Bearer: bearer, File: file}

	wait, err := r.once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sawBearer.Load() != "Bearer "+bearer {
		t.Error("the gate did not present the app's bearer")
	}
	if b, _ := os.ReadFile(file); string(b) != "tok-1" {
		t.Fatalf("token file %q", b)
	}
	if fi, _ := os.Stat(file); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o400 {
		t.Errorf("token file mode %v, want 0400", fi.Mode().Perm())
	}
	if wait < 6*time.Minute || wait > 7*time.Minute {
		t.Errorf("next fetch in %s, want about two thirds of 10m", wait)
	}
	if st := r.Status(); st.State != "ok" {
		t.Errorf("status %+v", st)
	}
	// No temp files left behind next to the token.
	if ents, _ := os.ReadDir(filepath.Dir(file)); len(ents) != 1 {
		t.Errorf("%d files in the token directory, want just the token", len(ents))
	}

	// Paused: the file goes (so the sidecar waits instead of being refused and exiting), the status
	// says why, and the gate asks again in 30s.
	mode.Store("paused")
	wait, err = r.once(context.Background())
	if err != nil || wait != 30*time.Second {
		t.Fatalf("paused: %s %v", wait, err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Error("the token file survived a refusal")
	}
	if st := r.Status(); st.State != "paused" || st.Reason != "owner gone" {
		t.Errorf("status %+v", st)
	}

	rec := httptest.NewRecorder()
	r.StatusHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, StatusPath, nil))
	if rec.Code != 200 || !json.Valid(rec.Body.Bytes()) {
		t.Errorf("status endpoint %d %s", rec.Code, rec.Body)
	}
}

func TestRefresher_BackendErrorKeepsTheFile(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer backend.Close()
	file := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(file, []byte("still-valid"), 0o400)
	r := &Refresher{URL: backend.URL, Bearer: bearer, File: file}
	if _, err := r.once(context.Background()); err == nil {
		t.Fatal("a 503 was not an error")
	}
	if b, _ := os.ReadFile(file); string(b) != "still-valid" {
		t.Error("a transient backend error removed a still-valid token")
	}
}
