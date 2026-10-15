package apps

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// App uploads (ADR 0117 items 1a, 6 and 7; booth-storage's docs/contracts/upload-grants.md). An
// owner declares here which booth-storage folders an app may add files to; the owner also grants
// the app on booth-storage's Admin page ("App uploads"), with their own token, and that grant sets
// the extensions and the size cap. An upload needs both. Uploads are create-only: no overwrite, no
// delete, nothing listed through the upload proxy.

// MaxUploadTargets bounds one app's upload declaration.
const MaxUploadTargets = 20

// maxUploadPrefixLen bounds a declared folder.
const maxUploadPrefixLen = 512

// UploadTarget is one declared folder: a booth-storage backend id and a prefix in it. Prefix is ""
// for the backend's root, otherwise it ends in "/" (booth-storage's backend.CleanPrefix).
type UploadTarget struct {
	Backend string `json:"backend"`
	Prefix  string `json:"prefix"`
}

func (t UploadTarget) String() string { return t.Backend + ":" + t.Prefix }

// backendIDPattern is booth-storage's backend id rule, as the file read proxy checks it.
var backendIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// CleanUploadPrefix normalizes a declared folder the way booth-storage does: no leading "/", ""
// for the root, otherwise ending in "/". It refuses "." and ".." segments, empty segments, a
// backslash and control characters, so the folder an owner sees is the folder files land in.
func CleanUploadPrefix(p string) (string, error) {
	p = strings.TrimPrefix(strings.TrimSpace(p), "/")
	if p == "" {
		return "", nil
	}
	if len(p) > maxUploadPrefixLen {
		return "", fmt.Errorf("a folder is at most %d characters", maxUploadPrefixLen)
	}
	for _, seg := range strings.Split(strings.TrimSuffix(p, "/"), "/") {
		switch {
		case seg == "" || seg == "." || seg == "..":
			return "", errors.New(`a folder can't have empty, "." or ".." parts`)
		case strings.ContainsAny(seg, `\`) || strings.IndexFunc(seg, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0:
			return "", errors.New("a folder can't contain a backslash or control characters")
		}
	}
	return strings.TrimSuffix(p, "/") + "/", nil
}

// AllowsUpload reports whether the app's declaration lets it add files directly in this folder.
func (a App) AllowsUpload(backend, prefix string) bool {
	return slices.Contains(a.UploadTargets, UploadTarget{Backend: backend, Prefix: prefix})
}

// Uploads reports whether the app declares any upload target.
func (a App) Uploads() bool { return len(a.UploadTargets) > 0 }

// ChangesData reports whether the app declares any write or upload target: such an app runs with
// egress closed and opens under the header every viewer sees (ADR 0117 items 2 and 7).
func (a App) ChangesData() bool { return a.Writes() || a.Uploads() }

func validateUploadTargets(in []UploadTarget) ([]UploadTarget, error) {
	if len(in) > MaxUploadTargets {
		return nil, fmt.Errorf("%w: at most %d upload targets", ErrInvalid, MaxUploadTargets)
	}
	out := make([]UploadTarget, 0, len(in))
	for _, t := range in {
		if !backendIDPattern.MatchString(t.Backend) {
			return nil, fmt.Errorf("%w: %q is not a Storage backend id (as shown on the Storage page)", ErrInvalid, t.Backend)
		}
		p, err := CleanUploadPrefix(t.Prefix)
		if err != nil {
			return nil, fmt.Errorf("%w: folder %q: %v", ErrInvalid, t.Prefix, err)
		}
		c := UploadTarget{Backend: t.Backend, Prefix: p}
		if slices.Contains(out, c) {
			return nil, fmt.Errorf("%w: upload target %s is listed twice", ErrInvalid, c)
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b UploadTarget) int { return strings.Compare(a.String(), b.String()) })
	return out, nil
}

// widensUploads reports whether next adds a folder cur doesn't have.
func widensUploads(cur, next []UploadTarget) bool {
	for _, t := range next {
		if !slices.Contains(cur, t) {
			return true
		}
	}
	return false
}

// Revokers revokes an app's grants in every module that may hold some (booth-api, booth-storage),
// in order, and stops at the first that fails: take ownership then fails closed.
type Revokers []GrantRevoker

// RevokeGrants implements GrantRevoker.
func (rs Revokers) RevokeGrants(ctx context.Context, workspace, principal, actingOwner string) error {
	for _, r := range rs {
		if err := r.RevokeGrants(ctx, workspace, principal, actingOwner); err != nil {
			return err
		}
	}
	return nil
}
