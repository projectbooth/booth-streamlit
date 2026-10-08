// Package proxy forwards a verified viewer's traffic, including Streamlit's websocket, to one
// app's container (design note (b)). It is the only path into an app, which makes it the place
// that decides what an app's user-written code gets to see about the viewer.
//
// User code must not receive anything it could replay as the viewer, so before forwarding the
// proxy removes:
//   - X-Booth-Identity: a bearer for this module's own API, valid up to 2 minutes (ADR 0069).
//   - core's booth_iframe_session cookie: a renewable bearer for core's iframe proxy. Core forwards
//     it to modules unchanged, and Streamlit exposes cookies to app code (st.context.cookies).
//   - every other X-Booth-* header and Authorization.
//
// It then sets plain headers it has already verified (X-Booth-User/-Workspace/-Role), which app
// code reads through st.context.headers. On the way back it drops any attempt by the app to set
// core's session cookie, since the app is served from the shell's own origin.
package proxy

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/projectbooth/booth-streamlit/internal/identity"
)

// Headers set for the app. Anything else named X-Booth-* is removed.
const (
	HeaderUser      = "X-Booth-User"
	HeaderWorkspace = "X-Booth-Workspace"
	HeaderRole      = "X-Booth-Role"
)

// CoreSessionCookie is booth-core's iframe session cookie (gateway.IframeCookieName).
const CoreSessionCookie = "booth_iframe_session"

// App is what the proxy needs to know about one app.
type App struct {
	ID        string
	Workspace string
	// Target is the app container's base URL. The request path is forwarded unchanged
	// (/apps/<id>/...), because Streamlit runs with server.baseUrlPath=apps/<id>.
	Target *url.URL
}

// ErrNotFound means no such app.
var ErrNotFound = errors.New("app not found")

// Resolver looks up an app by id.
type Resolver interface {
	Resolve(ctx context.Context, id string) (App, error)
}

// Verifier is the identity check; *identity.Verifier satisfies it.
type Verifier interface {
	Verify(r *http.Request) (identity.Caller, error)
}

// Handler serves /apps/{id}/*.
type Handler struct {
	Verifier Verifier
	Apps     Resolver
}

// Mount registers the proxy on r.
func (h *Handler) Mount(r chi.Router) {
	r.HandleFunc("/apps/{id}", func(w http.ResponseWriter, req *http.Request) {
		// Streamlit's base path needs the trailing slash; keep the query (core's own iframe entry
		// redirect has already removed its token from it).
		target := req.URL.Path + "/"
		if req.URL.RawQuery != "" {
			target += "?" + req.URL.RawQuery
		}
		http.Redirect(w, req, target, http.StatusFound)
	})
	r.HandleFunc("/apps/{id}/*", h.serve)
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request) {
	caller, err := h.Verifier.Verify(r)
	if err != nil {
		log.Printf("proxy: refused %s %s: %v", r.Method, r.URL.Path, err)
		if errors.Is(err, identity.ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
		} else {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
		return
	}

	app, err := h.Apps.Resolve(r.Context(), chi.URLParam(r, "id"))
	// Sharing (ADR 0104 item 5): members of the app's own workspace only, any role. An app in
	// another workspace is reported exactly like a missing one, so ids can't be probed.
	if errors.Is(err, ErrNotFound) || (err == nil && app.Workspace != caller.Workspace) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		log.Printf("proxy: resolving app: %v", err)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(app.Target)
			// SetURL joins the target's path with ours; the target is a bare host, so the path stays
			// /apps/<id>/... . Keep the browser's Host: Streamlit's websocket handler compares the
			// Origin header with Host, and both name the shell's origin.
			pr.Out.Host = pr.In.Host
			scrub(pr.Out.Header)
			pr.Out.Header.Set(HeaderUser, caller.Subject)
			pr.Out.Header.Set(HeaderWorkspace, caller.Workspace)
			pr.Out.Header.Set(HeaderRole, string(caller.Role))
		},
		ModifyResponse: func(resp *http.Response) error {
			dropCoreCookie(resp.Header)
			return nil
		},
		FlushInterval: -1, // stream; Streamlit's long-polling health and the websocket need it
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Printf("proxy: app %s: %v", app.ID, err)
			http.Error(w, "app unavailable", http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

// scrub removes everything a viewer's request carries that user code could replay as the viewer.
func scrub(h http.Header) {
	for k := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), "X-Booth-") {
			h.Del(k)
		}
	}
	h.Del("Authorization")

	var kept []string
	for _, line := range h.Values("Cookie") {
		for _, part := range strings.Split(line, ";") {
			part = strings.TrimSpace(part)
			name, _, _ := strings.Cut(part, "=")
			if part == "" || name == CoreSessionCookie {
				continue
			}
			kept = append(kept, part)
		}
	}
	h.Del("Cookie")
	if len(kept) > 0 {
		h.Set("Cookie", strings.Join(kept, "; "))
	}
}

// dropCoreCookie removes any Set-Cookie the app sends for core's session cookie: the app is
// same-origin with the shell, so it could otherwise overwrite or clear the viewer's session.
func dropCoreCookie(h http.Header) {
	cookies := h.Values("Set-Cookie")
	h.Del("Set-Cookie")
	for _, c := range cookies {
		name, _, _ := strings.Cut(strings.TrimSpace(c), "=")
		if name != CoreSessionCookie {
			h.Add("Set-Cookie", c)
		}
	}
}
