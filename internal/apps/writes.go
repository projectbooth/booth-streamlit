package apps

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"slices"

	"github.com/projectbooth/booth-streamlit/internal/identity"
)

// App writes (ADR 0117; its "Reconciled design" items 1a, 5 and 7). An owner declares here which
// booth-api APIs an app may write to and with which operations; the owner also grants the app on
// booth-api's own page, with their own token, and a write needs both. This module never holds a
// person's token, so it cannot make or read that grant: its copy is for the header every viewer
// sees, the write proxy's early refusal, and the per-app egress closure.

// Operations an app may declare. Insert is the default; update and delete are opt-in, and booth-api
// refuses any operation its own settings and the grant don't also allow.
const (
	OpInsert = "insert"
	OpUpdate = "update"
	OpDelete = "delete"
)

// MaxWriteTargets bounds one app's declaration.
const MaxWriteTargets = 20

// WriteTarget is one declared API: its booth-api slug and the operations the app may use on it.
type WriteTarget struct {
	API        string   `json:"api"`
	Operations []string `json:"operations"`
}

// WriteTargetsInput is what an owner saves: the whole declaration, writes and uploads. Confirmed is
// the plain-words confirmation; it is needed whenever the save adds a target, an operation or a
// folder, not to remove one.
type WriteTargetsInput struct {
	Targets   []WriteTarget  `json:"targets"`
	Uploads   []UploadTarget `json:"uploads"`
	Confirmed bool           `json:"confirmed"`
}

// apiSlugPattern is booth-api's slug rule (internal/appwrite slugRE).
var apiSlugPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

var opOrder = []string{OpInsert, OpUpdate, OpDelete}

// Allows reports whether the app's declaration lets it use op on the API with this slug.
func (a App) Allows(api, op string) bool {
	for _, t := range a.WriteTargets {
		if t.API == api {
			return slices.Contains(t.Operations, op)
		}
	}
	return false
}

// Writes reports whether the app declares any write target (and so runs with egress closed).
func (a App) Writes() bool { return len(a.WriteTargets) > 0 }

// Principal is the literal subject of the app's workload token, which booth-api's grants name
// (ADR 0117 "Principal string").
func Principal(workspace, appID string) string { return "streamlit:" + workspace + ":" + appID }

func validateWriteTargets(in []WriteTarget) ([]WriteTarget, error) {
	if len(in) > MaxWriteTargets {
		return nil, fmt.Errorf("%w: at most %d write targets", ErrInvalid, MaxWriteTargets)
	}
	out := make([]WriteTarget, 0, len(in))
	seen := map[string]bool{}
	for _, t := range in {
		if !apiSlugPattern.MatchString(t.API) {
			return nil, fmt.Errorf("%w: %q is not a booth-api API slug (lowercase letters, digits and hyphens, as shown on the API page)", ErrInvalid, t.API)
		}
		if seen[t.API] {
			return nil, fmt.Errorf("%w: API %q is listed twice", ErrInvalid, t.API)
		}
		seen[t.API] = true
		var ops []string
		for _, op := range opOrder { // canonical order, no duplicates
			if slices.Contains(t.Operations, op) {
				ops = append(ops, op)
			}
		}
		for _, op := range t.Operations {
			if !slices.Contains(opOrder, op) {
				return nil, fmt.Errorf("%w: %q is not an operation (insert, update or delete)", ErrInvalid, op)
			}
		}
		if len(ops) == 0 {
			return nil, fmt.Errorf("%w: API %q needs at least one operation", ErrInvalid, t.API)
		}
		out = append(out, WriteTarget{API: t.API, Operations: ops})
	}
	slices.SortFunc(out, func(a, b WriteTarget) int {
		switch {
		case a.API < b.API:
			return -1
		case a.API > b.API:
			return 1
		}
		return 0
	})
	return out, nil
}

// widens reports whether next adds a target or an operation that cur doesn't have.
func widens(cur, next []WriteTarget) bool {
	have := map[string][]string{}
	for _, t := range cur {
		have[t.API] = t.Operations
	}
	for _, t := range next {
		ops, ok := have[t.API]
		if !ok {
			return true
		}
		for _, op := range t.Operations {
			if !slices.Contains(ops, op) {
				return true
			}
		}
	}
	return false
}

// SetWriteTargets replaces an app's write and upload declaration. Owners only. Adding a target, an
// operation or a folder needs in.Confirmed; narrowing or removing doesn't. Turning the first target
// of either kind on or the last one off rolls the pod (its egress changes).
func (s *Service) SetWriteTargets(ctx context.Context, c identity.Caller, id string, in WriteTargetsInput) (App, error) {
	cur, err := s.authorizeWrite(ctx, c, id)
	if err != nil {
		return App{}, err
	}
	targets, err := validateWriteTargets(in.Targets)
	if err != nil {
		return App{}, err
	}
	uploads, err := validateUploadTargets(in.Uploads)
	if err != nil {
		return App{}, err
	}
	if (widens(cur.WriteTargets, targets) || widensUploads(cur.UploadTargets, uploads)) && !in.Confirmed {
		return App{}, ErrConfirmationRequired
	}
	a, err := s.store.SetTargets(ctx, c.Workspace, id, targets, uploads, c.Subject, s.now().UTC())
	if err != nil {
		return App{}, err
	}
	log.Printf("write targets: app=%s workspace=%s by=%s targets=%v uploads=%v", id, c.Workspace, c.Subject, targets, uploads)
	s.changed(id)
	return a, nil
}

// GrantRevoker revokes every grant a module holds for an app's principal. Take ownership calls it,
// and fails closed if it errors.
type GrantRevoker interface {
	// RevokeGrants revokes principal's grants in workspace. actingOwner is the workspace owner
	// taking the app over: the revoke token is minted under their access.
	RevokeGrants(ctx context.Context, workspace, principal, actingOwner string) error
}
