package gate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// The loopback listener forwards /files/... to the backend with the app's bearer, replacing any
// Authorization the caller sent, and passes the path exactly as written: no cleaning of "..", no
// decoding of %2F, so the backend sees (and refuses) what app code asked for.
func TestLoopback_ForwardsFilesWithTheBearerAndTheRawPath(t *testing.T) {
	var got *http.Request
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()
	u, _ := url.Parse(backend.URL)
	lb := httptest.NewServer(Loopback(&Refresher{}, u, bearer))
	defer lb.Close()

	for _, raw := range []string{
		"/files/storage/b/sales/q1.csv?x=1",
		"/files/storage/b/sales/../other/secret.csv",
		"/files/storage/b/sales%2F..%2Fsecret.csv",
	} {
		req, _ := http.NewRequest(http.MethodPut, lb.URL+"/x", nil)
		req.URL.Opaque = "//" + req.URL.Host + raw
		req.Header.Set("Authorization", "Bearer app-code-guess")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || got == nil {
			t.Fatalf("%s: %d (a redirect means the path was cleaned)", raw, resp.StatusCode)
		}
		if got.Header.Get("Authorization") != "Bearer "+bearer {
			t.Errorf("%s: backend saw Authorization %q", raw, got.Header.Get("Authorization"))
		}
		if got.Method != http.MethodPut {
			t.Errorf("method changed to %s; the backend must see (and refuse) the real method", got.Method)
		}
		want, _, _ := strings.Cut(raw, "?")
		if got.URL.EscapedPath() != want {
			t.Errorf("backend saw %q, want %q exactly", got.URL.EscapedPath(), want)
		}
	}

	resp, _ := http.Get(lb.URL + "/elsewhere")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("unknown loopback path: %d", resp.StatusCode)
	}
	resp, _ = http.Get(lb.URL + StatusPath)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status path: %d", resp.StatusCode)
	}
}
