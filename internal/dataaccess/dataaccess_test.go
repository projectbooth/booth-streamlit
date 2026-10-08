package dataaccess

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/projectbooth/booth-streamlit/internal/apps"
	"github.com/projectbooth/booth-streamlit/internal/identity"
)

// fakeMinter mints a JWT-shaped token whose sub is the requested subject, records calls, and can
// be told to refuse an owner or to grant too much.
type fakeMinter struct {
	mu      sync.Mutex
	calls   []string
	refuse  map[string]bool // owner -> refuse
	role    string
	now     func() time.Time
	ttl     time.Duration
	failing bool
}

func fakeJWT(sub string) string {
	p, _ := json.Marshal(map[string]string{"sub": sub})
	return "h." + base64.RawURLEncoding.EncodeToString(p) + ".s"
}

func (m *fakeMinter) Mint(_ context.Context, ws, subject, owner string) (Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, ws+"|"+subject+"|"+owner)
	if m.failing {
		return Token{}, errors.New("core unreachable")
	}
	if m.refuse[owner] {
		return Token{}, ErrOwnerNoAccess
	}
	role := m.role
	if role == "" {
		role = "viewer"
	}
	return Token{JWT: fakeJWT(subject), ExpiresAt: m.now().Add(m.ttl), Role: role}, nil
}

func (m *fakeMinter) n() int { m.mu.Lock(); defer m.mu.Unlock(); return len(m.calls) }

type rig struct {
	clock time.Time
	m     *fakeMinter
	tok   *Tokens
	svc   *apps.Service
	store *apps.MemoryStore
	ch    []string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{clock: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	now := func() time.Time { return r.clock }
	r.m = &fakeMinter{refuse: map[string]bool{}, now: now, ttl: 10 * time.Minute}
	r.tok = &Tokens{Minter: r.m, Now: now}
	r.store = apps.NewMemoryStore()
	r.svc = apps.NewService(r.store, 0)
	r.svc.OnChange = func(id string) { r.ch = append(r.ch, id) }
	return r
}

func (r *rig) app(t *testing.T, ws, owner string) apps.App {
	t.Helper()
	a, err := r.svc.Create(context.Background(), identity.Caller{Subject: owner, Workspace: ws, Role: identity.RoleOwner},
		apps.Input{Name: "a", Source: "x", Shared: true})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestSubject(t *testing.T) {
	// Core's own rule (internal/workload/service.go subjectPattern) must admit it.
	got := Subject("acme-analytics", "a2b3c4d5e6f7g")
	if got != "streamlit:acme-analytics:a2b3c4d5e6f7g" {
		t.Fatalf("Subject = %q", got)
	}
}

func TestTokens_RemintAtTwoThirdsAndOnOwnerChange(t *testing.T) {
	r := newRig(t)
	a := r.app(t, "acme", "alice")
	ctx := context.Background()
	t1, err := r.tok.Get(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	r.clock = r.clock.Add(6 * time.Minute) // < 2/3 of 10m
	if t2, _ := r.tok.Get(ctx, a); t2 != t1 || r.m.n() != 1 {
		t.Fatalf("re-minted before two thirds of its life (%d calls)", r.m.n())
	}
	r.clock = r.clock.Add(time.Minute) // 7m > 6m40s
	if _, _ = r.tok.Get(ctx, a); r.m.n() != 2 {
		t.Fatalf("not re-minted after two thirds of its life (%d calls)", r.m.n())
	}
	a.Owner = "bob" // a take-over
	if _, _ = r.tok.Get(ctx, a); r.m.n() != 3 || !strings.HasSuffix(r.m.calls[2], "|bob") {
		t.Fatalf("not re-minted for the new owner: %v", r.m.calls)
	}
	if !strings.Contains(r.m.calls[0], "|streamlit:acme:"+a.ID+"|") {
		t.Errorf("subject %v", r.m.calls[0])
	}
}

func TestTokens_RefusalsAreRememberedAndTooMuchIsRefused(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	a := r.app(t, "acme", "alice")
	r.m.refuse["alice"] = true
	for i := 0; i < 5; i++ {
		if _, err := r.tok.Get(ctx, a); !errors.Is(err, ErrOwnerNoAccess) {
			t.Fatal(err)
		}
	}
	if r.m.n() != 1 {
		t.Errorf("a burst on a paused app cost %d mints, want 1", r.m.n())
	}
	r.clock = r.clock.Add(RefusalMemory)
	r.m.refuse["alice"] = false
	if _, err := r.tok.Get(ctx, a); err != nil {
		t.Errorf("after the refusal window: %v", err)
	}

	b := r.app(t, "acme", "carol")
	r.m.role = "owner"
	if _, err := r.tok.Get(ctx, b); !errors.Is(err, ErrRoleExceeded) {
		t.Errorf("a token above viewer was used: %v", err)
	}
}

func TestTokens_TransientFailureKeepsAValidToken(t *testing.T) {
	r := newRig(t)
	a := r.app(t, "acme", "alice")
	ctx := context.Background()
	t1, _ := r.tok.Get(ctx, a)
	r.clock = r.clock.Add(7 * time.Minute)
	r.m.failing = true
	if t2, err := r.tok.Get(ctx, a); err != nil || t2 != t1 {
		t.Fatalf("core down while a token is still valid: %v", err)
	}
	r.clock = r.clock.Add(4 * time.Minute) // past expiry
	if _, err := r.tok.Get(ctx, a); err == nil {
		t.Fatal("served an expired token")
	}
}

func post(h http.Handler, path, auth string, hdr map[string]string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestInternalToken(t *testing.T) {
	r := newRig(t)
	in := &Internal{Apps: r.svc, Tokens: r.tok}
	h := in.Router()
	a := r.app(t, "acme", "alice")
	b := r.app(t, "acme", "bob")

	for name, auth := range map[string]string{"none": "", "short": "abc", "unknown": strings.Repeat("0", 64)} {
		if rec := post(h, "/internal/token", auth, nil, ""); rec.Code != 401 {
			t.Errorf("%s bearer: %d", name, rec.Code)
		}
	}
	// Forged identity headers are ignored: only the bearer identifies an app.
	if rec := post(h, "/internal/token", "", map[string]string{"X-Booth-User": "alice", "X-Workspace": "acme"}, ""); rec.Code != 401 {
		t.Errorf("forged headers without a bearer: %d", rec.Code)
	}

	rec := post(h, "/internal/token", a.GateBearer, nil, "")
	var got Token
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("A's bearer: %d %s", rec.Code, rec.Body)
	}
	if sub, _ := unverifiedSubject(got.JWT); sub != Subject("acme", a.ID) {
		t.Errorf("A got a token for %q", sub)
	}
	// B's bearer (stolen) gets B's token, never A's: the bearer is the app's whole identity.
	rec = post(h, "/internal/token", b.GateBearer, nil, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if sub, _ := unverifiedSubject(got.JWT); sub != Subject("acme", b.ID) {
		t.Errorf("B's bearer got a token for %q", sub)
	}

	// The owner loses access: 403 with the reason, the app is marked paused, its epoch bumped once
	// (the pod rolls), and the lifecycle is told.
	r.m.refuse["alice"] = true
	r.clock = r.clock.Add(7 * time.Minute)
	r.ch = nil
	rec = post(h, "/internal/token", a.GateBearer, nil, "")
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "data_access_paused") {
		t.Fatalf("paused owner: %d %s", rec.Code, rec.Body)
	}
	paused, _ := r.store.Get(context.Background(), "acme", a.ID)
	if paused.DataPausedReason == "" || paused.DataEpoch != 1 || len(r.ch) != 1 {
		t.Fatalf("pause not recorded: %+v changes=%v", paused, r.ch)
	}
	r.clock = r.clock.Add(RefusalMemory)
	post(h, "/internal/token", a.GateBearer, nil, "")
	if again, _ := r.store.Get(context.Background(), "acme", a.ID); again.DataEpoch != 1 {
		t.Error("a repeated refusal rolled the pod again")
	}

	// Access comes back: the pause clears.
	r.m.refuse["alice"] = false
	r.clock = r.clock.Add(RefusalMemory)
	if rec := post(h, "/internal/token", a.GateBearer, nil, ""); rec.Code != 200 {
		t.Fatalf("after access returned: %d", rec.Code)
	}
	if back, _ := r.store.Get(context.Background(), "acme", a.ID); back.DataPausedReason != "" {
		t.Error("pause not cleared")
	}
}

func TestInternalBroker(t *testing.T) {
	r := newRig(t)
	a := r.app(t, "acme", "alice")
	var forwarded *http.Request
	var forwardedBody string
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		forwarded = req.Clone(context.Background())
		b, _ := io.ReadAll(req.Body)
		forwardedBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"leaseId":"l1","credential":{"password":"never-logged"}}`))
	}))
	defer core.Close()
	h := (&Internal{Apps: r.svc, Tokens: r.tok, CoreURL: core.URL}).Router()
	tokA := fakeJWT(Subject("acme", a.ID))
	ws := map[string]string{"X-Workspace": "acme"}
	good := `{"kind":"postgres","access":"read","scope":{"workspace":"acme"}}`

	cases := []struct {
		name, tok, body string
		hdr             map[string]string
		want            int
	}{
		{"no token", "", good, ws, 401},
		{"garbage token", "x", good, ws, 401},
		{"another module's token", fakeJWT("apikeys:acme"), good, ws, 403},
		{"a person's token", fakeJWT("3f1c-uuid"), good, ws, 403},
		{"app that doesn't exist", fakeJWT(Subject("acme", "anope")), good, ws, 403},
		{"workspace header mismatch", tokA, good, map[string]string{"X-Workspace": "other"}, 403},
		{"readwrite (viewer cap)", tokA, `{"kind":"postgres","access":"readwrite","scope":{"workspace":"acme"}}`, ws, 403},
		{"another workspace's database", tokA, `{"kind":"postgres","access":"read","scope":{"workspace":"other"}}`, ws, 403},
		{"unknown kind", tokA, `{"kind":"gcs","access":"read","scope":{}}`, ws, 403},
	}
	for _, c := range cases {
		forwarded = nil
		if rec := post(h, "/internal/broker/api/credentials", c.tok, c.hdr, c.body); rec.Code != c.want {
			t.Errorf("%s: %d, want %d (%s)", c.name, rec.Code, c.want, rec.Body)
		}
		if forwarded != nil {
			t.Errorf("%s: forwarded to core", c.name)
		}
	}

	rec := post(h, "/internal/broker/api/credentials", tokA, ws, good)
	if rec.Code != 201 || forwarded == nil {
		t.Fatalf("good request: %d %s", rec.Code, rec.Body)
	}
	if forwarded.URL.Path != "/api/credentials" || forwarded.Header.Get("Authorization") != "Bearer "+tokA ||
		forwarded.Header.Get("X-Workspace") != "acme" || forwardedBody != good {
		t.Errorf("forwarded %s %v %q", forwarded.URL.Path, forwarded.Header, forwardedBody)
	}
}
