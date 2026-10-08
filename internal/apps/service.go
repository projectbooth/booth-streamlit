package apps

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/projectbooth/booth-streamlit/internal/identity"
)

// Service applies the package's authorization rules (see the package comment) over a Store.
type Service struct {
	store          Store
	maxSourceBytes int
	now            func() time.Time
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
		ID: newID(), Workspace: c.Workspace,
		Name: in.Name, Description: in.Description, Source: in.Source, Shared: in.Shared,
		DesiredState: Stopped, CreatedBy: c.Subject, CreatedAt: now, UpdatedBy: c.Subject, UpdatedAt: now,
	}
	if err := s.store.Create(ctx, a); err != nil {
		return App{}, err
	}
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
	cur.Name, cur.Description, cur.Source, cur.Shared = in.Name, in.Description, in.Source, in.Shared
	cur.UpdatedBy, cur.UpdatedAt = c.Subject, s.now().UTC()
	if err := s.store.Update(ctx, cur); err != nil {
		return App{}, err
	}
	return cur, nil
}

// SetDesiredState records that an app should run or stop (start/stop). Owners only. The lifecycle
// that acts on it is build step 3.
func (s *Service) SetDesiredState(ctx context.Context, c identity.Caller, id string, st DesiredState) (App, error) {
	if _, err := s.authorizeWrite(ctx, c, id); err != nil {
		return App{}, err
	}
	return s.store.SetDesiredState(ctx, c.Workspace, id, st, c.Subject)
}

// Delete removes an app. Owners only.
func (s *Service) Delete(ctx context.Context, c identity.Caller, id string) error {
	if _, err := s.authorizeWrite(ctx, c, id); err != nil {
		return err
	}
	return s.store.Delete(ctx, c.Workspace, id)
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
	return in, nil
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
