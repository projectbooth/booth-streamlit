package apps

import (
	"context"
	"errors"
	"testing"

	"github.com/projectbooth/booth-streamlit/internal/identity"
)

var (
	acmeOwner  = identity.Caller{Subject: "o", Workspace: "acme", Role: identity.RoleOwner}
	acmeViewer = identity.Caller{Subject: "v", Workspace: "acme", Role: identity.RoleViewer}
	otherOwner = identity.Caller{Subject: "x", Workspace: "other", Role: identity.RoleOwner}
)

func mustCreate(t *testing.T, s *Service, c identity.Caller, name string, shared bool) App {
	t.Helper()
	a, err := s.Create(context.Background(), c, Input{Name: name, Source: "import streamlit", Shared: shared})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestCreate_GivesEveryAppItsOwnBearer(t *testing.T) {
	s := NewService(NewMemoryStore(), 0)
	a, b := mustCreate(t, s, acmeOwner, "a", false), mustCreate(t, s, acmeOwner, "b", false)
	if len(a.GateBearer) != 64 || a.GateBearer == b.GateBearer {
		t.Fatalf("bearers %q %q: want 64 hex chars, distinct", a.GateBearer, b.GateBearer)
	}
}

// The cap counts apps that should have a container: Running and not suspended.
func TestCaps(t *testing.T) {
	ctx := context.Background()
	s := NewService(NewMemoryStore(), 0)
	s.SetCaps(Caps{MaxRunning: 2, MaxRunningPerWorkspace: 1})
	a1, a2 := mustCreate(t, s, acmeOwner, "a1", false), mustCreate(t, s, acmeOwner, "a2", false)
	o1, o2 := mustCreate(t, s, otherOwner, "o1", false), mustCreate(t, s, otherOwner, "o2", false)
	start := func(c identity.Caller, a App) error {
		_, err := s.SetDesiredState(ctx, c, a.ID, Running)
		return err
	}

	if err := start(acmeOwner, a1); err != nil {
		t.Fatal(err)
	}
	if err := start(acmeOwner, a1); err != nil {
		t.Errorf("starting an already-running app must not count it twice: %v", err)
	}
	if err := start(acmeOwner, a2); !errors.Is(err, ErrCapacity) {
		t.Errorf("second acme app: %v, want the per-workspace cap", err)
	}
	if err := start(otherOwner, o1); err != nil {
		t.Fatal(err)
	}
	if err := start(otherOwner, o2); !errors.Is(err, ErrCapacity) {
		t.Errorf("third app overall: %v, want the install cap", err)
	}
	// Stopping frees a slot; stopping is never capped.
	if _, err := s.SetDesiredState(ctx, acmeOwner, a1.ID, Stopped); err != nil {
		t.Fatal(err)
	}
	if err := start(acmeOwner, a2); err != nil {
		t.Errorf("after a stop: %v", err)
	}
}

// ADR 0105 and idle shutdown: suspending keeps the owner's choice; anyone who may open the app
// wakes it (within the cap); an owner-stopped app stays stopped for everyone else.
func TestSuspendAndWake(t *testing.T) {
	ctx := context.Background()
	s := NewService(NewMemoryStore(), 0)
	shared := mustCreate(t, s, acmeOwner, "shared", true)
	private := mustCreate(t, s, acmeOwner, "private", false)

	if _, err := s.Wake(ctx, acmeViewer, shared.ID); !errors.Is(err, ErrStopped) {
		t.Errorf("waking an owner-stopped app: %v, want ErrStopped", err)
	}
	if _, err := s.SetDesiredState(ctx, acmeOwner, shared.ID, Running); err != nil {
		t.Fatal(err)
	}
	if err := s.Suspend(ctx, "acme", shared.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, acmeOwner, shared.ID)
	if !got.Suspended || got.DesiredState != Running || Active(got) {
		t.Fatalf("after Suspend: %+v", got)
	}

	// The cap applies to waking too.
	s.SetCaps(Caps{MaxRunning: 1})
	other := mustCreate(t, s, acmeOwner, "other", false)
	if _, err := s.SetDesiredState(ctx, acmeOwner, other.ID, Running); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Wake(ctx, acmeViewer, shared.ID); !errors.Is(err, ErrCapacity) {
		t.Errorf("wake at the cap: %v, want ErrCapacity", err)
	}
	s.SetCaps(Caps{})

	woken, err := s.Wake(ctx, acmeViewer, shared.ID)
	if err != nil || woken.Suspended || !Active(woken) {
		t.Fatalf("viewer wake: %+v %v", woken, err)
	}
	// Visibility still applies: a viewer can't wake (or learn of) an unshared app, nor can
	// another workspace.
	if _, err := s.Wake(ctx, acmeViewer, private.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("viewer waking an unshared app: %v", err)
	}
	if _, err := s.Wake(ctx, otherOwner, shared.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("another workspace waking: %v", err)
	}

	// An owner's explicit Start clears suspension.
	_ = s.Suspend(ctx, "acme", shared.ID)
	if a, _ := s.SetDesiredState(ctx, acmeOwner, shared.ID, Running); a.Suspended {
		t.Error("Start left the app suspended")
	}
}

func TestOnChangeFiresOnEveryLifecycleRelevantWrite(t *testing.T) {
	ctx := context.Background()
	s := NewService(NewMemoryStore(), 0)
	n := 0
	var ids []string
	s.OnChange = func(id string) { n++; ids = append(ids, id) }
	a := mustCreate(t, s, acmeOwner, "a", true)
	_, _ = s.Update(ctx, acmeOwner, a.ID, Input{Name: "a", Source: "x", Shared: true})
	_, _ = s.SetDesiredState(ctx, acmeOwner, a.ID, Running)
	_ = s.Suspend(ctx, "acme", a.ID)
	_, _ = s.Wake(ctx, acmeViewer, a.ID)
	_ = s.Delete(ctx, acmeOwner, a.ID)
	if n != 6 {
		t.Errorf("OnChange fired %d times, want 6 (create, update, start, suspend, wake, delete)", n)
	}
	for _, id := range ids {
		if id != a.ID {
			t.Errorf("OnChange got id %q, want the changed app %q", id, a.ID)
		}
	}
	// Refused writes change nothing and notify nothing.
	n = 0
	b := mustCreate(t, s, acmeOwner, "b", true)
	n = 0
	_, _ = s.SetDesiredState(ctx, acmeViewer, b.ID, Running)
	if n != 0 {
		t.Error("a refused write fired OnChange")
	}
}
