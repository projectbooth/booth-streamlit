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
	ID           string       `json:"id"`
	Workspace    string       `json:"workspace"`
	Name         string       `json:"name"`
	Description  string       `json:"description"`
	Source       string       `json:"source,omitempty"` // omitted in lists
	Shared       bool         `json:"shared"`
	DesiredState DesiredState `json:"desiredState"`
	CreatedBy    string       `json:"createdBy"`
	CreatedAt    time.Time    `json:"createdAt"`
	UpdatedBy    string       `json:"updatedBy"`
	UpdatedAt    time.Time    `json:"updatedAt"`
}

// Input is what an owner sets when creating or editing an app.
type Input struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
	Shared      bool   `json:"shared"`
}

// Limits on Input. Source is bounded well under a Kubernetes ConfigMap's ~1 MiB, which is how the
// lifecycle delivers it (design note (a)).
const (
	MaxNameLen            = 100
	MaxDescriptionLen     = 2000
	DefaultMaxSourceBytes = 256 * 1024
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
)
