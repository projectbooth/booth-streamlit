package apps

import (
	"context"
	"sort"
	"strings"
	"sync"
)

// Store persists apps. Every method is workspace-scoped: an app in another workspace is
// ErrNotFound. Authorization is not the store's job; Service does it.
type Store interface {
	// List returns the workspace's apps ordered by name, without Source. sharedOnly limits it to
	// apps shared with the workspace.
	List(ctx context.Context, workspace string, sharedOnly bool) ([]App, error)
	Get(ctx context.Context, workspace, id string) (App, error)
	Create(ctx context.Context, a App) error
	// Update replaces name, description, source, shared, updated_by and updated_at.
	Update(ctx context.Context, a App) error
	// SetDesiredState also clears Suspended: an owner's explicit Start or Stop overrides idleness.
	SetDesiredState(ctx context.Context, workspace, id string, s DesiredState, by string) (App, error)
	// SetSuspended marks an app idle-suspended (true) or awake (false); it never changes
	// DesiredState. Unknown ids are ErrNotFound.
	SetSuspended(ctx context.Context, workspace, id string, suspended bool) error
	// ListAll returns every app in every workspace, with Source and GateBearer, for the lifecycle
	// reconciler. Nothing user-facing may call it.
	ListAll(ctx context.Context) ([]App, error)
	Delete(ctx context.Context, workspace, id string) error
	Ping(ctx context.Context) error
}

// MemoryStore is a Store held in process memory, for tests.
type MemoryStore struct {
	mu   sync.Mutex
	apps map[string]App // by id
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore { return &MemoryStore{apps: map[string]App{}} }

func (m *MemoryStore) List(_ context.Context, workspace string, sharedOnly bool) ([]App, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []App
	for _, a := range m.apps {
		if a.Workspace == workspace && (!sharedOnly || a.Shared) {
			a.Source, a.GateBearer = "", ""
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		li, lj := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if li != lj {
			return li < lj
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *MemoryStore) Get(_ context.Context, workspace, id string) (App, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.apps[id]
	if !ok || a.Workspace != workspace {
		return App{}, ErrNotFound
	}
	return a, nil
}

func (m *MemoryStore) Create(_ context.Context, a App) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.apps[a.ID] = a
	return nil
}

func (m *MemoryStore) Update(_ context.Context, a App) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.apps[a.ID]
	if !ok || cur.Workspace != a.Workspace {
		return ErrNotFound
	}
	cur.Name, cur.Description, cur.Source, cur.Shared = a.Name, a.Description, a.Source, a.Shared
	cur.UpdatedBy, cur.UpdatedAt = a.UpdatedBy, a.UpdatedAt
	m.apps[a.ID] = cur
	return nil
}

func (m *MemoryStore) SetDesiredState(_ context.Context, workspace, id string, s DesiredState, by string) (App, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.apps[id]
	if !ok || cur.Workspace != workspace {
		return App{}, ErrNotFound
	}
	cur.DesiredState, cur.UpdatedBy, cur.Suspended = s, by, false
	m.apps[id] = cur
	return cur, nil
}

func (m *MemoryStore) SetSuspended(_ context.Context, workspace, id string, suspended bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.apps[id]
	if !ok || cur.Workspace != workspace {
		return ErrNotFound
	}
	cur.Suspended = suspended
	m.apps[id] = cur
	return nil
}

func (m *MemoryStore) ListAll(context.Context) ([]App, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]App, 0, len(m.apps))
	for _, a := range m.apps {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemoryStore) Delete(_ context.Context, workspace, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.apps[id]
	if !ok || cur.Workspace != workspace {
		return ErrNotFound
	}
	delete(m.apps, id)
	return nil
}

func (m *MemoryStore) Ping(context.Context) error { return nil }
