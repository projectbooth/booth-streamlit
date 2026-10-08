package proxy

import (
	"context"
	"errors"
	"net/url"

	"github.com/projectbooth/booth-streamlit/internal/apps"
	"github.com/projectbooth/booth-streamlit/internal/identity"
	"github.com/projectbooth/booth-streamlit/internal/lifecycle"
)

// LifecycleResolver resolves an app through apps.Service (so the proxy applies exactly the
// visibility rule the API does) and the lifecycle's observed state.
//
// Opening an idle-suspended app wakes it, for anyone who may open it (apps.Service.Wake): idle
// shutdown is the module's choice, not the owner's (ADR 0105 reserves Start for owners). An app
// its owner stopped stays stopped.
type LifecycleResolver struct {
	Apps      *apps.Service
	Lifecycle *lifecycle.Controller
	Namespace string
}

// Resolve implements Resolver.
func (r LifecycleResolver) Resolve(ctx context.Context, c identity.Caller, id string) (App, error) {
	a, err := r.Apps.Wake(ctx, c, id)
	switch {
	case errors.Is(err, apps.ErrNotFound):
		return App{}, ErrNotFound
	case errors.Is(err, apps.ErrStopped):
		return App{}, ErrNotRunning
	case errors.Is(err, apps.ErrCapacity):
		return App{}, ErrBusy
	case err != nil:
		return App{}, err
	}
	st := r.Lifecycle.Status(a)
	switch st.State {
	case lifecycle.StateRunning:
	case lifecycle.StateFailed:
		return App{}, &FailedError{Reason: st.Reason}
	default:
		return App{}, ErrStarting
	}
	target, err := url.Parse(lifecycle.ServiceURL(r.Namespace, a.ID))
	if err != nil {
		return App{}, err
	}
	return App{ID: a.ID, Workspace: a.Workspace, Target: target, Bearer: a.GateBearer}, nil
}
