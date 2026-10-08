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
	"fmt"
	"html"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/projectbooth/booth-streamlit/internal/gate"
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
	// Target is the app's gate (its Service). The request path is forwarded unchanged
	// (/apps/<id>/...), because Streamlit runs with server.baseUrlPath=apps/<id>.
	Target *url.URL
	// Bearer is presented to the gate as gate.HeaderToken (design note (b)).
	Bearer string
}

// Resolver errors, each answered differently. Their texts are shown to the viewer.
var (
	// ErrNotFound: no such app, or one the caller may not open; the two are indistinguishable.
	ErrNotFound = errors.New("app not found")
	// ErrNotRunning: its owner stopped it.
	ErrNotRunning = errors.New("This app is stopped. A workspace owner can start it.")
	// ErrStarting: it is (re)starting; a page that refreshes itself is served meanwhile.
	ErrStarting = errors.New("This app is starting…")
	// ErrBusy: waking it would exceed the running-app cap.
	ErrBusy = errors.New("Too many apps are running right now. Try again later, or ask a workspace owner to stop one.")
)

// FailedError: its container won't start; Reason says why (for the app's owners to fix).
type FailedError struct{ Reason string }

func (e *FailedError) Error() string { return "This app failed to start (" + e.Reason + ")." }

// Activity records traffic for idle shutdown; *lifecycle.Controller satisfies it.
type Activity interface {
	Touch(id string)
	WebsocketOpened(id string)
	WebsocketClosed(id string)
}

// DefaultMaxWebsocket bounds one websocket's life (design note (a)): an abandoned tab would
// otherwise keep its app from idling forever, and identity is re-checked when Streamlit's client
// reconnects.
const DefaultMaxWebsocket = 8 * time.Hour

// Resolver looks up an app for a verified caller. It applies the visibility rule (the app's own
// workspace only; owners always, others only if shared) and returns ErrNotFound otherwise.
type Resolver interface {
	Resolve(ctx context.Context, c identity.Caller, id string) (App, error)
}

// Verifier is the identity check; *identity.Verifier satisfies it.
type Verifier interface {
	Verify(r *http.Request) (identity.Caller, error)
}

// Handler serves /apps/{id}/*.
type Handler struct {
	Verifier Verifier
	Apps     Resolver
	// Activity, if set, is told about every request and websocket.
	Activity Activity
	// MaxWebsocket bounds a websocket's life; 0 means DefaultMaxWebsocket.
	MaxWebsocket time.Duration
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

	app, err := h.Apps.Resolve(r.Context(), caller, chi.URLParam(r, "id"))
	// Sharing (ADR 0104 item 5, ADR 0105): the resolver applies it. The workspace check here is a
	// second guard, so a resolver bug can't open another workspace's app.
	if errors.Is(err, ErrNotFound) || (err == nil && app.Workspace != caller.Workspace) {
		http.NotFound(w, r)
		return
	}
	var failed *FailedError
	switch {
	case errors.Is(err, ErrStarting):
		unavailable(w, r, err.Error(), true)
		return
	case errors.Is(err, ErrNotRunning), errors.Is(err, ErrBusy), errors.As(err, &failed):
		unavailable(w, r, err.Error(), false)
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
			if app.Bearer != "" {
				pr.Out.Header.Set(gate.HeaderToken, app.Bearer)
			}
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
	if h.Activity != nil {
		h.Activity.Touch(app.ID)
	}
	if isUpgrade(r) {
		// ReverseProxy holds an upgraded connection open until it ends or the request's context
		// does, so this both counts the open websocket and caps its life.
		limit := h.MaxWebsocket
		if limit == 0 {
			limit = DefaultMaxWebsocket
		}
		ctx, cancel := context.WithTimeout(r.Context(), limit)
		defer cancel()
		r = r.WithContext(ctx)
		if h.Activity != nil {
			h.Activity.WebsocketOpened(app.ID)
			defer h.Activity.WebsocketClosed(app.ID)
		}
	}
	rp.ServeHTTP(w, r)
}

func isUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// unavailable answers 503. A page load (the iframe navigating to the app) gets a small page, which
// refreshes itself while the app starts; anything else gets plain text.
func unavailable(w http.ResponseWriter, r *http.Request, msg string, refresh bool) {
	w.Header().Set("Cache-Control", "no-store")
	dest := r.Header.Get("Sec-Fetch-Dest")
	page := r.Method == http.MethodGet && (dest == "iframe" || dest == "document" || strings.Contains(r.Header.Get("Accept"), "text/html"))
	if !page {
		http.Error(w, msg, http.StatusServiceUnavailable)
		return
	}
	meta := ""
	if refresh {
		w.Header().Set("Retry-After", "2")
		meta = `<meta http-equiv="refresh" content="2">`
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = fmt.Fprintf(w, `<!doctype html><meta charset="utf-8">%s<title>Streamlit app</title>
<body style="font-family:system-ui,sans-serif;padding:2rem;color:#374151"><p>%s</p></body>`, meta, html.EscapeString(msg))
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
