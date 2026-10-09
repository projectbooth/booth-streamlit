// Package apps is booth-streamlit's app model: what an app is, who may see and change it, and
// where it is stored.
//
// Authorization lives here, in Service, not in the HTTP layer or the UI, so every path (API,
// proxy) applies the same rules from the verified caller (ADR 0041):
//
//   - An app belongs to exactly one workspace and is invisible outside it (ADR 0104 item 5).
//   - Only workspace owners may create, edit, start, stop or delete an app (ADR 0105, an interim
//     measure while user-authored code runs same-origin with the shell, ARCHITECTURE.md item 55).
//     Any owner of the workspace may manage any app in it.
//   - An app is visible to the workspace's owners always, and to its editors and viewers only
//     when it is shared with the workspace.
package apps

import (
	"errors"
	"time"
)

// DesiredState is what the lifecycle (build step 3) should make true for an app.
type DesiredState string

const (
	Stopped DesiredState = "stopped"
	Running DesiredState = "running"
)

// App is one Streamlit app.
type App struct {
	ID          string `json:"id"`
	Workspace   string `json:"workspace"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source,omitempty"` // omitted in lists
	// Requirements is the app's optional requirements.txt, installed into its pod on every start.
	Requirements string `json:"requirements,omitempty"` // omitted in lists
	// Sources are catalog dataset ids the owner declared the app reads: lineage only, never a
	// restriction on what it can read (ADR 0107 item 4).
	Sources      []string     `json:"sources"`
	Shared       bool         `json:"shared"`
	DesiredState DesiredState `json:"desiredState"`
	// Suspended: idle shutdown stopped the container; DesiredState is still Running and any
	// member who may open the app wakes it.
	Suspended bool `json:"suspended"`
	// GateBearer is the per-app secret for the app pod's gate (design note (b)). Never serialized.
	GateBearer string `json:"-"`
	// Owner is whose read access the app uses for data (ADR 0104/0107): the creator, until a
	// workspace owner takes it over.
	Owner string `json:"owner"`
	// DataPausedReason is set while core refuses to mint for Owner (data access paused); the app
	// still runs.
	DataPausedReason string     `json:"dataPausedReason,omitempty"`
	DataPausedAt     *time.Time `json:"dataPausedAt,omitempty"`
	// DataEpoch is on the pod template; bumping it rolls the pod.
	DataEpoch int       `json:"-"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedBy string    `json:"updatedBy"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Input is what an owner sets when creating or editing an app.
type Input struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
	// Requirements: an optional requirements.txt (package specifiers only; no pip options).
	Requirements string `json:"requirements"`
	// Sources: catalog dataset ids, lineage only (at most 50).
	Sources []string `json:"sources"`
	Shared  bool     `json:"shared"`
}

// Limits on Input. Source is bounded well under a Kubernetes ConfigMap's ~1 MiB, which is how the
// lifecycle delivers it (design note (a)).
const (
	MaxNameLen            = 100
	MaxDescriptionLen     = 2000
	DefaultMaxSourceBytes = 256 * 1024
	MaxRequirementsBytes  = 16 * 1024
	maxSourceBytesCeiling = 900 * 1024
)

// Errors. The HTTP layer maps them to 404, 403 and 400.
var (
	// ErrNotFound: no such app in the caller's workspace, or one the caller may not see. The two
	// are deliberately indistinguishable, so ids can't be probed.
	ErrNotFound = errors.New("app not found")
	// ErrForbidden: the caller may see the app (or the list) but not change it.
	ErrForbidden = errors.New("only workspace owners may create, edit, start, stop or delete apps")
	// ErrInvalid wraps an input validation failure.
	ErrInvalid = errors.New("invalid app")
	// ErrCapacity: starting or waking the app would exceed the running-app cap.
	ErrCapacity = errors.New("too many apps are running; stop one first")
	// ErrStopped: the app's owner has stopped it; only an owner's Start brings it back.
	ErrStopped = errors.New("this app is stopped")
	// ErrNotPaused: take ownership is only for an app whose owner lost access (ADR 0107 item 7).
	ErrNotPaused = errors.New("this app's owner still has access; take ownership is only for an app whose data access is paused")
)
