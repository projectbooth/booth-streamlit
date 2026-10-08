package config

import (
	"strings"
	"testing"
)

// setEnv clears every variable Load reads, then sets the given ones, so a developer's own
// environment can't leak into a case.
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range []string{"BOOTH_HTTP_ADDR", "BOOTH_POSTGRES_DSN", "BOOTH_NATS_URL", "BOOTH_NATS_CREDS_FILE", "BOOTH_WEB_DIR", "BOOTH_IFRAME_IDENTITY_ISSUER_URL", "BOOTH_OIDC_GROUPS_CLAIM", "BOOTH_STREAMLIT_STATIC_APPS"} {
		t.Setenv(k, "")
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestLoad_Defaults(t *testing.T) {
	setEnv(t, map[string]string{"BOOTH_POSTGRES_DSN": "postgres://x", "BOOTH_IFRAME_IDENTITY_ISSUER_URL": "http://core/iframe-identity"})
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
	}
	if cfg.GroupsClaim != "groups" {
		t.Errorf("GroupsClaim = %q, want groups (booth-core's default)", cfg.GroupsClaim)
	}
	if cfg.NATSURL != "" || cfg.NATSCredsFile != "" || cfg.WebDir != "" || cfg.StaticApps != "" {
		t.Errorf("unexpected non-empty optional values: %+v", cfg)
	}
}

func TestLoad_RequiresDatabase(t *testing.T) {
	setEnv(t, nil)
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "BOOTH_POSTGRES_DSN") {
		t.Fatalf("err = %v, want one naming BOOTH_POSTGRES_DSN", err)
	}
}

func TestLoad_RequiresTheIframeIssuer(t *testing.T) {
	setEnv(t, map[string]string{"BOOTH_POSTGRES_DSN": "postgres://x"})
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "BOOTH_IFRAME_IDENTITY_ISSUER_URL") {
		t.Fatalf("err = %v, want one naming BOOTH_IFRAME_IDENTITY_ISSUER_URL", err)
	}
}

func TestLoad_CredentialsWithoutURLIsRefused(t *testing.T) {
	setEnv(t, map[string]string{"BOOTH_POSTGRES_DSN": "postgres://x", "BOOTH_IFRAME_IDENTITY_ISSUER_URL": "http://core/iframe-identity", "BOOTH_NATS_CREDS_FILE": "/etc/booth/event-bus/nats.creds"})
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "BOOTH_NATS_URL") {
		t.Fatalf("err = %v, want one naming BOOTH_NATS_URL", err)
	}
}

func TestLoad_ReadsEverything(t *testing.T) {
	setEnv(t, map[string]string{
		"BOOTH_HTTP_ADDR":       ":9090",
		"BOOTH_POSTGRES_DSN":    "postgres://x",
		"BOOTH_NATS_URL":        "nats://nats:4222",
		"BOOTH_NATS_CREDS_FILE": "/creds",
		"BOOTH_WEB_DIR":         "/web",

		"BOOTH_IFRAME_IDENTITY_ISSUER_URL": "http://core/iframe-identity",
		"BOOTH_OIDC_GROUPS_CLAIM":          "memberships",
		"BOOTH_STREAMLIT_STATIC_APPS":      "{}",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := Config{HTTPAddr: ":9090", PostgresDSN: "postgres://x", NATSURL: "nats://nats:4222", NATSCredsFile: "/creds", WebDir: "/web",
		IframeIssuerURL: "http://core/iframe-identity", GroupsClaim: "memberships", StaticApps: "{}"}
	if cfg != want {
		t.Errorf("cfg = %+v, want %+v", cfg, want)
	}
}
