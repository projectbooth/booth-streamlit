package dataaccess

import (
	"sync"
	"time"
)

// Uses remembers, for a short while, which declared targets each viewer's writes and uploads went
// to, so the module's page can show the "Write allowed" prompt the first time one is used
// (ADR 0117 item 2; docs/design-app-writes.md, "Disclosure"). The write and upload proxies record a
// successful call; GET /api/apps/{id}/uses reads the caller's own.
//
// The viewer is X-Booth-App-User, which the app's code states (booth_streamlit sets it from the
// verified X-Booth-User). An app that leaves it out shows no prompt, and one that names someone
// else shows that person a prompt about a target the app does declare: the prompt is a disclosure,
// never a check. The Permissions control in the page's header doesn't depend on it.
//
// In memory, per backend replica (replicaCount is 1 by default), bounded, and forgotten after
// UseTTL. Nothing here is a row value or a file name.
type Uses struct {
	mu   sync.Mutex
	seen map[useKey]map[Use]time.Time // (app, viewer) -> target -> last use
	now  func() time.Time
}

// Use is one target: a booth-api API slug, or a Storage backend and folder.
type Use struct {
	Kind    string `json:"kind"` // "write" or "upload"
	API     string `json:"api,omitempty"`
	Backend string `json:"backend,omitempty"`
	Prefix  string `json:"prefix,omitempty"`
}

// UseAt is a Use and when it last happened.
type UseAt struct {
	Use
	At time.Time `json:"at"`
}

type useKey struct{ app, viewer string }

const (
	UseTTL = 12 * time.Hour
	// maxUseKeys bounds memory: past it, the oldest (app, viewer) is dropped. A dropped viewer only
	// misses a prompt; the Permissions control still lists everything.
	maxUseKeys          = 10000
	maxUsesPerViewerApp = 64
)

func (u *Uses) clock() time.Time {
	if u.now != nil {
		return u.now()
	}
	return time.Now()
}

// Record notes that viewer used t through app. An empty viewer is not recorded.
func (u *Uses) Record(app, viewer string, t Use) {
	if u == nil || viewer == "" {
		return
	}
	now := u.clock()
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.seen == nil {
		u.seen = map[useKey]map[Use]time.Time{}
	}
	k := useKey{app, viewer}
	m := u.seen[k]
	if m == nil {
		if len(u.seen) >= maxUseKeys {
			u.evictLocked(now)
		}
		m = map[Use]time.Time{}
		u.seen[k] = m
	}
	if _, ok := m[t]; !ok && len(m) >= maxUsesPerViewerApp {
		return // more targets than an app can usefully declare; the first ones were prompted already
	}
	m[t] = now
}

// For returns the targets viewer used through app within UseTTL.
func (u *Uses) For(app, viewer string) []UseAt {
	out := []UseAt{}
	if u == nil {
		return out
	}
	cutoff := u.clock().Add(-UseTTL)
	u.mu.Lock()
	defer u.mu.Unlock()
	for t, at := range u.seen[useKey{app, viewer}] {
		if at.After(cutoff) {
			out = append(out, UseAt{Use: t, At: at})
		}
	}
	return out
}

// evictLocked drops expired entries and, if still full, the (app, viewer) used least recently.
func (u *Uses) evictLocked(now time.Time) {
	cutoff := now.Add(-UseTTL)
	var oldestKey useKey
	var oldest time.Time
	for k, m := range u.seen {
		latest := time.Time{}
		for t, at := range m {
			if !at.After(cutoff) {
				delete(m, t)
			} else if at.After(latest) {
				latest = at
			}
		}
		if len(m) == 0 {
			delete(u.seen, k)
			continue
		}
		if oldest.IsZero() || latest.Before(oldest) {
			oldestKey, oldest = k, latest
		}
	}
	if len(u.seen) >= maxUseKeys {
		delete(u.seen, oldestKey)
	}
}
