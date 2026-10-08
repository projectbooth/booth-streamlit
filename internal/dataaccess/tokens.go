package dataaccess

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/projectbooth/booth-streamlit/internal/apps"
)

// RefusalMemory is how long a refused mint is remembered, so a burst of requests for a paused app
// costs one call to core (booth-api's rule, docs/decisions/0006).
const RefusalMemory = 30 * time.Second

// Tokens keeps each app's current workload token in memory, re-minting when RemintAfter of its life
// has passed (default two thirds: about every 6.7 minutes for core's 10-minute tokens).
type Tokens struct {
	Minter Minter
	// RemintAfter is the fraction of a token's life after which it is re-minted (0 < x <= 1).
	RemintAfter float64
	// MaxAge, if set, re-mints at least this often whatever the token's life (a test knob, so a
	// lost owner is noticed in seconds instead of minutes).
	MaxAge time.Duration
	Now    func() time.Time

	mu      sync.Mutex
	entries map[string]entry // app id
}

type entry struct {
	tok      Token
	mintedAt time.Time
	owner    string // the owner it was minted for; a take-over invalidates it
	refused  time.Time
	err      error
}

// Get returns the app's token, minting if there is none, it is due, or the owner changed.
// ErrOwnerNoAccess when core refuses the owner; ErrRoleExceeded if core granted more than viewer.
func (t *Tokens) Get(ctx context.Context, a apps.App) (Token, error) {
	now := t.now()
	t.mu.Lock()
	if t.entries == nil {
		t.entries = map[string]entry{}
	}
	e, ok := t.entries[a.ID]
	t.mu.Unlock()

	if ok && e.owner == a.Owner {
		if e.err != nil && now.Sub(e.refused) < RefusalMemory {
			return Token{}, e.err
		}
		if e.err == nil && now.Before(t.due(e)) {
			return e.tok, nil
		}
	}

	tok, err := t.Minter.Mint(ctx, a.Workspace, Subject(a.Workspace, a.ID), a.Owner)
	if err == nil && tok.Role != RoleCeiling {
		err = ErrRoleExceeded
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case errors.Is(err, ErrOwnerNoAccess), errors.Is(err, ErrRoleExceeded):
		t.entries[a.ID] = entry{owner: a.Owner, refused: now, err: err}
		return Token{}, err
	case err != nil:
		// Transient (core unreachable): keep serving a still-valid token rather than drop data.
		if ok && e.err == nil && now.Before(e.tok.ExpiresAt) {
			return e.tok, nil
		}
		return Token{}, err
	}
	t.entries[a.ID] = entry{tok: tok, mintedAt: now, owner: a.Owner}
	return tok, nil
}

// Forget drops an app's token (deleted app).
func (t *Tokens) Forget(id string) {
	t.mu.Lock()
	delete(t.entries, id)
	t.mu.Unlock()
}

func (t *Tokens) due(e entry) time.Time {
	f := t.RemintAfter
	if f <= 0 || f > 1 {
		f = 2.0 / 3
	}
	d := e.mintedAt.Add(time.Duration(float64(e.tok.ExpiresAt.Sub(e.mintedAt)) * f))
	if t.MaxAge > 0 {
		if m := e.mintedAt.Add(t.MaxAge); m.Before(d) {
			d = m
		}
	}
	return d
}

func (t *Tokens) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}
