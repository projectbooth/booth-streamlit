// Command gate runs inside every app pod in front of Streamlit (package gate). It ships in the
// module backend's image, so the app pod's only non-Streamlit code is the module's own.
package main

import (
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
