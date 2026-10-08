package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Refresher keeps the app's workload token on disk for the credential sidecars (ADR 0107 item 1).
//
// The token file lives on a memory-backed volume mounted only into the gate (read-write) and the
// sidecars (read-only), never into the Streamlit container. The sidecars re-read it on every broker
// call (booth-core's sidecar FileToken), so writing a fresh one is all renewal takes; no Kubernetes
// object ever holds the token.
//
// While the owner has no access the backend answers 403, and the refresher removes the file rather
// than leave a dead token in it: a sidecar whose first lease is refused by the broker exits (and
// would crash-loop the pod), but one that can't read its token file waits and retries.
type Refresher struct {
	URL    string // the backend's POST /internal/token
	Bearer string
	File   string
	// MaxInterval, if set, caps the time between fetches (a test knob; normally two thirds of the
	// token's life).
	MaxInterval time.Duration
	HTTP        *http.Client

	mu     sync.Mutex
	status Status
}

// Status is what the app can learn about its data access: whether the owner's access is working.
type Status struct {
	State  string `json:"state"` // "ok", "paused", "waiting"
	Reason string `json:"reason,omitempty"`
}

// Run fetches and writes the token until ctx ends.
func (r *Refresher) Run(ctx context.Context) {
	r.set(Status{State: "waiting"})
	backoff := 2 * time.Second
	for {
		wait, err := r.once(ctx)
		if err != nil {
			log.Printf("gate: token refresh: %v (retrying in %s)", err, backoff)
			wait, backoff = backoff, min(backoff*2, 30*time.Second)
		} else {
			backoff = 2 * time.Second
		}
		if r.MaxInterval > 0 && wait > r.MaxInterval {
			wait = r.MaxInterval
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// once does one fetch and returns how long to wait before the next.
func (r *Refresher) once(ctx context.Context) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.URL, nil)
	req.Header.Set("Authorization", "Bearer "+r.Bearer)
	hc := r.HTTP
	if hc == nil {
		hc = &http.Client{}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch resp.StatusCode {
	case http.StatusOK:
		var tok struct {
			Token     string    `json:"token"`
			ExpiresAt time.Time `json:"expiresAt"`
		}
		if err := json.Unmarshal(raw, &tok); err != nil || tok.Token == "" {
			return 0, errors.New("unreadable token response")
		}
		if err := writeAtomic(r.File, tok.Token); err != nil {
			return 0, err
		}
		r.set(Status{State: "ok"})
		left := time.Until(tok.ExpiresAt)
		return max(left*2/3, 5*time.Second), nil
	case http.StatusForbidden:
		var why struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(raw, &why)
		if err := os.Remove(r.File); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("gate: removing the token file: %v", err)
		}
		r.set(Status{State: "paused", Reason: why.Reason})
		log.Printf("gate: data access paused: %s", why.Reason)
		return 30 * time.Second, nil
	default:
		return 0, fmt.Errorf("backend answered %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
}

// writeAtomic writes s to path so a reader sees the old or the new token, never half of one. 0400:
// only this uid (the sidecars run as the same uid) can read it.
func writeAtomic(path, s string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".token-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(s); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o400); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (r *Refresher) set(s Status) {
	r.mu.Lock()
	r.status = s
	r.mu.Unlock()
}

// Status reports the current state.
func (r *Refresher) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// StatusPath is served on the gate's loopback-only listener, for booth_streamlit's helpers.
const StatusPath = "/_booth/data/status"

// StatusHandler answers StatusPath. It is served on 127.0.0.1 only, inside the pod, and carries no
// secret: just whether data access works, and if not why.
func (r *Refresher) StatusHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+StatusPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(r.Status())
	})
	return mux
}
