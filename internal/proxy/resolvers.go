package proxy

import (
	"context"
	"errors"

	"github.com/projectbooth/booth-streamlit/internal/apps"
	"github.com/projectbooth/booth-streamlit/internal/identity"
)

// AppsResolver resolves apps from the app model, through apps.Service so the proxy applies exactly
// the visibility rule the API does. Until the lifecycle exists (build step 3) no app has a
// container, so every visible app resolves to ErrNotRunning.
type AppsResolver struct{ Apps *apps.Service }

// Resolve implements Resolver.
func (r AppsResolver) Resolve(ctx context.Context, c identity.Caller, id string) (App, error) {
	if _, err := r.Apps.Get(ctx, c, id); err != nil {
		if errors.Is(err, apps.ErrNotFound) {
			return App{}, ErrNotFound
		}
		return App{}, err
	}
	return App{}, ErrNotRunning
}

// Chain tries each resolver in turn, moving on only past ErrNotFound.
type Chain []Resolver

// Resolve implements Resolver.
func (ch Chain) Resolve(ctx context.Context, c identity.Caller, id string) (App, error) {
	for _, r := range ch {
		a, err := r.Resolve(ctx, c, id)
		if !errors.Is(err, ErrNotFound) {
			return a, err
		}
	}
	return App{}, ErrNotFound
}
