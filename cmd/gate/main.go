// Command gate runs inside every app pod in front of Streamlit (package gate). It ships in the
// module backend's image, so the app pod's only non-Streamlit code is the module's own.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/projectbooth/booth-streamlit/internal/gate"
)

func main() {
	listen := getEnv("BOOTH_GATE_LISTEN", ":8080")
	upstream, err := url.Parse(getEnv("BOOTH_GATE_UPSTREAM", "http://127.0.0.1:8501"))
	if err != nil {
		log.Fatalf("BOOTH_GATE_UPSTREAM: %v", err)
	}
	health := os.Getenv("BOOTH_GATE_UPSTREAM_HEALTH")
	if health == "" {
		log.Fatal("BOOTH_GATE_UPSTREAM_HEALTH is required (Streamlit's /<baseUrlPath>/_stcore/health)")
	}
	raw, err := os.ReadFile(getEnv("BOOTH_GATE_BEARER_FILE", "/etc/booth/gate/bearer"))
	if err != nil {
		log.Fatalf("reading the gate bearer: %v", err)
	}
	bearer := strings.TrimSpace(string(raw))
	if len(bearer) < 32 {
		log.Fatal("the gate bearer is missing or too short; refusing to start")
	}

	// Data access (ADR 0107): keep the app's workload token on the volume only the sidecars share,
	// and tell the app's own code (on loopback only) whether its data access works.
	if tokenURL := os.Getenv("BOOTH_GATE_TOKEN_URL"); tokenURL != "" {
		r := &gate.Refresher{URL: tokenURL, Bearer: bearer, File: getEnv("BOOTH_GATE_TOKEN_FILE", "/var/run/booth/token/token")}
		if v := os.Getenv("BOOTH_GATE_REFRESH_MAX"); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				log.Fatalf("BOOTH_GATE_REFRESH_MAX: %v", err)
			}
			r.MaxInterval = d
		}
		go r.Run(context.Background())
		statusAddr := getEnv("BOOTH_GATE_STATUS_LISTEN", "127.0.0.1:8090")
		if host, _, _ := strings.Cut(statusAddr, ":"); host != "127.0.0.1" {
			log.Fatal("BOOTH_GATE_STATUS_LISTEN must be a 127.0.0.1 address: it is for the app's own code only")
		}
		go func() {
			s := &http.Server{Addr: statusAddr, Handler: r.StatusHandler(), ReadHeaderTimeout: 5 * time.Second}
			log.Fatal(s.ListenAndServe())
		}()
	}

	srv := &http.Server{
		Addr:              listen,
		Handler:           gate.New(gate.Config{Bearer: bearer, Upstream: upstream, UpstreamHealth: health}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("gate listening on %s, forwarding to %s", listen, upstream)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func getEnv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
