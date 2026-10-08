package config

import (
	"strings"
	"testing"
	"time"
)

var allVars = []string{
	"BOOTH_HTTP_ADDR", "BOOTH_POSTGRES_DSN", "BOOTH_NATS_URL", "BOOTH_NATS_CREDS_FILE", "BOOTH_WEB_DIR",
	"BOOTH_IFRAME_IDENTITY_ISSUER_URL", "BOOTH_OIDC_GROUPS_CLAIM", "BOOTH_STREAMLIT_MAX_SOURCE_BYTES",
	"BOOTH_NAMESPACE", "BOOTH_SELF_DEPLOYMENT", "BOOTH_APP_RUNTIME_IMAGE", "BOOTH_APP_GATE_IMAGE", "BOOTH_APP_SERVICE_ACCOUNT",
	"BOOTH_APP_IMAGE_PULL_POLICY", "BOOTH_APP_RESOURCES", "BOOTH_APP_GATE_RESOURCES", "BOOTH_APP_TMP_SIZE_LIMIT",
	"BOOTH_APP_IDLE_TIMEOUT", "BOOTH_APP_MAX_WEBSOCKET", "BOOTH_APP_MAX_RUNNING", "BOOTH_APP_MAX_RUNNING_PER_WORKSPACE",
}

// minimal is the smallest valid environment.
func minimal() map[string]string {
	return map[string]string{
		"BOOTH_POSTGRES_DSN":               "postgres://x",
		"BOOTH_IFRAME_IDENTITY_ISSUER_URL": "http://core/iframe-identity",
		"BOOTH_NAMESPACE":                  "booth-streamlit",
		"BOOTH_SELF_DEPLOYMENT":            "booth-streamlit",
		"BOOTH_APP_RUNTIME_IMAGE":          "runtime:1",
		"BOOTH_APP_GATE_IMAGE":             "backend:1",
		"BOOTH_APP_SERVICE_ACCOUNT":        "booth-streamlit-app",
	}
}

// setEnv clears every variable Load reads, then sets the given ones, so a developer's own
// environment can't leak into a case.
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range allVars {
		t.Setenv(k, "")
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func with(extra map[string]string) map[string]string {
	m := minimal()
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestLoad_Defaults(t *testing.T) {
	setEnv(t, minimal())
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.GroupsClaim != "groups" || cfg.MaxSourceBytes != 0 {
		t.Errorf("defaults: %+v", cfg)
	}
	l := cfg.Lifecycle
	if l.IdleTimeout != 30*time.Minute || l.MaxWebsocket != 8*time.Hour || l.MaxRunning != 0 || l.PullPolicy != "IfNotPresent" || l.TmpSizeLimit != "256Mi" {
		t.Errorf("lifecycle defaults: %+v", l)
	}
}

func TestLoad_RequiredValues(t *testing.T) {
	for _, name := range []string{
		"BOOTH_POSTGRES_DSN", "BOOTH_IFRAME_IDENTITY_ISSUER_URL", "BOOTH_NAMESPACE", "BOOTH_SELF_DEPLOYMENT",
		"BOOTH_APP_RUNTIME_IMAGE", "BOOTH_APP_GATE_IMAGE", "BOOTH_APP_SERVICE_ACCOUNT",
	} {
		env := minimal()
		delete(env, name)
		setEnv(t, env)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("without %s: err = %v, want one naming it", name, err)
		}
	}
}

func TestLoad_CredentialsWithoutURLIsRefused(t *testing.T) {
	setEnv(t, with(map[string]string{"BOOTH_NATS_CREDS_FILE": "/etc/booth/event-bus/nats.creds"}))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "BOOTH_NATS_URL") {
		t.Fatalf("err = %v, want one naming BOOTH_NATS_URL", err)
	}
}

func TestLoad_ReadsEverything(t *testing.T) {
	setEnv(t, with(map[string]string{
		"BOOTH_HTTP_ADDR":                     ":9090",
		"BOOTH_NATS_URL":                      "nats://nats:4222",
		"BOOTH_NATS_CREDS_FILE":               "/creds",
		"BOOTH_WEB_DIR":                       "/web",
		"BOOTH_OIDC_GROUPS_CLAIM":             "memberships",
		"BOOTH_STREAMLIT_MAX_SOURCE_BYTES":    "4096",
		"BOOTH_APP_IMAGE_PULL_POLICY":         "Never",
		"BOOTH_APP_RESOURCES":                 `{"limits":{"cpu":"1"}}`,
		"BOOTH_APP_GATE_RESOURCES":            `{"limits":{"cpu":"100m"}}`,
		"BOOTH_APP_TMP_SIZE_LIMIT":            "64Mi",
		"BOOTH_APP_IDLE_TIMEOUT":              "90s",
		"BOOTH_APP_MAX_WEBSOCKET":             "1h",
		"BOOTH_APP_MAX_RUNNING":               "5",
		"BOOTH_APP_MAX_RUNNING_PER_WORKSPACE": "2",
	}))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":9090" || cfg.NATSURL != "nats://nats:4222" || cfg.NATSCredsFile != "/creds" || cfg.WebDir != "/web" || cfg.GroupsClaim != "memberships" || cfg.MaxSourceBytes != 4096 {
		t.Errorf("cfg = %+v", cfg)
	}
	want := Lifecycle{
		Namespace: "booth-streamlit", SelfDeployment: "booth-streamlit", RuntimeImage: "runtime:1", GateImage: "backend:1",
		PullPolicy: "Never", ServiceAccount: "booth-streamlit-app", AppResources: `{"limits":{"cpu":"1"}}`, GateResources: `{"limits":{"cpu":"100m"}}`,
		TmpSizeLimit: "64Mi", IdleTimeout: 90 * time.Second, MaxWebsocket: time.Hour, MaxRunning: 5, MaxPerWS: 2,
	}
	if cfg.Lifecycle != want {
		t.Errorf("lifecycle = %+v\nwant %+v", cfg.Lifecycle, want)
	}
}

func TestLoad_InvalidValues(t *testing.T) {
	for name, bad := range map[string][]string{
		"BOOTH_STREAMLIT_MAX_SOURCE_BYTES":    {"0", "-1", "lots"},
		"BOOTH_APP_IDLE_TIMEOUT":              {"soon", "-5m"},
		"BOOTH_APP_MAX_WEBSOCKET":             {"forever"},
		"BOOTH_APP_MAX_RUNNING":               {"-1", "many"},
		"BOOTH_APP_MAX_RUNNING_PER_WORKSPACE": {"x"},
	} {
		for _, v := range bad {
			setEnv(t, with(map[string]string{name: v}))
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s=%q: err = %v", name, v, err)
			}
		}
	}
}

// 0 turns idle shutdown off, which an operator may want.
func TestLoad_ZeroIdleTimeoutDisablesIdleShutdown(t *testing.T) {
	setEnv(t, with(map[string]string{"BOOTH_APP_IDLE_TIMEOUT": "0s"}))
	cfg, err := Load()
	if err != nil || cfg.Lifecycle.IdleTimeout != 0 {
		t.Fatalf("%v %v", cfg.Lifecycle.IdleTimeout, err)
	}
}
