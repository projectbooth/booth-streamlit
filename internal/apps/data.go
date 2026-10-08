package apps

import (
	"context"
	"log"

	"github.com/projectbooth/booth-streamlit/internal/identity"
)

// Data access (ADR 0104/0107): an app reads data as its Owner, capped at viewer. These are the
// app-model operations it needs; minting and the internal endpoints live in package dataaccess.

// ByBearer finds the app whose gate presents bearer, in any workspace. Only the backend's internal
// port calls it, with a bearer from an app pod's gate.
func (s *Service) ByBearer(ctx context.Context, bearer string) (App, error) {
	return s.store.GetByBearer(ctx, bearer)
}

// DataPaused records that core refused to mint for the app's owner. On the transition from working
// to paused it also bumps DataEpoch, which rolls the pod: the Postgres sidecar keeps an issued lease
// until it expires (up to an hour) even when renewal is refused, and rolling the pod is what ends
// connections opened under it (ADR 0107 item 6). It returns the app as stored.
func (s *Service) DataPaused(ctx context.Context, a App, reason string) (App, error) {
	if a.DataPausedReason == reason {
		return a, nil
	}
	transition := a.DataPausedReason == ""
	out, err := s.store.SetDataPaused(ctx, a.Workspace, a.ID, reason, s.now().UTC(), transition)
	if err != nil {
		return a, err
	}
	log.Printf("data access paused: app=%s workspace=%s owner=%s reason=%q", a.ID, a.Workspace, a.Owner, reason)
	if transition {
		s.changed(a.ID)
	}
	return out, nil
}

// DataResumed clears a pause after a successful mint.
func (s *Service) DataResumed(ctx context.Context, a App) error {
	if a.DataPausedReason == "" {
		return nil
	}
	if _, err := s.store.SetDataPaused(ctx, a.Workspace, a.ID, "", s.now().UTC(), false); err != nil {
		return err
	}
	log.Printf("data access resumed: app=%s workspace=%s owner=%s", a.ID, a.Workspace, a.Owner)
	s.changed(a.ID)
	return nil
}

// TakeOwnership makes the caller the app's owner (ADR 0107 item 7): any current owner of the app's
// workspace, and only while the app's data access is paused because its owner lost access. Each
// take-over is logged and kept: who, which app, the previous owner.
func (s *Service) TakeOwnership(ctx context.Context, c identity.Caller, id string) (App, error) {
	a, err := s.authorizeWrite(ctx, c, id)
	if err != nil {
		return App{}, err
	}
	if a.DataPausedReason == "" {
		return App{}, ErrNotPaused
	}
	prev, err := s.store.TakeOwnership(ctx, a.Workspace, a.ID, c.Subject, a.DataPausedReason, s.now().UTC())
	if err != nil {
		return App{}, err
	}
	log.Printf("take ownership: app=%s workspace=%s by=%s previous_owner=%s reason=%q", a.ID, a.Workspace, c.Subject, prev, a.DataPausedReason)
	s.changed(a.ID)
	return s.store.Get(ctx, a.Workspace, a.ID)
}

// OwnershipChanges lists an app's take-overs, for anyone who may see the app.
func (s *Service) OwnershipChanges(ctx context.Context, c identity.Caller, id string) ([]OwnershipChange, error) {
	if _, err := s.Get(ctx, c, id); err != nil {
		return nil, err
	}
	return s.store.OwnershipChanges(ctx, c.Workspace, id)
}

// Lookup returns an app by workspace and id with no caller: only the backend's internal port uses
// it, to check that a workload token names one of this module's apps.
func (s *Service) Lookup(ctx context.Context, workspace, id string) (App, error) {
	return s.store.Get(ctx, workspace, id)
}
