// Package config loads booth-streamlit's runtime configuration from environment variables.
// Every value maps 1:1 to a Helm chart value/env var, mirroring booth-catalog's and
// booth-storage's own internal/config; there is no config file format of our own to version.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
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

	// IframeIssuerURL is booth-core's iframe-identity issuer (ADR 0069), whose X-Booth-Identity
	// assertions this module verifies on every non-health request. Compared character-for-character
	// with the assertion's `iss`.
	IframeIssuerURL string

	// GroupsClaim is the claim carrying workspace memberships (ADR 0025); must match booth-core's.
	GroupsClaim string

	// MaxSourceBytes bounds one app's source (0 means the default, 256 KiB).
	MaxSourceBytes int

	// Lifecycle configures the per-app containers (design note (a)).
	Lifecycle Lifecycle
}

// Lifecycle is the per-app container configuration. Every value comes from the chart.
type Lifecycle struct {
	// Namespace the backend runs in, where app objects are created (the chart's downward API).
	Namespace string
	// SelfDeployment is the backend's own Deployment, which owns every app Deployment.
	SelfDeployment string
	RuntimeImage   string
	// GateImage is the image holding /booth-streamlit-gate: the backend's own image.
	GateImage      string
	PullPolicy     string
	ServiceAccount string
	// AppResources and GateResources are Kubernetes ResourceRequirements, as JSON.
	AppResources  string
	GateResources string
	TmpSizeLimit  string
	IdleTimeout   time.Duration
	MaxWebsocket  time.Duration
	MaxRunning    int
	MaxPerWS      int
}

// Load reads configuration from the environment.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:      getEnv("BOOTH_HTTP_ADDR", ":8080"),
		PostgresDSN:   os.Getenv("BOOTH_POSTGRES_DSN"),
		NATSURL:       os.Getenv("BOOTH_NATS_URL"),
		NATSCredsFile: os.Getenv("BOOTH_NATS_CREDS_FILE"),
		WebDir:        os.Getenv("BOOTH_WEB_DIR"),

		IframeIssuerURL: os.Getenv("BOOTH_IFRAME_IDENTITY_ISSUER_URL"),
		GroupsClaim:     getEnv("BOOTH_OIDC_GROUPS_CLAIM", "groups"),
		Lifecycle: Lifecycle{
			Namespace:      os.Getenv("BOOTH_NAMESPACE"),
			SelfDeployment: os.Getenv("BOOTH_SELF_DEPLOYMENT"),
			RuntimeImage:   os.Getenv("BOOTH_APP_RUNTIME_IMAGE"),
			GateImage:      os.Getenv("BOOTH_APP_GATE_IMAGE"),
			PullPolicy:     getEnv("BOOTH_APP_IMAGE_PULL_POLICY", "IfNotPresent"),
			ServiceAccount: os.Getenv("BOOTH_APP_SERVICE_ACCOUNT"),
			AppResources:   os.Getenv("BOOTH_APP_RESOURCES"),
			GateResources:  os.Getenv("BOOTH_APP_GATE_RESOURCES"),
			TmpSizeLimit:   getEnv("BOOTH_APP_TMP_SIZE_LIMIT", "256Mi"),
		},
	}
	if v := os.Getenv("BOOTH_STREAMLIT_MAX_SOURCE_BYTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return Config{}, fmt.Errorf("BOOTH_STREAMLIT_MAX_SOURCE_BYTES must be a positive integer, got %q", v)
		}
		cfg.MaxSourceBytes = n
	}

	if cfg.PostgresDSN == "" {
		return Config{}, fmt.Errorf("BOOTH_POSTGRES_DSN is required: app definitions live in this module's own database (ADR 0014/0053)")
	}
	if cfg.IframeIssuerURL == "" {
		return Config{}, fmt.Errorf("BOOTH_IFRAME_IDENTITY_ISSUER_URL is required: every app request is authenticated by booth-core's X-Booth-Identity assertion (ADR 0069)")
	}
	// Credentials with nowhere to connect is a wiring mistake, not a reason to run quietly without
	// publishing: say so at startup rather than leaving apps missing from the catalog.
	if cfg.NATSCredsFile != "" && cfg.NATSURL == "" {
		return Config{}, fmt.Errorf("BOOTH_NATS_CREDS_FILE is set but BOOTH_NATS_URL is not: the credentials have no bus to connect to")
	}
	l := &cfg.Lifecycle
	for _, req := range []struct{ name, val string }{
		{"BOOTH_NAMESPACE", l.Namespace}, {"BOOTH_SELF_DEPLOYMENT", l.SelfDeployment},
		{"BOOTH_APP_RUNTIME_IMAGE", l.RuntimeImage}, {"BOOTH_APP_GATE_IMAGE", l.GateImage},
		{"BOOTH_APP_SERVICE_ACCOUNT", l.ServiceAccount},
	} {
		if req.val == "" {
			return Config{}, fmt.Errorf("%s is required: the backend runs every app as its own Deployment", req.name)
		}
	}
	durations := []struct {
		name string
		dst  *time.Duration
		def  time.Duration
	}{
		{"BOOTH_APP_IDLE_TIMEOUT", &l.IdleTimeout, 30 * time.Minute},
		{"BOOTH_APP_MAX_WEBSOCKET", &l.MaxWebsocket, 8 * time.Hour},
	}
	for _, d := range durations {
		*d.dst = d.def
		if v := os.Getenv(d.name); v != "" {
			n, err := time.ParseDuration(v)
			if err != nil || n < 0 {
				return Config{}, fmt.Errorf("%s must be a non-negative duration such as 30m, got %q", d.name, v)
			}
			*d.dst = n
		}
	}
	ints := []struct {
		name string
		dst  *int
	}{{"BOOTH_APP_MAX_RUNNING", &l.MaxRunning}, {"BOOTH_APP_MAX_RUNNING_PER_WORKSPACE", &l.MaxPerWS}}
	for _, i := range ints {
		if v := os.Getenv(i.name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return Config{}, fmt.Errorf("%s must be a non-negative integer (0 = no cap), got %q", i.name, v)
			}
			*i.dst = n
		}
	}
	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
