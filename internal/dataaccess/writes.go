package dataaccess

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/projectbooth/booth-streamlit/internal/apps"
)

// Writes is the write proxy (ADR 0117 items 2 and 7, amended by item 1a; docs/design-app-writes.md
// item 1). App code calls its gate on loopback (booth_streamlit.api); the gate adds the app's bearer
// and forwards here. The backend identifies the app by the bearer, refuses anything the app's
// owner hasn't declared, and forwards the rest to booth-api through booth-core's gateway with the
// app's existing workload token, unchanged (ADR 0117 "Reconciled design" item 2). booth-api then
// requires its own live grant for the app: a write needs both.
//
//	POST   /writes/{api}/rows                   insert       -> POST   /modules/api/app/v1/{api}/rows
//	PATCH  /writes/{api}/rows/{key}             update       -> PATCH  /modules/api/app/v1/{api}/rows/{key}
//	DELETE /writes/{api}/rows/{key}             delete       -> DELETE /modules/api/app/v1/{api}/rows/{key}
//	PATCH|DELETE /writes/{api}/rows?key[f]=v    composite keys, or keys containing "/"
//
// booth-api's GraphQL write route is not forwarded: one GraphQL request can name any enabled
// operation, so the declaration's operations couldn't be enforced here without parsing it.
//
// Forwarded headers: Content-Type, Idempotency-Key, If-Match, and X-Booth-App-User (stated by the
// app's code; booth-api's audit, and this module's "Write allowed" prompt (Uses); never used for
// authorization). booth-api's answer is relayed as is,
// except that a not_granted refusal says in plain words what is missing.
//
// Refused here: an API or operation the app doesn't declare (403 not_a_write_target), any other
// method (405), a malformed path, key or query (400), a body over MaxBodyBytes (413), more than
// MaxConcurrent writes at once for one app (429), an unknown bearer (401), a paused owner (403).
type Writes struct {
	// GatewayURL is booth-core's base URL; requests go to <GatewayURL>/modules/api/app/v1/...
	GatewayURL    string
	Timeout       time.Duration
	MaxConcurrent int
	HTTP          *http.Client
	// Uses, if set, records each successful write for the page's "Write allowed" prompt.
	Uses *Uses

	mu   sync.Mutex
	busy map[string]int // app id -> writes in flight
}

const (
	// MaxWriteBodyBytes is booth-api's own limit (one row, 64 KiB); larger is refused here.
	MaxWriteBodyBytes     = 64 << 10
	DefaultWriteTimeout   = 30 * time.Second
	maxAppUserLen         = 256
	maxIdempotencyKeyLen  = 255
	maxIfMatchLen         = 200
	writeErrorBodyMaxSize = 64 << 10
)

var (
	keyParamPattern = regexp.MustCompile(`^key\[[A-Za-z0-9_]{1,128}\]$`)
	// printable ASCII without control characters: what may go in a forwarded header.
	headerValuePattern = regexp.MustCompile(`^[\x20-\x7e]+$`)
	apiSlugPattern     = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
)

func (wr *Writes) mount(r chi.Router, in *Internal) {
	r.HandleFunc("/writes/*", func(w http.ResponseWriter, req *http.Request) {
		// Check the request's shape before anything else: a malformed one costs no token.
		t, ok := parseWrite(w, req)
		if !ok {
			return
		}
		a, tok, ok := in.appFor(w, req)
		if !ok {
			return
		}
		if !a.Allows(t.api, t.op) {
			log.Printf("writes: app=%s workspace=%s refused: %s on API %q is not declared", a.ID, a.Workspace, t.op, t.api)
			writeWriteError(w, http.StatusForbidden, "not_a_write_target",
				fmt.Sprintf("this app's owner hasn't declared %q on API %q in the app's Write targets", t.op, t.api))
			return
		}
		if !wr.acquire(a.ID) {
			writeWriteError(w, http.StatusTooManyRequests, "too_many_writes", fmt.Sprintf("at most %d writes at once per app", wr.maxConcurrent()))
			return
		}
		defer wr.release(a.ID)
		wr.forward(w, req, a, tok, t)
	})
}

// writeCall is a parsed write request.
type writeCall struct {
	api      string
	op       string
	rest     string // the escaped path after /writes/{api}: "/rows" or "/rows/{key}"
	rawQuery string
}

func parseWrite(w http.ResponseWriter, r *http.Request) (writeCall, bool) {
	bad := func(msg string) (writeCall, bool) {
		writeWriteError(w, http.StatusBadRequest, "invalid_request", msg)
		return writeCall{}, false
	}
	raw, ok := strings.CutPrefix(r.URL.EscapedPath(), "/writes/")
	if !ok {
		return bad("malformed path")
	}
	segs := strings.Split(raw, "/")
	if !apiSlugPattern.MatchString(segs[0]) {
		return bad("invalid API slug")
	}
	if len(segs) < 2 || segs[1] != "rows" || len(segs) > 3 {
		return bad("the write proxy serves /writes/{api}/rows and /writes/{api}/rows/{key} only")
	}
	t := writeCall{api: segs[0], rest: "/rows"}
	hasKey := len(segs) == 3
	if hasKey {
		key := segs[2]
		lower := strings.ToLower(key)
		dec, err := url.PathUnescape(key)
		switch {
		case key == "" || err != nil || dec == "." || dec == "..":
			return bad("invalid row key")
		case strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") || strings.Contains(lower, "%00"):
			return bad(`a key containing "/" is sent as ?key[field]=value`)
		}
		t.rest += "/" + key
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return bad("malformed query")
	}
	for name, vals := range q {
		if !keyParamPattern.MatchString(name) || len(vals) != 1 {
			return bad("the only query parameters are key[field]=value, once each")
		}
	}
	hasQueryKey := len(q) > 0
	if hasKey && hasQueryKey {
		return bad("give the key in the path or the query, not both")
	}
	t.rawQuery = q.Encode()
	switch {
	case r.Method == http.MethodPost && !hasKey && !hasQueryKey:
		t.op = apps.OpInsert
	case r.Method == http.MethodPatch && (hasKey || hasQueryKey):
		t.op = apps.OpUpdate
	case r.Method == http.MethodDelete && (hasKey || hasQueryKey):
		t.op = apps.OpDelete
	default:
		writeWriteError(w, http.StatusMethodNotAllowed, "invalid_request", "POST /rows inserts; PATCH and DELETE need a row key; nothing is read here")
		return writeCall{}, false
	}
	return t, true
}

func (wr *Writes) forward(w http.ResponseWriter, r *http.Request, a apps.App, tok Token, t writeCall) {
	var body []byte
	if t.op != apps.OpDelete {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, MaxWriteBodyBytes+1))
		if err != nil {
			writeWriteError(w, http.StatusBadRequest, "invalid_request", "unreadable body")
			return
		}
		if len(body) > MaxWriteBodyBytes {
			writeWriteError(w, http.StatusRequestEntityTooLarge, "invalid_request", fmt.Sprintf("a write is one row of at most %d bytes", MaxWriteBodyBytes))
			return
		}
	}
	target := strings.TrimRight(wr.GatewayURL, "/") + "/modules/api/app/v1/" + t.api + t.rest
	if t.rawQuery != "" {
		target += "?" + t.rawQuery
	}
	ctx, cancel := context.WithTimeout(r.Context(), wr.timeout())
	defer cancel()
	out, err := http.NewRequestWithContext(ctx, r.Method, target, bytes.NewReader(body))
	if err != nil {
		writeWriteError(w, http.StatusBadRequest, "invalid_request", "malformed request")
		return
	}
	out.Header.Set("Authorization", "Bearer "+tok.JWT)
	out.Header.Set("X-Workspace", a.Workspace)
	if t.op != apps.OpDelete {
		out.Header.Set("Content-Type", "application/json")
	}
	for _, h := range []struct {
		name string
		max  int
	}{{"Idempotency-Key", maxIdempotencyKeyLen}, {"If-Match", maxIfMatchLen}, {"X-Booth-App-User", maxAppUserLen}} {
		v := r.Header.Get(h.name)
		if v == "" {
			continue
		}
		if len(v) > h.max || !headerValuePattern.MatchString(v) {
			writeWriteError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("%s must be at most %d printable ASCII characters", h.name, h.max))
			return
		}
		out.Header.Set(h.name, v)
	}
	hc := wr.HTTP
	if hc == nil {
		hc = &http.Client{}
	}
	start := time.Now()
	resp, err := hc.Do(out)
	if err != nil {
		log.Printf("writes: app=%s workspace=%s api=%s op=%s: %v", a.ID, a.Workspace, t.api, t.op, err)
		code := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			code = http.StatusGatewayTimeout
		}
		writeWriteError(w, code, "unavailable", "booth-api could not be reached")
		return
	}
	defer resp.Body.Close()
	// Never the body: row values are not logged (ADR 0117 item 5).
	log.Printf("writes: app=%s workspace=%s api=%s op=%s status=%d duration=%s", a.ID, a.Workspace, t.api, t.op, resp.StatusCode, time.Since(start).Round(time.Millisecond))
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, writeErrorBodyMaxSize))
	if resp.StatusCode == http.StatusForbidden {
		respBody = explainNotGranted(respBody, a, t.api)
	}
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		wr.Uses.Record(a.ID, out.Header.Get("X-Booth-App-User"), Use{Kind: "write", API: t.api})
	}
	for _, h := range []string{"Content-Type", "ETag", "Retry-After", "Idempotent-Replayed"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

// NotGrantedMessage is what a not_granted refusal says (ADR 0117 item 1a): the app's declaration is
// only half of it, an owner also grants the app on the API page.
func NotGrantedMessage(a apps.App, api string) string {
	return fmt.Sprintf("This app has no write grant on API %q. A write also needs an owner's grant on the API page: in the API module, open %q, then Writes, and grant %s.",
		api, api, apps.Principal(a.Workspace, a.ID))
}

// explainNotGranted rewrites booth-api's not_granted message in plain words, keeping its code;
// any other body is returned unchanged.
func explainNotGranted(body []byte, a apps.App, api string) []byte {
	var e struct {
		Error map[string]any `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil || e.Error == nil || e.Error["code"] != "not_granted" {
		return body
	}
	e.Error["message"] = NotGrantedMessage(a, api)
	out, err := json.Marshal(e)
	if err != nil {
		return body
	}
	return out
}

// writeWriteError answers in booth-api's error shape, so app code reads one shape for both layers.
func writeWriteError(w http.ResponseWriter, code int, errCode, msg string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, code, map[string]any{"error": map[string]string{"code": errCode, "message": msg}})
}

func (wr *Writes) acquire(id string) bool {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	if wr.busy == nil {
		wr.busy = map[string]int{}
	}
	if wr.busy[id] >= wr.maxConcurrent() {
		return false
	}
	wr.busy[id]++
	return true
}

func (wr *Writes) release(id string) {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	if wr.busy[id]--; wr.busy[id] <= 0 {
		delete(wr.busy, id)
	}
}

func (wr *Writes) maxConcurrent() int {
	if wr.MaxConcurrent > 0 {
		return wr.MaxConcurrent
	}
	return DefaultMaxConcurrent
}

func (wr *Writes) timeout() time.Duration {
	if wr.Timeout > 0 {
		return wr.Timeout
	}
	return DefaultWriteTimeout
}
