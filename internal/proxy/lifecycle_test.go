package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/projectbooth/booth-streamlit/internal/apps"
	"github.com/projectbooth/booth-streamlit/internal/gate"
	"github.com/projectbooth/booth-streamlit/internal/identity"
	"github.com/projectbooth/booth-streamlit/internal/lifecycle"
)

// recorder is an Activity that counts.
type recorder struct {
	mu                   sync.Mutex
	touches, opened, cls int
}

func (r *recorder) Touch(string)           { r.mu.Lock(); r.touches++; r.mu.Unlock() }
func (r *recorder) WebsocketOpened(string) { r.mu.Lock(); r.opened++; r.mu.Unlock() }
func (r *recorder) WebsocketClosed(string) { r.mu.Lock(); r.cls++; r.mu.Unlock() }
func (r *recorder) counts() (int, int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.touches, r.opened, r.cls
}

// The backend presents the app's bearer to the gate, so the gate's check can pass.
func TestProxy_SendsTheBearerToTheGate(t *testing.T) {
	a := newApp(t)
	srv := newProxy(t, a)
	if resp := do(t, srv, "/apps/demo/", map[string]string{identity.HeaderIdentity: "good-acme"}); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := a.last.Header.Get(gate.HeaderToken); got != "the-bearer" {
		t.Errorf("gate token = %q, want the app's bearer", got)
	}
	// A client can't supply its own: any inbound value is replaced, never forwarded as-is.
	do(t, srv, "/apps/demo/", map[string]string{identity.HeaderIdentity: "good-acme", gate.HeaderToken: "client-guess"})
	if got := a.last.Header.Get(gate.HeaderToken); got != "the-bearer" {
		t.Errorf("client-supplied gate token reached the app: %q", got)
	}
}

func TestProxy_RecordsActivityAndCapsWebsockets(t *testing.T) {
	a := newApp(t)
	u, _ := url.Parse(a.srv.URL)
	rec := &recorder{}
	r := chi.NewRouter()
	(&Handler{Verifier: fakeVerifier{}, Apps: fixed{"demo": {ID: "demo", Workspace: "acme", Target: u}}, Activity: rec, MaxWebsocket: 300 * time.Millisecond}).Mount(r)
	srv := httptest.NewServer(r)
	defer srv.Close()

	do(t, srv, "/apps/demo/", map[string]string{identity.HeaderIdentity: "good-acme"})
	if touches, _, _ := rec.counts(); touches != 1 {
		t.Errorf("touches = %d after one request", touches)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/apps/demo/_stcore/stream",
		&websocket.DialOptions{HTTPHeader: http.Header{identity.HeaderIdentity: {"good-acme"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	time.Sleep(50 * time.Millisecond)
	if _, opened, closed := rec.counts(); opened != 1 || closed != 0 {
		t.Errorf("while open: opened=%d closed=%d", opened, closed)
	}
	// The proxy ends the connection at MaxWebsocket, even though neither side closed it.
	start := time.Now()
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("read succeeded; the websocket should have been cut")
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Errorf("connection lived %s, cap was 300ms", waited)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, closed := rec.counts(); closed == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("websocket close was not recorded")
}

// The lifecycle resolver end to end against the app model and a (fake) cluster.
func TestLifecycleResolver(t *testing.T) {
	ctx := context.Background()
	svc := apps.NewService(apps.NewMemoryStore(), 0)
	backend := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "booth-streamlit", Namespace: "ns", UID: "u"}}
	client := fake.NewSimpleClientset(backend)
	ctrl := lifecycle.New(lifecycle.Config{Namespace: "ns", RuntimeImage: "r", GateImage: "g", TmpSizeLimit: "64Mi", Owner: backend}, client, svc)
	owner := identity.Caller{Subject: "o", Workspace: "acme", Role: identity.RoleOwner}
	shared, _ := svc.Create(ctx, owner, apps.Input{Name: "shared", Source: "x", Shared: true})
	private, _ := svc.Create(ctx, owner, apps.Input{Name: "private", Source: "x"})

	r := chi.NewRouter()
	(&Handler{Verifier: fakeVerifier{}, Apps: LifecycleResolver{Apps: svc, Lifecycle: ctrl, Namespace: "ns"}}).Mount(r)
	srv := httptest.NewServer(r)
	defer srv.Close()
	page := func(id, who string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/apps/"+id+"/", nil)
		req.Header.Set(identity.HeaderIdentity, who)
		req.Header.Set("Sec-Fetch-Dest", "iframe")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// fakeVerifier makes every caller a viewer of the named workspace.
	if code, body := page(shared.ID, "good-acme"); code != 503 || !strings.Contains(body, "stopped") || strings.Contains(body, "refresh") {
		t.Errorf("owner-stopped app: %d %q", code, body)
	}
	if code, _ := page(private.ID, "good-acme"); code != 404 {
		t.Errorf("unshared app for a viewer: %d", code)
	}
	if code, _ := page(shared.ID, "good-other"); code != 404 {
		t.Errorf("another workspace: %d", code)
	}

	_, _ = svc.SetDesiredState(ctx, owner, shared.ID, apps.Running)
	if code, body := page(shared.ID, "good-acme"); code != 503 || !strings.Contains(body, `http-equiv="refresh"`) {
		t.Errorf("starting app: %d %q (want a self-refreshing page)", code, body)
	}

	// Idle-suspended: a viewer's visit wakes it.
	_ = svc.Suspend(ctx, "acme", shared.ID)
	page(shared.ID, "good-acme")
	if a, _ := svc.Get(ctx, owner, shared.ID); a.Suspended {
		t.Error("a viewer's visit did not wake the suspended app")
	}

	// At the cap, a wake is refused with the reason, not silently.
	_ = svc.Suspend(ctx, "acme", shared.ID)
	other, _ := svc.Create(ctx, owner, apps.Input{Name: "other", Source: "x"})
	_, _ = svc.SetDesiredState(ctx, owner, other.ID, apps.Running)
	svc.SetCaps(apps.Caps{MaxRunning: 1})
	if code, body := page(shared.ID, "good-acme"); code != 503 || !strings.Contains(body, "Too many apps") {
		t.Errorf("wake at the cap: %d %q", code, body)
	}
}

// answers resolves every id to one error.
type answers struct{ err error }

func (a answers) Resolve(context.Context, identity.Caller, string) (App, error) { return App{}, a.err }

// A viewer on the starting page counts as activity (a long install isn't suspended under them); a
// failed or stopped app's page doesn't, so a broken app still goes idle.
func TestProxy_StartingPageIsActivityFailedIsNot(t *testing.T) {
	for _, tc := range []struct {
		err     error
		touches int
	}{{ErrStarting, 1}, {&FailedError{Reason: "pip install failed"}, 0}, {ErrNotRunning, 0}} {
		rec := &recorder{}
		r := chi.NewRouter()
		(&Handler{Verifier: fakeVerifier{}, Apps: answers{tc.err}, Activity: rec}).Mount(r)
		srv := httptest.NewServer(r)
		do(t, srv, "/apps/demo/", map[string]string{identity.HeaderIdentity: "good-acme"})
		srv.Close()
		if touches, _, _ := rec.counts(); touches != tc.touches {
			t.Errorf("%v: touches = %d, want %d", tc.err, touches, tc.touches)
		}
	}
}
