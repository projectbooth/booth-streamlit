package apps

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSetWriteTargets_Uploads(t *testing.T) {
	ctx := context.Background()
	s := NewService(NewMemoryStore(), 0)
	a := mustCreate(t, s, acmeOwner, "finance", true)

	one := []UploadTarget{{Backend: "local", Prefix: "/finance/inbox"}}
	// Adding a folder needs the plain-words confirmation.
	if _, err := s.SetWriteTargets(ctx, acmeOwner, a.ID, WriteTargetsInput{Uploads: one}); !errors.Is(err, ErrConfirmationRequired) {
		t.Fatalf("unconfirmed add: %v", err)
	}
	got, err := s.SetWriteTargets(ctx, acmeOwner, a.ID, WriteTargetsInput{Uploads: one, Confirmed: true})
	want := []UploadTarget{{Backend: "local", Prefix: "finance/inbox/"}}
	if err != nil || !reflect.DeepEqual(got.UploadTargets, want) {
		t.Fatalf("confirmed add: %+v %v (the folder as booth-storage cleans it)", got.UploadTargets, err)
	}
	if !got.Uploads() || got.Writes() || !got.ChangesData() || !got.AllowsUpload("local", "finance/inbox/") || got.AllowsUpload("local", "finance/") {
		t.Errorf("predicates on %+v", got.UploadTargets)
	}
	// Another folder is widening; the same one resaved isn't; removing never needs it.
	two := append(want, UploadTarget{Backend: "local", Prefix: "finance/receipts/"})
	if _, err := s.SetWriteTargets(ctx, acmeOwner, a.ID, WriteTargetsInput{Uploads: two}); !errors.Is(err, ErrConfirmationRequired) {
		t.Errorf("unconfirmed widening: %v", err)
	}
	if _, err := s.SetWriteTargets(ctx, acmeOwner, a.ID, WriteTargetsInput{Uploads: want}); err != nil {
		t.Errorf("unchanged: %v", err)
	}
	// Writes and uploads are one declaration: saving writes alone clears the uploads.
	if got, err := s.SetWriteTargets(ctx, acmeOwner, a.ID, WriteTargetsInput{}); err != nil || got.ChangesData() {
		t.Errorf("removing: %+v %v", got, err)
	}

	for name, bad := range map[string][]UploadTarget{
		"bad backend":   {{Backend: "a/b", Prefix: "x/"}},
		"no backend":    {{Prefix: "x/"}},
		"dot dot":       {{Backend: "local", Prefix: "a/../b"}},
		"empty segment": {{Backend: "local", Prefix: "a//b"}},
		"backslash":     {{Backend: "local", Prefix: `a\b`}},
		"control":       {{Backend: "local", Prefix: "a\x01b"}},
		"twice":         {{Backend: "local", Prefix: "a"}, {Backend: "local", Prefix: "a/"}},
		"too long":      {{Backend: "local", Prefix: strings.Repeat("a", 513)}},
	} {
		if _, err := s.SetWriteTargets(ctx, acmeOwner, a.ID, WriteTargetsInput{Uploads: bad, Confirmed: true}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	many := make([]UploadTarget, MaxUploadTargets+1)
	for i := range many {
		many[i] = UploadTarget{Backend: "local", Prefix: strings.Repeat("x", i+1)}
	}
	if _, err := s.SetWriteTargets(ctx, acmeOwner, a.ID, WriteTargetsInput{Uploads: many, Confirmed: true}); !errors.Is(err, ErrInvalid) {
		t.Errorf("too many: %v", err)
	}
	// The backend's root is a folder too.
	if got, err := s.SetWriteTargets(ctx, acmeOwner, a.ID, WriteTargetsInput{Uploads: []UploadTarget{{Backend: "local", Prefix: "/"}}, Confirmed: true}); err != nil || !got.AllowsUpload("local", "") {
		t.Errorf("root: %+v %v", got.UploadTargets, err)
	}
}

func TestTakeOwnership_ClearsUploadsAndRevokesInBothModules(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	s := NewService(store, 0)
	a := mustCreate(t, s, acmeAlice, "finance", true)
	if _, err := s.SetWriteTargets(ctx, acmeAlice, a.ID, WriteTargetsInput{Uploads: []UploadTarget{{Backend: "local", Prefix: "in"}}, Confirmed: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetDataPaused(ctx, "acme", a.ID, "owner gone", time.Now(), true); err != nil {
		t.Fatal(err)
	}
	api, storage := &fakeRevoker{store: store}, &fakeRevoker{store: store, err: errors.New("booth-storage answered 503")}
	s.Revoker = Revokers{api, storage}
	if _, err := s.TakeOwnership(ctx, acmeOwner, a.ID); !errors.Is(err, ErrRevokeFailed) || !strings.Contains(err.Error(), "booth-storage") {
		t.Fatalf("storage revoke failing: %v, want ErrRevokeFailed naming booth-storage", err)
	}
	got, _ := store.Get(ctx, "acme", a.ID)
	if got.Owner != "alice" || got.Uploads() {
		t.Errorf("after a failed revoke: owner %q uploads %+v (owner unchanged, declaration cleared)", got.Owner, got.UploadTargets)
	}
	storage.err = nil
	if got, err := s.TakeOwnership(ctx, acmeOwner, a.ID); err != nil || got.Owner != "o" {
		t.Fatalf("retry: %+v %v", got, err)
	}
	if len(api.calls) != 2 || len(storage.calls) != 2 || storage.calls[1] != "acme streamlit:acme:"+a.ID+" o" {
		t.Errorf("revoke calls: api %v storage %v", api.calls, storage.calls)
	}
}

func TestRevokers_StopAtTheFirstFailure(t *testing.T) {
	first, second := &fakeRevoker{err: errors.New("booth-api answered 503")}, &fakeRevoker{}
	if err := (Revokers{first, second}).RevokeGrants(context.Background(), "acme", "streamlit:acme:a1", "o"); err == nil || len(second.calls) != 0 {
		t.Errorf("err %v, later revoker called %v", err, second.calls)
	}
}
