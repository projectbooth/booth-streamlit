package apps

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/projectbooth/booth-streamlit/internal/identity"
)

// Service applies the package's authorization rules (see the package comment) over a Store.
type Service struct {
	store          Store
	maxSourceBytes int
	now            func() time.Time

	// capMu serializes the running-app cap check with the write that follows it, so two
	// concurrent starts can't both pass. The backend runs one replica (the chart's default and
	// the design); a second replica would need this in the database instead.
	capMu sync.Mutex
	caps  Caps

	// OnChange, if set, is called with the app's id after any change the lifecycle must act on:
	// create, edit, start, stop, suspend, wake, delete. It runs after the change is stored and
	// before the call returns, so the lifecycle can drop what it had observed about that app before
	// anyone (the proxy) relies on it again.
	OnChange func(appID string)
}

// Caps bounds how many apps run at once (design note (a)): sized for a single node.
type Caps struct {
	// MaxRunning is the cap for the whole install; 0 means unlimited.
	MaxRunning int
	// MaxRunningPerWorkspace is an optional per-workspace cap; 0 means none.
	MaxRunningPerWorkspace int
}

// SetCaps sets the running-app caps.
func (s *Service) SetCaps(c Caps) {
	s.capMu.Lock()
	s.caps = c
	s.capMu.Unlock()
}

// NewService returns a Service. maxSourceBytes <= 0 means DefaultMaxSourceBytes; it is capped
// below a ConfigMap's size limit.
func NewService(store Store, maxSourceBytes int) *Service {
	if maxSourceBytes <= 0 {
		maxSourceBytes = DefaultMaxSourceBytes
	}
	if maxSourceBytes > maxSourceBytesCeiling {
		maxSourceBytes = maxSourceBytesCeiling
	}
	return &Service{store: store, maxSourceBytes: maxSourceBytes, now: time.Now}
}

// CanAuthor reports whether the caller may create, edit, start, stop or delete apps (ADR 0105).
func CanAuthor(c identity.Caller) bool { return c.Role == identity.RoleOwner }

// visible reports whether the caller may see a (same-workspace) app.
func visible(c identity.Caller, a App) bool { return CanAuthor(c) || a.Shared }

// List returns the apps the caller may see in their workspace.
func (s *Service) List(ctx context.Context, c identity.Caller) ([]App, error) {
	return s.store.List(ctx, c.Workspace, !CanAuthor(c))
}

// Get returns one app the caller may see, including its source.
func (s *Service) Get(ctx context.Context, c identity.Caller, id string) (App, error) {
	a, err := s.store.Get(ctx, c.Workspace, id)
	if err != nil {
		return App{}, err
	}
	if !visible(c, a) {
		return App{}, ErrNotFound
	}
	return a, nil
}

// Create adds an app to the caller's workspace. Owners only.
func (s *Service) Create(ctx context.Context, c identity.Caller, in Input) (App, error) {
	if !CanAuthor(c) {
		return App{}, ErrForbidden
	}
	in, err := s.validate(in)
	if err != nil {
		return App{}, err
	}
	now := s.now().UTC()
	a := App{
		ID: newID(), Workspace: c.Workspace, GateBearer: newBearer(), Owner: c.Subject,
		Name: in.Name, Description: in.Description, Source: in.Source, Requirements: in.Requirements, Sources: in.Sources, Shared: in.Shared,
		DesiredState: Stopped, CreatedBy: c.Subject, CreatedAt: now, UpdatedBy: c.Subject, UpdatedAt: now,
	}
	if err := s.store.Create(ctx, a); err != nil {
		return App{}, err
	}
	s.changed(a.ID)
	return a, nil
}

// Update replaces an app's name, description, source and sharing. Owners only.
func (s *Service) Update(ctx context.Context, c identity.Caller, id string, in Input) (App, error) {
	cur, err := s.authorizeWrite(ctx, c, id)
	if err != nil {
		return App{}, err
	}
	in, err = s.validate(in)
	if err != nil {
		return App{}, err
	}
	cur.Name, cur.Description, cur.Source, cur.Requirements, cur.Sources, cur.Shared = in.Name, in.Description, in.Source, in.Requirements, in.Sources, in.Shared
	cur.UpdatedBy, cur.UpdatedAt = c.Subject, s.now().UTC()
	if err := s.store.Update(ctx, cur); err != nil {
		return App{}, err
	}
	s.changed(cur.ID)
	return cur, nil
}

// SetDesiredState records that an app should run or stop (start/stop). Owners only. The lifecycle
// that acts on it is build step 3.
func (s *Service) SetDesiredState(ctx context.Context, c identity.Caller, id string, st DesiredState) (App, error) {
	if _, err := s.authorizeWrite(ctx, c, id); err != nil {
		return App{}, err
	}
	s.capMu.Lock()
	defer s.capMu.Unlock()
	if st == Running {
		if err := s.checkCapLocked(ctx, c.Workspace, id); err != nil {
			return App{}, err
		}
	}
	a, err := s.store.SetDesiredState(ctx, c.Workspace, id, st, c.Subject)
	if err == nil {
		s.changed(id)
	}
	return a, err
}

// Wake brings an idle-suspended app back for anyone who may open it (ADR 0105: waking is not
// starting; the owner already chose Running). It returns the app; ErrStopped if its owner stopped
// it, ErrCapacity if the cap is reached. Waking an app that is already awake is a no-op.
func (s *Service) Wake(ctx context.Context, c identity.Caller, id string) (App, error) {
	a, err := s.Get(ctx, c, id)
	if err != nil {
		return App{}, err
	}
	if a.DesiredState != Running {
		return a, ErrStopped
	}
	if !a.Suspended {
		return a, nil
	}
	s.capMu.Lock()
	defer s.capMu.Unlock()
	if err := s.checkCapLocked(ctx, a.Workspace, a.ID); err != nil {
		return a, err
	}
	if err := s.store.SetSuspended(ctx, a.Workspace, a.ID, false); err != nil {
		return a, err
	}
	a.Suspended = false
	s.changed(a.ID)
	return a, nil
}

// Suspend is idle shutdown: the lifecycle calls it, never a person, so it takes no caller.
func (s *Service) Suspend(ctx context.Context, workspace, id string) error {
	if err := s.store.SetSuspended(ctx, workspace, id, true); err != nil {
		return err
	}
	s.changed(id)
	return nil
}

// All returns every app with source and bearer, for the lifecycle only.
func (s *Service) All(ctx context.Context) ([]App, error) { return s.store.ListAll(ctx) }

// Active reports whether an app should have a container: its owner chose Running and it is not
// idle-suspended.
func Active(a App) bool { return a.DesiredState == Running && !a.Suspended }

// checkCapLocked refuses if making app id active would exceed a cap. capMu must be held.
func (s *Service) checkCapLocked(ctx context.Context, workspace, id string) error {
	if s.caps.MaxRunning == 0 && s.caps.MaxRunningPerWorkspace == 0 {
		return nil
	}
	all, err := s.store.ListAll(ctx)
	if err != nil {
		return err
	}
	total, inWS := 0, 0
	for _, a := range all {
		if a.ID == id || !Active(a) {
			continue
		}
		total++
		if a.Workspace == workspace {
			inWS++
		}
	}
	if (s.caps.MaxRunning > 0 && total >= s.caps.MaxRunning) || (s.caps.MaxRunningPerWorkspace > 0 && inWS >= s.caps.MaxRunningPerWorkspace) {
		return ErrCapacity
	}
	return nil
}

func (s *Service) changed(id string) {
	if s.OnChange != nil {
		s.OnChange(id)
	}
}

// Delete removes an app. Owners only.
func (s *Service) Delete(ctx context.Context, c identity.Caller, id string) error {
	if _, err := s.authorizeWrite(ctx, c, id); err != nil {
		return err
	}
	if err := s.store.Delete(ctx, c.Workspace, id); err != nil {
		return err
	}
	s.changed(id)
	return nil
}

// authorizeWrite: an app the caller can't see is ErrNotFound (no probing); one they can see but
// may not change is ErrForbidden.
func (s *Service) authorizeWrite(ctx context.Context, c identity.Caller, id string) (App, error) {
	a, err := s.Get(ctx, c, id)
	if err != nil {
		return App{}, err
	}
	if !CanAuthor(c) {
		return App{}, ErrForbidden
	}
	return a, nil
}

// Ping checks the store, for /healthz.
func (s *Service) Ping(ctx context.Context) error { return s.store.Ping(ctx) }

func (s *Service) validate(in Input) (Input, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Description = strings.TrimSpace(in.Description)
	switch {
	case in.Name == "":
		return Input{}, fmt.Errorf("%w: name is required", ErrInvalid)
	case utf8.RuneCountInString(in.Name) > MaxNameLen:
		return Input{}, fmt.Errorf("%w: name is longer than %d characters", ErrInvalid, MaxNameLen)
	case utf8.RuneCountInString(in.Description) > MaxDescriptionLen:
		return Input{}, fmt.Errorf("%w: description is longer than %d characters", ErrInvalid, MaxDescriptionLen)
	case strings.TrimSpace(in.Source) == "":
		return Input{}, fmt.Errorf("%w: source is required", ErrInvalid)
	case len(in.Source) > s.maxSourceBytes:
		return Input{}, fmt.Errorf("%w: source is larger than %d bytes", ErrInvalid, s.maxSourceBytes)
	case !utf8.ValidString(in.Source) || strings.ContainsRune(in.Source, 0):
		return Input{}, fmt.Errorf("%w: source must be UTF-8 text", ErrInvalid)
	}
	if err := validateRequirements(in.Requirements); err != nil {
		return Input{}, err
	}
	src, err := validateSources(in.Sources)
	if err != nil {
		return Input{}, err
	}
	in.Sources = src
	return in, nil
}

// validateRequirements bounds an app's requirements.txt. Package specifiers only: a line starting
// with "-" is a pip option (an index URL, another requirements file, an editable install), and the
// package index is the operator's choice (the chart's apps.pip.indexUrl), not the app's. pip
// itself reports anything else it can't parse, and the owner sees that as the install failure.
func validateRequirements(r string) error {
	switch {
	case len(r) > MaxRequirementsBytes:
		return fmt.Errorf("%w: requirements.txt is larger than %d bytes", ErrInvalid, MaxRequirementsBytes)
	case !utf8.ValidString(r) || strings.ContainsRune(r, 0):
		return fmt.Errorf("%w: requirements.txt must be UTF-8 text", ErrInvalid)
	}
	for i, line := range strings.Split(r, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "-") {
			return fmt.Errorf("%w: requirements.txt line %d is a pip option; only package specifiers are allowed (the package index is set by the operator)", ErrInvalid, i+1)
		}
	}
	return nil
}

// newBearer returns 256 random bits, hex-encoded.
func newBearer() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}

// newID returns "a" plus 12 random base-32 characters: valid as a Kubernetes name part and as a
// path segment, unguessable enough that ids are not a way to enumerate apps.
func newID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	out := []byte{'a'}
	for _, c := range b {
		out = append(out, alphabet[int(c)%len(alphabet)])
	}
	return string(out)
}
