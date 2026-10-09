package apps

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

// Dashboard events (ADR 0018/0046; docs/design-data-access.md item 6). booth-catalog lists
// dashboards to every workspace member, so only shared apps appear there: an app becoming shared
// publishes created, a change to a shared app publishes updated (re-owning included), and an app
// becoming unshared or deleted publishes deleted.
//
// Each event is written to an outbox in the same transaction as the change it describes, and
// drained to the bus afterwards (internal/events), so a change is never committed without its
// event or published without its change. publishedAt is the outbox row's database timestamp, which
// only moves forward for an app (its changes are serialized by the row lock), as ADR 0046's
// last-writer-wins requires.
const (
	EventCreated = "dashboard.created"
	EventUpdated = "dashboard.updated"
	EventDeleted = "dashboard.deleted"

	// DashboardPath opens the module's app list: deep links into an iframe module's own pages
	// aren't defined, so every app's dashboard points at the module.
	DashboardPath = "/streamlit"
	// PublishedBy is this module's id, the envelope's publishedBy (identity is
	// (workspace, publishedBy, dashboardId)).
	PublishedBy = "streamlit"

	MaxSources = 50
)

var datasetIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,200}$`)

// DashboardEvent is one event as written to the outbox.
type DashboardEvent struct {
	Workspace string
	AppID     string
	Type      string
	Data      json.RawMessage
}

// OutboxRow is a stored event waiting to be (or already) published.
type OutboxRow struct {
	ID        int64
	Workspace string
	AppID     string
	Type      string
	Data      json.RawMessage
	CreatedAt time.Time // the database clock at the change: the envelope's publishedAt
	Attempts  int
}

// OutboxStats is the outbox as /healthz reports it.
type OutboxStats struct {
	Pending   int    `json:"pending"`
	Failed    int    `json:"failed"`
	LastError string `json:"lastError,omitempty"`
}

type dashboardSource struct {
	Type      string `json:"type"`
	DatasetID string `json:"datasetId"`
}

// upsertData is created/updated's full current state; deletedData only names the dashboard.
type upsertData struct {
	DashboardID     string            `json:"dashboardId"`
	Name            string            `json:"name"`
	Description     string            `json:"description"`
	Owner           string            `json:"owner"`
	Path            string            `json:"path"`
	LineageComplete bool              `json:"lineageComplete"`
	Sources         []dashboardSource `json:"sources"` // always present: a full-state upsert clears old lineage
}

type deletedData struct {
	DashboardID string `json:"dashboardId"`
}

// dashboardEvents decides what a change publishes. before is nil for a create, after nil for a
// delete. Only shared apps are dashboards.
func dashboardEvents(before, after *App) []DashboardEvent {
	wasShared := before != nil && before.Shared
	isShared := after != nil && after.Shared
	switch {
	case isShared && !wasShared:
		return []DashboardEvent{upsert(EventCreated, *after)}
	case isShared && wasShared:
		if !dashboardChanged(*before, *after) {
			return nil
		}
		return []DashboardEvent{upsert(EventUpdated, *after)}
	case wasShared && !isShared:
		a := before
		data, _ := json.Marshal(deletedData{DashboardID: a.ID})
		return []DashboardEvent{{Workspace: a.Workspace, AppID: a.ID, Type: EventDeleted, Data: data}}
	}
	return nil
}

// dashboardChanged: only what the event carries counts (a source-code edit alone is not a
// catalog change).
func dashboardChanged(a, b App) bool {
	if a.Name != b.Name || a.Description != b.Description || a.Owner != b.Owner || len(a.Sources) != len(b.Sources) {
		return true
	}
	for i := range a.Sources {
		if a.Sources[i] != b.Sources[i] {
			return true
		}
	}
	return false
}

func upsert(typ string, a App) DashboardEvent {
	sources := make([]dashboardSource, 0, len(a.Sources))
	for _, id := range a.Sources {
		sources = append(sources, dashboardSource{Type: "dataset", DatasetID: id})
	}
	// lineageComplete is false: declared sources are what the owner says the app reads, not a
	// guarantee (it can read anything its owner can, ADR 0107 item 4).
	data, _ := json.Marshal(upsertData{
		DashboardID: a.ID, Name: a.Name, Description: a.Description, Owner: a.Owner,
		Path: DashboardPath, LineageComplete: false, Sources: sources,
	})
	return DashboardEvent{Workspace: a.Workspace, AppID: a.ID, Type: typ, Data: data}
}

// Envelope is ADR 0026's event envelope around a row's data.
func Envelope(r OutboxRow) ([]byte, error) {
	return json.Marshal(map[string]any{
		"workspace":   r.Workspace,
		"eventType":   r.Type,
		"publishedAt": r.CreatedAt.UTC().Format(time.RFC3339Nano),
		"publishedBy": PublishedBy,
		"data":        r.Data,
	})
}

// Subject is ADR 0026's subject for a row: booth.<workspace>.dashboard.<verb>.
func Subject(r OutboxRow) string { return "booth." + r.Workspace + "." + r.Type }

func validateSources(src []string) ([]string, error) {
	if len(src) > MaxSources {
		return nil, fmt.Errorf("%w: at most %d sources", ErrInvalid, MaxSources)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(src))
	for _, id := range src {
		if !datasetIDPattern.MatchString(id) {
			return nil, fmt.Errorf("%w: %q is not a catalog dataset id", ErrInvalid, id)
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}
