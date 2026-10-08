package contract

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var dataOn = []string{"--set", "dataAccess.enabled=true"}
var dataAndDB = []string{"--set", "dataAccess.enabled=true", "--set", "dataAccess.database.enabled=true"}

// Off by default: no minting, no internal port, no minting Secret, no extra policy.
func TestChart_DataAccessIsOffByDefault(t *testing.T) {
	if bytes.Contains(helmTemplate(t, "templates/boothmodule.yaml"), []byte("workloadIdentity")) {
		t.Error("the manifest asks to mint with data access off")
	}
	all := string(helmTemplate(t, ""))
	for _, s := range []string{"booth-workload-minting-credentials", "8081", "BOOTH_DATA_ACCESS"} {
		if strings.Contains(all, s) {
			t.Errorf("%q rendered with data access off", s)
		}
	}
}

// ADR 0056/0058, ADR 0107: with data access on the manifest declares workloadIdentity.mint (and core's
// real CRD keeps the field), the backend reads core's minting Secret, and the sidecar is digest-pinned.
func TestChart_DataAccessManifestAndBackend(t *testing.T) {
	var m struct {
		Spec map[string]any `yaml:"spec"`
	}
	if err := yaml.Unmarshal(helmTemplate(t, "templates/boothmodule.yaml", dataOn...), &m); err != nil {
		t.Fatal(err)
	}
	wi, _ := m.Spec["workloadIdentity"].(map[string]any)
	if wi["mint"] != true {
		t.Fatalf("spec.workloadIdentity = %v, want {mint: true}", m.Spec["workloadIdentity"])
	}
	// The CRD-field check, with data access on: workloadIdentity must not be pruned.
	checkFieldsInCoresCRD(t, dataOn...)

	dep := string(helmTemplate(t, "templates/deployment.yaml", dataOn...))
	for _, re := range []string{
		`BOOTH_WORKLOAD_MINT_URL\s+valueFrom:\s+secretKeyRef: \{name: booth-workload-minting-credentials, key: url\}`,
		`BOOTH_WORKLOAD_MINT_CREDENTIAL\s+valueFrom:\s+secretKeyRef: \{name: booth-workload-minting-credentials, key: credential\}`,
		`BOOTH_INTERNAL_URL\s+value: "http://booth-streamlit\.booth-streamlit\.svc:8081"`,
		`BOOTH_APP_SIDECAR_IMAGE\s+value: "ghcr\.io/projectbooth/credential-sidecar@sha256:[0-9a-f]{64}"`,
		`name: internal\s+containerPort: 8081`,
	} {
		if !regexp.MustCompile(re).MatchString(dep) {
			t.Errorf("deployment lacks %s", re)
		}
	}
	if !strings.Contains(string(helmTemplate(t, "templates/service.yaml", dataOn...)), "port: 8081") {
		t.Error("service has no internal port")
	}
	// The Role is the same with data access on: no Secret read or update is needed (ADR 0107 item 1).
	TestChart_BackendRoleIsExactlyTheDesignNotes(t)
}

// The internal port admits app pods only; app pods may reach it, and booth-database only when enabled.
func TestChart_DataAccessNetworkPolicies(t *testing.T) {
	pols := docs(t, "NetworkPolicy", dataOn...)
	if len(pols) != 2 {
		t.Fatalf("want the app policy and the backend policy, got %d", len(pols))
	}
	var backend, app map[string]any
	for _, p := range pols {
		if strings.HasSuffix(p["metadata"].(map[string]any)["name"].(string), "-backend") {
			backend = p["spec"].(map[string]any)
		} else {
			app = p["spec"].(map[string]any)
		}
	}
	ing := backend["ingress"].([]any)
	if len(ing) != 2 {
		t.Fatalf("backend ingress %v", ing)
	}
	internal := ing[1].(map[string]any)
	from := internal["from"].([]any)[0].(map[string]any)["podSelector"].(map[string]any)["matchLabels"].(map[string]any)
	if from["booth.projectbooth.io/component"] != "app" || internal["ports"].([]any)[0].(map[string]any)["port"] != 8081 {
		t.Errorf("internal port admits %v", internal)
	}
	if _, restricted := ing[0].(map[string]any)["from"]; restricted {
		t.Error("the module port (8080) must stay reachable from booth-core's gateway")
	}

	egress := yamlString(t, app["egress"])
	if !strings.Contains(egress, "port: 8081") {
		t.Error("app pods can't reach the backend's internal port")
	}
	if strings.Contains(egress, "booth-database") {
		t.Error("booth-database egress rendered without dataAccess.database.enabled")
	}
	withDB := string(helmTemplate(t, "templates/networkpolicy.yaml", dataAndDB...))
	if !strings.Contains(withDB, "kubernetes.io/metadata.name: booth-database") || !strings.Contains(withDB, "port: 5432") {
		t.Errorf("no booth-database egress with it enabled:\n%s", withDB)
	}
	if strings.Contains(withDB, "booth-system") {
		t.Error("app pods must have no route to booth-core (broker calls go through the backend)")
	}
}

func yamlString(t *testing.T, v any) string {
	t.Helper()
	b, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
