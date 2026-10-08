// Package gate is the small proxy in front of Streamlit inside every app pod (design note (b)).
//
// Streamlit binds 127.0.0.1 only, so the gate is the pod's one network entry point. It refuses
// every request that doesn't carry the app's per-app bearer, which only the module backend has (it
// generated it, stored it, and wrote it into a Secret mounted into this container and no other).
// That makes the X-Booth-User/-Workspace/-Role headers the backend sets trustworthy to app code
// even if something other than the backend reaches the pod, which NetworkPolicy alone can't
// promise on a cluster whose CNI doesn't enforce it (ARCHITECTURE.md item 37b).
package gate

import (
	"crypto/subtle"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// HeaderToken carries the per-app bearer from the backend to the gate. The gate removes it before
// forwarding, so Streamlit and the app's code never see it.
const HeaderToken = "X-Booth-Gate-Token"

// HealthPath is the gate's own readiness endpoint. It needs no bearer (the kubelet probes it) and
// reveals nothing but whether Streamlit answers.
const HealthPath = "/_booth/gate/healthz"

// Config configures a gate.
type Config struct {
	// Bearer is the app's per-app secret. Empty refuses everything (fail closed).
	Bearer string
	// Upstream is Streamlit's loopback address, e.g. http://127.0.0.1:8501.
	Upstream *url.URL
	// UpstreamHealth is Streamlit's health path, e.g. /apps/<id>/_stcore/health.
	UpstreamHealth string
}

// New returns the gate's handler.
func New(cfg Config) http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(cfg.Upstream)
			pr.Out.Host = pr.In.Host // Streamlit's websocket origin check compares Origin with Host
			pr.Out.Header.Del(HeaderToken)
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Printf("gate: upstream: %v", err)
			http.Error(w, "app unavailable", http.StatusBadGateway)
		},
	}
	health := &http.Client{Timeout: 2 * time.Second}
	want := []byte(cfg.Bearer)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == HealthPath {
			resp, err := health.Get(strings.TrimSuffix(cfg.Upstream.String(), "/") + cfg.UpstreamHealth)
			if err != nil {
				http.Error(w, "streamlit not answering", http.StatusServiceUnavailable)
				return
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				http.Error(w, "streamlit not healthy", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		got := []byte(r.Header.Get(HeaderToken))
		if len(want) == 0 || subtle.ConstantTimeCompare(got, want) != 1 {
			log.Printf("gate: refused %s %s from %s: missing or wrong bearer", r.Method, r.URL.Path, r.RemoteAddr)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		rp.ServeHTTP(w, r)
	})
}
