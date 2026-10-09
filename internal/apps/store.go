package apps

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
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
	// GetByBearer finds the app whose gate presents bearer, in any workspace: the bearer is the
	// app's identity on the backend's internal port. ErrNotFound for anything else.
	GetByBearer(ctx context.Context, bearer string) (App, error)
	// SetDataPaused records (reason != "") or clears (reason == "") a data-access pause; bumpEpoch
	// also bumps DataEpoch so the pod rolls. It returns the app as stored.
	SetDataPaused(ctx context.Context, workspace, id, reason string, at time.Time, bumpEpoch bool) (App, error)
	// TakeOwnership sets Owner to newOwner, clears any pause and records the change, atomically.
	TakeOwnership(ctx context.Context, workspace, id, newOwner, reason string, at time.Time) (previous string, err error)
	// OwnershipChanges lists an app's recorded take-overs, oldest first.
	OwnershipChanges(ctx context.Context, workspace, id string) ([]OwnershipChange, error)
	Delete(ctx context.Context, workspace, id string) error
	Ping(ctx context.Context) error

	// Create, Update, Delete and TakeOwnership also write the change's dashboard events
	// (dashboardEvents) to the outbox, in the same transaction. The drainer reads them back:
	PublishNext(ctx context.Context, maxAttempts int, publish func(OutboxRow) error) (found bool, err error)
	OutboxStats(ctx context.Context) (OutboxStats, error)
	PruneOutbox(ctx context.Context, olderThan time.Duration) error
}

// OwnershipChange is one recorded take-over (ADR 0107 item 7).
type OwnershipChange struct {
	AppID         string    `json:"appId"`
	Workspace     string    `json:"workspace"`
	PreviousOwner string    `json:"previousOwner"`
	NewOwner      string    `json:"newOwner"`
	Reason        string    `json:"reason"`
	At            time.Time `json:"at"`
}

// MemoryStore is a Store held in process memory, for tests.
type MemoryStore struct {
	mu      sync.Mutex
	apps    map[string]App // by id
	changes []OwnershipChange
	outbox  []memRow
	nextID  int64
	// Now is the outbox's clock (the database clock in Postgres); tests may set it.
	Now func() time.Time
}

type memRow struct {
	OutboxRow
	published   bool
	failed      bool
	nextAttempt time.Time
	lastError   string
}

func (m *MemoryStore) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// writeEvents must be called with m.mu held.
func (m *MemoryStore) writeEvents(evs []DashboardEvent) {
	for _, e := range evs {
		m.nextID++
		t := m.now()
		for _, r := range m.outbox { // strictly after this app's previous event, as in Postgres
			if r.Workspace == e.Workspace && r.AppID == e.AppID && !t.After(r.CreatedAt) {
				t = r.CreatedAt.Add(time.Microsecond)
			}
		}
		m.outbox = append(m.outbox, memRow{OutboxRow: OutboxRow{ID: m.nextID, Workspace: e.Workspace, AppID: e.AppID, Type: e.Type, Data: e.Data, CreatedAt: t}, nextAttempt: m.now()})
	}
}

// Outbox returns every row written so far, for tests.
func (m *MemoryStore) Outbox() []OutboxRow {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]OutboxRow, 0, len(m.outbox))
	for _, r := range m.outbox {
		out = append(out, r.OutboxRow)
	}
	return out
}

func (m *MemoryStore) PublishNext(_ context.Context, maxAttempts int, publish func(OutboxRow) error) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for i := range m.outbox {
		r := &m.outbox[i]
		if r.published || r.failed || r.nextAttempt.After(now) {
			continue
		}
		r.Attempts++
		if err := publish(r.OutboxRow); err != nil {
			r.lastError = err.Error()
			r.failed = r.Attempts >= maxAttempts
			backoff := time.Duration(1<<min(r.Attempts, 8)) * time.Second
			r.nextAttempt = now.Add(min(backoff, 5*time.Minute))
			return true, nil
		}
		r.published, r.lastError = true, ""
		return true, nil
	}
	return false, nil
}

func (m *MemoryStore) OutboxStats(context.Context) (OutboxStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var st OutboxStats
	for _, r := range m.outbox {
		switch {
		case r.failed:
			st.Failed++
		case !r.published:
			st.Pending++
		}
		if !r.published && r.lastError != "" {
			st.LastError = r.lastError
		}
	}
	return st, nil
}

func (m *MemoryStore) PruneOutbox(context.Context, time.Duration) error { return nil }

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore { return &MemoryStore{apps: map[string]App{}} }

func (m *MemoryStore) List(_ context.Context, workspace string, sharedOnly bool) ([]App, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []App
	for _, a := range m.apps {
		if a.Workspace == workspace && (!sharedOnly || a.Shared) {
			a.Source, a.Requirements, a.GateBearer = "", "", ""
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
	if a.Sources == nil {
		a.Sources = []string{}
	}
	m.apps[a.ID] = a
	m.writeEvents(dashboardEvents(nil, &a))
	return nil
}

func (m *MemoryStore) Update(_ context.Context, a App) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.apps[a.ID]
	if !ok || cur.Workspace != a.Workspace {
		return ErrNotFound
	}
	before := cur
	if a.Sources == nil {
		a.Sources = []string{}
	}
	cur.Name, cur.Description, cur.Source, cur.Requirements, cur.Sources, cur.Shared = a.Name, a.Description, a.Source, a.Requirements, append([]string{}, a.Sources...), a.Shared
	cur.UpdatedBy, cur.UpdatedAt = a.UpdatedBy, a.UpdatedAt
	m.apps[a.ID] = cur
	m.writeEvents(dashboardEvents(&before, &cur))
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
	m.writeEvents(dashboardEvents(&cur, nil))
	return nil
}

func (m *MemoryStore) GetByBearer(_ context.Context, bearer string) (App, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if bearer == "" {
		return App{}, ErrNotFound
	}
	for _, a := range m.apps {
		if a.GateBearer == bearer {
			return a, nil
		}
	}
	return App{}, ErrNotFound
}

func (m *MemoryStore) SetDataPaused(_ context.Context, workspace, id, reason string, at time.Time, bumpEpoch bool) (App, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.apps[id]
	if !ok || cur.Workspace != workspace {
		return App{}, ErrNotFound
	}
	cur.DataPausedReason = reason
	if reason == "" {
		cur.DataPausedAt = nil
	} else {
		t := at
		cur.DataPausedAt = &t
	}
	if bumpEpoch {
		cur.DataEpoch++
	}
	m.apps[id] = cur
	return cur, nil
}

func (m *MemoryStore) TakeOwnership(_ context.Context, workspace, id, newOwner, reason string, at time.Time) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.apps[id]
	if !ok || cur.Workspace != workspace {
		return "", ErrNotFound
	}
	prev, before := cur.Owner, cur
	cur.Owner, cur.DataPausedReason, cur.DataPausedAt = newOwner, "", nil
	m.apps[id] = cur
	m.writeEvents(dashboardEvents(&before, &cur))
	m.changes = append(m.changes, OwnershipChange{AppID: id, Workspace: workspace, PreviousOwner: prev, NewOwner: newOwner, Reason: reason, At: at})
	return prev, nil
}

func (m *MemoryStore) OwnershipChanges(_ context.Context, workspace, id string) ([]OwnershipChange, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []OwnershipChange
	for _, c := range m.changes {
		if c.AppID == id && c.Workspace == workspace {
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *MemoryStore) Ping(context.Context) error { return nil }
