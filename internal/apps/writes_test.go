package apps

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/projectbooth/booth-streamlit/internal/identity"
)

var acmeAlice = identity.Caller{Subject: "alice", Workspace: "acme", Role: identity.RoleOwner}

func TestSetWriteTargets(t *testing.T) {
	ctx := context.Background()
	s := NewService(NewMemoryStore(), 0)
	a := mustCreate(t, s, acmeOwner, "finance", true)
	var changed []string
	s.OnChange = func(id string) { changed = append(changed, id) }

	one := []WriteTarget{{API: "household", Operations: []string{"update", "insert", "insert"}}}
	// Adding a target needs the plain-words confirmation.
	if _, err := s.SetWriteTargets(ctx, acmeOwner, a.ID, WriteTargetsInput{Targets: one}); !errors.Is(err, ErrConfirmationRequired) {
		t.Fatalf("unconfirmed add: %v", err)
	}
	got, err := s.SetWriteTargets(ctx, acmeOwner, a.ID, WriteTargetsInput{Targets: one, Confirmed: true})
	want := []WriteTarget{{API: "household", Operations: []string{"insert", "update"}}}
	if err != nil || !reflect.DeepEqual(got.WriteTargets, want) || got.WriteTargetsBy != "o" {
		t.Fatalf("confirmed add: %+v %v (operations canonical, deduplicated)", got.WriteTargets, err)
	}
	if len(changed) != 1 {
		t.Errorf("OnChange calls = %v, want one (the pod's egress follows the declaration)", changed)
	}
	// Adding an operation is widening too.
	wider := []WriteTarget{{API: "household", Operations: []string{"insert", "update", "delete"}}}
	if _, err := s.SetWriteTargets(ctx, acmeOwner, a.ID, WriteTargetsInput{Targets: wider}); !errors.Is(err, ErrConfirmationRequired) {
		t.Errorf("unconfirmed widening: %v", err)
	}
	// Narrowing and removing need no confirmation.
	if got, err := s.SetWriteTargets(ctx, acmeOwner, a.ID, WriteTargetsInput{Targets: []WriteTarget{{API: "household", Operations: []string{"insert"}}}}); err != nil || !got.Allows("household", OpInsert) || got.Allows("household", OpUpdate) {
		t.Errorf("narrowing: %+v %v", got.WriteTargets, err)
	}
	if got, err := s.SetWriteTargets(ctx, acmeOwner, a.ID, WriteTargetsInput{}); err != nil || got.Writes() {
		t.Errorf("removing: %+v %v", got.WriteTargets, err)
	}

	// Owners only, and only in their workspace.
	if _, err := s.SetWriteTargets(ctx, acmeViewer, a.ID, WriteTargetsInput{Targets: one, Confirmed: true}); !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrForbidden) {
		t.Errorf("viewer: %v", err)
	}
	if _, err := s.SetWriteTargets(ctx, otherOwner, a.ID, WriteTargetsInput{Targets: one, Confirmed: true}); !errors.Is(err, ErrNotFound) {
		t.Errorf("other workspace: %v", err)
	}

	for name, bad := range map[string][]WriteTarget{
		"bad slug":      {{API: "Household", Operations: []string{"insert"}}},
		"path in slug":  {{API: "a/b", Operations: []string{"insert"}}},
		"no operations": {{API: "household"}},
		"unknown op":    {{API: "household", Operations: []string{"insert", "truncate"}}},
		"twice":         {{API: "x", Operations: []string{"insert"}}, {API: "x", Operations: []string{"update"}}},
	} {
		if _, err := s.SetWriteTargets(ctx, acmeOwner, a.ID, WriteTargetsInput{Targets: bad, Confirmed: true}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	many := make([]WriteTarget, MaxWriteTargets+1)
	for i := range many {
		many[i] = WriteTarget{API: "api-" + strings.Repeat("x", i+1), Operations: []string{"insert"}}
	}
	if _, err := s.SetWriteTargets(ctx, acmeOwner, a.ID, WriteTargetsInput{Targets: many, Confirmed: true}); !errors.Is(err, ErrInvalid) {
		t.Errorf("too many: %v", err)
	}
}

type fakeRevoker struct {
	calls []string
	err   error
	store Store
	// seen is the app's declaration as stored when the revoke ran.
	seen []WriteTarget
}

func (f *fakeRevoker) RevokeGrants(ctx context.Context, workspace, principal, actingOwner string) error {
	f.calls = append(f.calls, workspace+" "+principal+" "+actingOwner)
	if f.store != nil {
		id := strings.TrimPrefix(principal, "streamlit:"+workspace+":")
		a, _ := f.store.Get(ctx, workspace, id)
		f.seen = a.WriteTargets
	}
	return f.err
}

// pausedWithTargets is an app whose owner lost access and which declares a write target.
func pausedWithTargets(t *testing.T) (*Service, *MemoryStore, App) {
	t.Helper()
	ctx := context.Background()
	store := NewMemoryStore()
	s := NewService(store, 0)
	a := mustCreate(t, s, acmeAlice, "finance", true)
	if _, err := s.SetWriteTargets(ctx, acmeAlice, a.ID, WriteTargetsInput{Targets: []WriteTarget{{API: "household", Operations: []string{"insert"}}}, Confirmed: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetDataPaused(ctx, "acme", a.ID, "owner gone", time.Now(), true); err != nil {
		t.Fatal(err)
	}
	return s, store, a
}

func TestTakeOwnership_ClearsThenRevokesThenChangesOwner(t *testing.T) {
	ctx := context.Background()
	s, store, a := pausedWithTargets(t)
	rv := &fakeRevoker{store: store}
	s.Revoker = rv
	got, err := s.TakeOwnership(ctx, acmeOwner, a.ID)
	if err != nil || got.Owner != "o" || got.Writes() {
		t.Fatalf("TakeOwnership: %+v %v", got, err)
	}
	if want := []string{"acme streamlit:acme:" + a.ID + " o"}; !reflect.DeepEqual(rv.calls, want) {
		t.Errorf("revoke calls = %v, want %v (the literal principal, minted under the new owner)", rv.calls, want)
	}
	if rv.seen == nil || len(rv.seen) != 0 {
		t.Errorf("the declaration was %+v when the revoke ran; it must be cleared first", rv.seen)
	}
}

func TestTakeOwnership_FailsClosedWhenTheRevokeFails(t *testing.T) {
	ctx := context.Background()
	s, store, a := pausedWithTargets(t)
	s.Revoker = &fakeRevoker{err: errors.New("booth-api answered 503")}
	_, err := s.TakeOwnership(ctx, acmeOwner, a.ID)
	if !errors.Is(err, ErrRevokeFailed) || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v, want ErrRevokeFailed with the cause", err)
	}
	got, _ := store.Get(ctx, "acme", a.ID)
	if got.Owner != "alice" || got.DataPausedReason == "" {
		t.Errorf("the owner changed anyway: %+v", got)
	}
	if got.Writes() {
		t.Errorf("the declaration survived a failed take-over: %+v (clearing it only ever reduces access)", got.WriteTargets)
	}
	if ch, _ := store.OwnershipChanges(ctx, "acme", a.ID); len(ch) != 0 {
		t.Errorf("a take-over was recorded: %+v", ch)
	}
	// A retry once booth-api answers goes through.
	s.Revoker = &fakeRevoker{}
	if got, err := s.TakeOwnership(ctx, acmeOwner, a.ID); err != nil || got.Owner != "o" {
		t.Errorf("retry: %+v %v", got, err)
	}
}

// The revoke runs even with no declaration here: booth-api's grant is made on its own page and can
// exist without one.
func TestTakeOwnership_RevokesWithoutADeclaration(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	s := NewService(store, 0)
	a := mustCreate(t, s, acmeAlice, "finance", true)
	if _, err := store.SetDataPaused(ctx, "acme", a.ID, "owner gone", time.Now(), true); err != nil {
		t.Fatal(err)
	}
	rv := &fakeRevoker{}
	s.Revoker = rv
	if _, err := s.TakeOwnership(ctx, acmeOwner, a.ID); err != nil || len(rv.calls) != 1 {
		t.Fatalf("TakeOwnership: %v, revoke calls %v", err, rv.calls)
	}
	// Not paused: refused before anything is revoked.
	b := mustCreate(t, s, acmeOwner, "other", true)
	if _, err := s.TakeOwnership(ctx, acmeOwner, b.ID); !errors.Is(err, ErrNotPaused) || len(rv.calls) != 1 {
		t.Errorf("not paused: %v, revoke calls %v", err, rv.calls)
	}
}
