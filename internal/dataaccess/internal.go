package dataaccess

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/projectbooth/booth-streamlit/internal/apps"
)

// Internal is the backend's internal port (8081): reachable only from app pods (NetworkPolicy),
// never through booth-core's gateway or iframe proxy. Two endpoints:
//
//	POST /internal/token
//	    The gate's token refresh. Authorization: Bearer <the app's gate bearer>. Answers the app's
//	    current workload token, or 403 with the reason while its owner has no access.
//	POST /internal/broker/api/credentials
//	    The credential sidecars' --core-url points here, so app pods never need a route to booth-core
//	    (ADR 0107 item 3). Forwarded to core's broker unchanged, but only a read request for one of
//	    this module's apps, in that app's workspace.
type Internal struct {
	Apps    *apps.Service
	Tokens  *Tokens
	CoreURL string // booth-core, for /api/credentials
	HTTP    *http.Client
	// Files, if set, serves the file read proxy (/files/...) on the same port.
	Files *Files
}

// Router returns the internal port's handler.
func (in *Internal) Router() http.Handler {
	r := chi.NewRouter()
	r.Post("/internal/token", in.token)
	r.Post("/internal/broker/api/credentials", in.broker)
	if in.Files != nil {
		in.Files.mount(r, in)
	}
	return r
}

// appFor identifies the calling app by its gate bearer and gets its current token. It writes the
// response itself when it fails: 401 for an unknown bearer, 403 with the reason while the owner has
// no access (recording the pause), 503 when core is unreachable.
func (in *Internal) appFor(w http.ResponseWriter, r *http.Request) (apps.App, Token, bool) {
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || len(bearer) < 32 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return apps.App{}, Token{}, false
	}
	a, err := in.Apps.ByBearer(r.Context(), bearer)
	// The lookup is by exact value; the constant-time compare is belt and braces against a store
	// that ever matched loosely.
	if err != nil || subtle.ConstantTimeCompare([]byte(a.GateBearer), []byte(bearer)) != 1 {
		log.Printf("internal: %s %s refused from %s: unknown bearer", r.Method, r.URL.Path, r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return apps.App{}, Token{}, false
	}
	tok, err := in.Tokens.Get(r.Context(), a)
	switch {
	case errors.Is(err, ErrOwnerNoAccess), errors.Is(err, ErrRoleExceeded):
		if _, perr := in.Apps.DataPaused(r.Context(), a, err.Error()); perr != nil {
			log.Printf("internal: recording pause for %s: %v", a.ID, perr)
		}
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "data_access_paused", "reason": err.Error()})
		return apps.App{}, Token{}, false
	case err != nil:
		log.Printf("internal: minting for %s: %v", a.ID, err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
		return apps.App{}, Token{}, false
	}
	if err := in.Apps.DataResumed(r.Context(), a); err != nil {
		log.Printf("internal: clearing pause for %s: %v", a.ID, err)
	}
	return a, tok, true
}

func (in *Internal) token(w http.ResponseWriter, r *http.Request) {
	_, tok, ok := in.appFor(w, r)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, tok)
}

// brokerRequest is the subset of contracts/credential-broker.md's request the forwarder checks.
type brokerRequest struct {
	Kind   string          `json:"kind"`
	Access string          `json:"access"`
	Scope  json.RawMessage `json:"scope"`
}

func (in *Internal) broker(w http.ResponseWriter, r *http.Request) {
	refuse := func(code int, why string) {
		log.Printf("internal: broker request refused from %s: %s", r.RemoteAddr, why)
		writeJSON(w, code, map[string]string{"error": why})
	}
	jwt, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		refuse(http.StatusUnauthorized, "no token")
		return
	}
	// Core verifies the token itself; here its subject only has to name one of our apps, in the
	// workspace the request is for. Anything else is not ours to forward.
	sub, err := unverifiedSubject(jwt)
	if err != nil {
		refuse(http.StatusUnauthorized, "unreadable token")
		return
	}
	rest, ok := strings.CutPrefix(sub, "streamlit:")
	ws, appID, ok2 := strings.Cut(rest, ":")
	if !ok || !ok2 || ws == "" || appID == "" || r.Header.Get("X-Workspace") != ws {
		refuse(http.StatusForbidden, "not a booth-streamlit app token for this workspace")
		return
	}
	if _, err := in.Apps.Lookup(r.Context(), ws, appID); err != nil {
		refuse(http.StatusForbidden, "no such app")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		refuse(http.StatusBadRequest, "unreadable body")
		return
	}
	var req brokerRequest
	if err := json.Unmarshal(body, &req); err != nil {
		refuse(http.StatusBadRequest, "unreadable body")
		return
	}
	// The viewer cap at the forwarder too (ADR 0104 item 1): read only, known kinds only.
	if req.Access != "read" {
		refuse(http.StatusForbidden, "apps may only request read access")
		return
	}
	if req.Kind != "postgres" && req.Kind != "s3" {
		refuse(http.StatusForbidden, "unknown credential kind")
		return
	}
	if req.Kind == "postgres" {
		var sc struct {
			Workspace string `json:"workspace"`
		}
		if json.Unmarshal(req.Scope, &sc) != nil || sc.Workspace != ws {
			refuse(http.StatusForbidden, "postgres scope must be the app's own workspace")
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	out, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(in.CoreURL, "/")+"/api/credentials", bytes.NewReader(body))
	out.Header.Set("Authorization", "Bearer "+jwt)
	out.Header.Set("X-Workspace", ws)
	out.Header.Set("Content-Type", "application/json")
	hc := in.HTTP
	if hc == nil {
		hc = &http.Client{}
	}
	resp, err := hc.Do(out)
	if err != nil {
		log.Printf("internal: broker: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "booth-core unreachable"})
		return
	}
	defer resp.Body.Close()
	// Relayed as-is; the credential in it is never logged or kept.
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 1<<20))
}

// unverifiedSubject reads a JWT's `sub` without verifying it. Only for routing a request that core
// then verifies; never for an authorization decision of our own.
func unverifiedSubject(jwt string) (string, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return "", errors.New("not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", err
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Sub == "" {
		return "", errors.New("no subject")
	}
	return claims.Sub, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
