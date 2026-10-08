package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"mime"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/projectbooth/booth-streamlit/internal/apps"
	"github.com/projectbooth/booth-streamlit/internal/identity"
)

// Verifier is the identity check; *identity.Verifier satisfies it.
type Verifier interface {
	Verify(r *http.Request) (identity.Caller, error)
}

// maxBodyBytes bounds a request body: the largest allowed source plus room for the other fields.
const maxBodyBytes = 1 << 20

// mountAppAPI registers the app-management API. The UI calls it with relative URLs from inside the
// iframe, so every request arrives through booth-core's iframe proxy carrying X-Booth-Identity,
// which is verified here on every route. Authorization is apps.Service's; this layer only maps
// its errors to status codes.
//
//	GET    /api/me                  the caller, and whether they may author apps
//	GET    /api/apps                apps the caller may see
//	POST   /api/apps                create (owners)
//	GET    /api/apps/{id}           one app, with source
//	PUT    /api/apps/{id}           edit (owners)
//	DELETE /api/apps/{id}           delete (owners)
//	POST   /api/apps/{id}/start     desired state running (owners)
//	POST   /api/apps/{id}/stop      desired state stopped (owners)
func mountAppAPI(r chi.Router, v Verifier, svc *apps.Service) {
	r.Route("/api", func(r chi.Router) {
		r.Use(authenticate(v))
		r.Get("/me", func(w http.ResponseWriter, r *http.Request) {
			c := caller(r)
			writeJSON(w, http.StatusOK, map[string]any{
				"subject": c.Subject, "workspace": c.Workspace, "role": c.Role, "canAuthor": apps.CanAuthor(c),
			})
		})
		r.Get("/apps", func(w http.ResponseWriter, r *http.Request) {
			list, err := svc.List(r.Context(), caller(r))
			if err != nil {
				writeErr(w, err)
				return
			}
			if list == nil {
				list = []apps.App{}
			}
			writeJSON(w, http.StatusOK, map[string]any{"apps": list})
		})
		r.Post("/apps", func(w http.ResponseWriter, r *http.Request) {
			var in apps.Input
			if !decode(w, r, &in) {
				return
			}
			a, err := svc.Create(r.Context(), caller(r), in)
			respond(w, http.StatusCreated, a, err)
		})
		r.Get("/apps/{id}", func(w http.ResponseWriter, r *http.Request) {
			a, err := svc.Get(r.Context(), caller(r), chi.URLParam(r, "id"))
			respond(w, http.StatusOK, a, err)
		})
		r.Put("/apps/{id}", func(w http.ResponseWriter, r *http.Request) {
			var in apps.Input
			if !decode(w, r, &in) {
				return
			}
			a, err := svc.Update(r.Context(), caller(r), chi.URLParam(r, "id"), in)
			respond(w, http.StatusOK, a, err)
		})
		r.Delete("/apps/{id}", func(w http.ResponseWriter, r *http.Request) {
			if err := svc.Delete(r.Context(), caller(r), chi.URLParam(r, "id")); err != nil {
				writeErr(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
		r.Post("/apps/{id}/start", setState(svc, apps.Running))
		r.Post("/apps/{id}/stop", setState(svc, apps.Stopped))
	})
}

func setState(svc *apps.Service, st apps.DesiredState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, err := svc.SetDesiredState(r.Context(), caller(r), chi.URLParam(r, "id"), st)
		respond(w, http.StatusOK, a, err)
	}
}

type callerKey struct{}

func authenticate(v Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := v.Verify(r)
			if err != nil {
				log.Printf("api: refused %s %s: %v", r.Method, r.URL.Path, err)
				if errors.Is(err, identity.ErrForbidden) {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
				} else {
					writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				}
				return
			}
			next.ServeHTTP(w, r.WithContext(contextWith(r, c)))
		})
	}
}

func contextWith(r *http.Request, c identity.Caller) context.Context {
	return context.WithValue(r.Context(), callerKey{}, c)
}

func caller(r *http.Request) identity.Caller {
	c, _ := r.Context().Value(callerKey{}).(identity.Caller)
	return c
}

// decode reads a JSON body. It requires Content-Type application/json, which a cross-site HTML
// form cannot send, and refuses unknown fields so a typo doesn't silently do nothing.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "Content-Type must be application/json"})
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return false
	}
	return true
}

func respond(w http.ResponseWriter, code int, a apps.App, err error) {
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, code, a)
}

func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, apps.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "app not found"})
	case errors.Is(err, apps.ErrForbidden):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
	case errors.Is(err, apps.ErrInvalid):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		log.Printf("api: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	}
}
