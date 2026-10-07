// Package config loads booth-streamlit's runtime configuration from environment variables.
// Every value maps 1:1 to a Helm chart value/env var, mirroring booth-catalog's and
// booth-storage's own internal/config; there is no config file format of our own to version.
package config

import (
	"fmt"
	"os"
)

// Config is booth-streamlit's full runtime configuration.
type Config struct {
	// HTTPAddr is the address the HTTP server listens on.
	HTTPAddr string

	// PostgresDSN is the connection string for this module's own database on the shared
	// PostgreSQL cluster (ADR 0014). booth-core provisions it when the manifest declares
	// `database: {enabled: true}` (ADR 0053) and the chart wires its `dsn` key here.
	PostgresDSN string

	// NATSURL is the event bus (ADR 0021) that dashboard.* events are published to (ADR 0018,
	// 0046). Empty disables the bus connection: apps still run, but booth-catalog never hears
	// about them. Chart installs always set it unless the operator turns the bus off on purpose.
	NATSURL string

	// NATSCredsFile is the NATS ".creds" file booth-core mints for this module's declared
	// `events` (ADR 0050). The chart mounts the "booth-event-bus-credentials" Secret and points
	// this at its "nats.creds" key. Only omit it against a bus with authentication off (a test
	// stand-in).
	NATSCredsFile string

	// WebDir is the directory holding the built UI (web/dist). The container image sets it;
	// empty means no UI is served, which is what local `go run` and the Go tests get.
	WebDir string
}

// Load reads configuration from the environment.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:      getEnv("BOOTH_HTTP_ADDR", ":8080"),
		PostgresDSN:   os.Getenv("BOOTH_POSTGRES_DSN"),
		NATSURL:       os.Getenv("BOOTH_NATS_URL"),
		NATSCredsFile: os.Getenv("BOOTH_NATS_CREDS_FILE"),
		WebDir:        os.Getenv("BOOTH_WEB_DIR"),
	}

	if cfg.PostgresDSN == "" {
		return Config{}, fmt.Errorf("BOOTH_POSTGRES_DSN is required: app definitions live in this module's own database (ADR 0014/0053)")
	}
	// Credentials with nowhere to connect is a wiring mistake, not a reason to run quietly without
	// publishing: say so at startup rather than leaving apps missing from the catalog.
	if cfg.NATSCredsFile != "" && cfg.NATSURL == "" {
		return Config{}, fmt.Errorf("BOOTH_NATS_CREDS_FILE is set but BOOTH_NATS_URL is not: the credentials have no bus to connect to")
	}
	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
