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
	"BOOTH_DATA_ACCESS", "BOOTH_WORKLOAD_MINT_URL", "BOOTH_WORKLOAD_MINT_CREDENTIAL", "BOOTH_CORE_URL", "BOOTH_INTERNAL_ADDR",
	"BOOTH_INTERNAL_URL", "BOOTH_APP_SIDECAR_IMAGE", "BOOTH_APP_SIDECAR_RESOURCES", "BOOTH_APP_DATABASE", "BOOTH_DATA_REFRESH_MAX",
	"BOOTH_FILES_MAX_OBJECT_BYTES", "BOOTH_FILES_OBJECT_TIMEOUT", "BOOTH_FILES_META_TIMEOUT", "BOOTH_FILES_MAX_CONCURRENT",
	"BOOTH_APP_LAKEHOUSE", "BOOTH_APP_PIP_INDEX_URL", "BOOTH_APP_PIP_TIMEOUT", "BOOTH_APP_PIP_SITE_SIZE_LIMIT", "BOOTH_APP_PIP_EGRESS_CLOSED",
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
		"BOOTH_APP_PIP_INDEX_URL":             "http://pypi.test/simple",
		"BOOTH_APP_PIP_TIMEOUT":               "90s",
		"BOOTH_APP_PIP_SITE_SIZE_LIMIT":       "512Mi",
		"BOOTH_APP_PIP_EGRESS_CLOSED":         "true",
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
		PipIndexURL: "http://pypi.test/simple", PipTimeout: 90 * time.Second, PipSiteSizeLimit: "512Mi", PipEgressClosed: true,
	}
	if cfg.Lifecycle != want {
		t.Errorf("lifecycle = %+v\nwant %+v", cfg.Lifecycle, want)
	}
}

func TestLoad_InvalidValues(t *testing.T) {
	for name, bad := range map[string][]string{
		"BOOTH_APP_PIP_TIMEOUT":               {"0s", "-1m", "soon"},
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

func dataEnv() map[string]string {
	return with(map[string]string{
		"BOOTH_DATA_ACCESS":              "true",
		"BOOTH_WORKLOAD_MINT_URL":        "http://core/api/internal/workload-tokens",
		"BOOTH_WORKLOAD_MINT_CREDENTIAL": "cred",
		"BOOTH_CORE_URL":                 "http://core:8080",
		"BOOTH_INTERNAL_URL":             "http://booth-streamlit.ns.svc:8081",
		"BOOTH_APP_SIDECAR_IMAGE":        "ghcr.io/projectbooth/credential-sidecar@sha256:6a0a",
	})
}

func TestLoad_DataAccess(t *testing.T) {
	setEnv(t, minimal())
	if cfg, err := Load(); err != nil || cfg.Data.Enabled {
		t.Fatalf("data access must be off unless asked for: %+v %v", cfg.Data, err)
	}

	env := dataEnv()
	env["BOOTH_APP_DATABASE"] = "true"
	env["BOOTH_DATA_REFRESH_MAX"] = "20s"
	setEnv(t, env)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Data
	if !d.Enabled || !d.Database || d.InternalAddr != ":8081" || d.RefreshMax != 20*time.Second || d.MintCredential != "cred" {
		t.Errorf("data config %+v", d)
	}

	for _, name := range []string{"BOOTH_WORKLOAD_MINT_URL", "BOOTH_WORKLOAD_MINT_CREDENTIAL", "BOOTH_CORE_URL", "BOOTH_INTERNAL_URL", "BOOTH_APP_SIDECAR_IMAGE"} {
		env := dataEnv()
		delete(env, name)
		setEnv(t, env)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("without %s: %v", name, err)
		}
	}
	env = dataEnv()
	env["BOOTH_APP_SIDECAR_IMAGE"] = "ghcr.io/projectbooth/credential-sidecar:latest"
	setEnv(t, env)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "digest-pinned") {
		t.Errorf("a tag-pinned sidecar image was accepted: %v", err)
	}
}

func TestLoad_FileProxyLimits(t *testing.T) {
	setEnv(t, dataEnv())
	cfg, err := Load()
	if err != nil || cfg.Data.FilesMaxObjectBytes != 0 || cfg.Data.FilesMaxConcurrent != 0 {
		t.Fatalf("defaults must be zero (the package defaults apply): %+v %v", cfg.Data, err)
	}
	env := dataEnv()
	env["BOOTH_FILES_MAX_OBJECT_BYTES"] = "1048576"
	env["BOOTH_FILES_OBJECT_TIMEOUT"] = "1m"
	env["BOOTH_FILES_META_TIMEOUT"] = "5s"
	env["BOOTH_FILES_MAX_CONCURRENT"] = "2"
	setEnv(t, env)
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Data
	if d.FilesMaxObjectBytes != 1<<20 || d.FilesObjectTimeout != time.Minute || d.FilesMetaTimeout != 5*time.Second || d.FilesMaxConcurrent != 2 {
		t.Errorf("file proxy limits %+v", d)
	}
	for name, bad := range map[string]string{
		"BOOTH_FILES_MAX_OBJECT_BYTES": "0", "BOOTH_FILES_OBJECT_TIMEOUT": "soon",
		"BOOTH_FILES_META_TIMEOUT": "-1s", "BOOTH_FILES_MAX_CONCURRENT": "many",
	} {
		env := dataEnv()
		env[name] = bad
		setEnv(t, env)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=%q: %v", name, bad, err)
		}
	}
}
