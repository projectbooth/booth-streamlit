// Package contract validates booth-streamlit's own manifest (the BoothModule custom resource its
// Helm chart templates) against contracts/module-manifest.md, and checks the chart's wiring and
// security posture. Per contracts/testing-strategy.md this runs against a rendered template
// (`helm template`), not a deployed cluster: no cluster needed, but a `helm` binary is, which
// ci.yml installs. Locally, a missing helm skips these tests; in CI it fails them.
package contract

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type boothModule struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Spec       struct {
		ID                string   `yaml:"id"`
		DisplayName       string   `yaml:"displayName"`
		Icon              string   `yaml:"icon"`
		Version           string   `yaml:"version"`
		ContractVersion   string   `yaml:"contractVersion"`
		HasOwnUI          bool     `yaml:"hasOwnUi"`
		UIIntegrationMode string   `yaml:"uiIntegrationMode"`
		HealthCheckPath   string   `yaml:"healthCheckPath"`
		RequiredScopes    []string `yaml:"requiredScopes"`
		NavGroup          string   `yaml:"navGroup"`
		NavPath           string   `yaml:"navPath"`
		AdminNavPath      string   `yaml:"adminNavPath"`
		Database          *struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"database"`
		Events *struct {
			Publish   []string `yaml:"publish"`
			Subscribe []string `yaml:"subscribe"`
		} `yaml:"events"`
		ServiceRef struct {
			Name string `yaml:"name"`
			Port int    `yaml:"port"`
		} `yaml:"serviceRef"`
	} `yaml:"spec"`
}

func helm(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is not installed, but CI must run the contract tests")
		}
		t.Skip("helm not installed; these contract tests run in CI, where it is")
	}
	return exec.Command("helm", args...).CombinedOutput()
}

func chartDir() string { return filepath.Join("..", "..", "charts", "booth-streamlit") }

func helmTemplate(t *testing.T, showOnly string, extra ...string) []byte {
	t.Helper()
	args := []string{"template", "booth-streamlit", chartDir(), "--namespace", "booth-streamlit"}
	args = append(args, extra...)
	if showOnly != "" {
		args = append(args, "--show-only", showOnly)
	}
	out, err := helm(t, args...)
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return out
}

func renderBoothModule(t *testing.T, extra ...string) boothModule {
	t.Helper()
	var m boothModule
	out := helmTemplate(t, "templates/boothmodule.yaml", extra...)
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatalf("parsing rendered BoothModule: %v\n%s", err, out)
	}
	return m
}

// TestManifest_RequiredFields checks every field contracts/module-manifest.md marks required.
func TestManifest_RequiredFields(t *testing.T) {
	m := renderBoothModule(t)
	if m.APIVersion != "booth.projectbooth.io/v1alpha1" || m.Kind != "BoothModule" {
		t.Errorf("apiVersion/kind = %s/%s, want booth.projectbooth.io/v1alpha1 BoothModule (ADR 0019)", m.APIVersion, m.Kind)
	}
	// The manifest contract: id "matches the repo name minus booth-".
	if m.Spec.ID != "streamlit" {
		t.Errorf("spec.id = %q, want streamlit", m.Spec.ID)
	}
	if m.Spec.DisplayName == "" {
		t.Error("spec.displayName is required but empty")
	}
	semver := regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	if !semver.MatchString(m.Spec.Version) || !semver.MatchString(m.Spec.ContractVersion) {
		t.Errorf("version=%q contractVersion=%q, want semver", m.Spec.Version, m.Spec.ContractVersion)
	}
	if m.Spec.HealthCheckPath == "" || m.Spec.HealthCheckPath[0] != '/' {
		t.Errorf("spec.healthCheckPath = %q, want a URL path", m.Spec.HealthCheckPath)
	}
	if m.Spec.ServiceRef.Name != "booth-streamlit" || m.Spec.ServiceRef.Port != 8080 {
		t.Errorf("spec.serviceRef = %+v, want the chart's own Service (booth-streamlit:8080)", m.Spec.ServiceRef)
	}
}

// TestManifest_UI checks the "required if hasOwnUi" rules and the placement ADRs 0005, 0016 and
// 0017 and the brief call for.
func TestManifest_UI(t *testing.T) {
	m := renderBoothModule(t)
	if !m.Spec.HasOwnUI {
		t.Fatal("spec.hasOwnUi = false, want true")
	}
	if m.Spec.UIIntegrationMode != "iframe-proxy" {
		t.Errorf("spec.uiIntegrationMode = %q, want iframe-proxy (ADR 0016: a Streamlit app is its own full web app)", m.Spec.UIIntegrationMode)
	}
	if m.Spec.NavGroup != "build" {
		t.Errorf("spec.navGroup = %q, want build (agent brief; ADR 0017)", m.Spec.NavGroup)
	}
	if m.Spec.NavPath != "/streamlit" {
		t.Errorf("spec.navPath = %q, want /streamlit", m.Spec.NavPath)
	}
	if m.Spec.AdminNavPath != "" {
		t.Errorf("spec.adminNavPath = %q; this module has no admin-only view", m.Spec.AdminNavPath)
	}
}

// ADR 0050: a module that omits `events` gets no bus credential and cannot publish at all. The
// brief requires exactly `events: {publish: ["dashboard.*"]}`; least privilege means no
// subscriptions.
func TestManifest_DeclaresItsEventBusUsage(t *testing.T) {
	m := renderBoothModule(t)
	if m.Spec.Events == nil {
		t.Fatal("spec.events is missing: without it booth-core mints no bus credential and no dashboard event can be published (ADR 0050)")
	}
	if want := []string{"dashboard.*"}; !reflect.DeepEqual(m.Spec.Events.Publish, want) {
		t.Errorf("spec.events.publish = %v, want exactly %v", m.Spec.Events.Publish, want)
	}
	if len(m.Spec.Events.Subscribe) != 0 {
		t.Errorf("spec.events.subscribe = %v; this module consumes no events and must not ask to", m.Spec.Events.Subscribe)
	}
	// booth-core's CRD validation pattern for event types.
	pat := regexp.MustCompile(`^[a-z][a-z0-9]*(\.([a-z][a-z0-9]*|\*))+$`)
	for _, s := range m.Spec.Events.Publish {
		if !pat.MatchString(s) {
			t.Errorf("%q would be rejected by booth-core's event-pattern validation", s)
		}
	}
}

// ADR 0053: core provisions the module's database only when the manifest asks for it.
func TestManifest_AsksCoreForADatabase(t *testing.T) {
	m := renderBoothModule(t)
	if m.Spec.Database == nil || !m.Spec.Database.Enabled {
		t.Fatal("spec.database.enabled is not true: booth-core would provision no database and the pod would never start")
	}
	dep := string(helmTemplate(t, "templates/deployment.yaml"))
	if !regexp.MustCompile(`BOOTH_POSTGRES_DSN\s+valueFrom:\s+secretKeyRef:\s+name: booth-database-credentials\s+key: dsn`).MatchString(dep) {
		t.Errorf("the DSN should come from core's booth-database-credentials Secret:\n%s", dep)
	}
}

// With the operator's own database, nothing is asked of core, and a missing Secret name fails the
// render with an actionable message instead of a pod that never starts.
func TestChart_OwnDatabase(t *testing.T) {
	m := renderBoothModule(t, "--set", "postgres.provisionedByCore=false", "--set", "postgres.dsnSecret.name=my-db")
	if m.Spec.Database != nil {
		t.Error("spec.database rendered although the operator supplies the database")
	}
	dep := string(helmTemplate(t, "templates/deployment.yaml", "--set", "postgres.provisionedByCore=false", "--set", "postgres.dsnSecret.name=my-db"))
	if !strings.Contains(dep, "name: my-db") {
		t.Errorf("the operator's Secret is not used:\n%s", dep)
	}

	out, err := helm(t, "template", "x", chartDir(), "--set", "postgres.provisionedByCore=false")
	if err == nil {
		t.Fatalf("rendered with no database at all:\n%s", out)
	}
	if !bytes.Contains(out, []byte("postgres.dsnSecret.name is required")) {
		t.Errorf("failure message not actionable:\n%s", out)
	}
}

// Every spec field this chart renders must exist in booth-core's real BoothModule CRD (vendored
// unchanged from booth-core's chart into test/integration/fixtures). The API server silently
// prunes unknown fields, so a misspelled or not-yet-supported field would vanish without error,
// which is how a module ends up with no event-bus credential and no clue why.
func TestManifest_EveryFieldIsInCoresCRD(t *testing.T) {
	crdBytes, err := os.ReadFile(filepath.Join("..", "integration", "fixtures", "boothmodule-crd.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Name   string `yaml:"name"`
				Schema struct {
					OpenAPIV3Schema struct {
						Properties struct {
							Spec struct {
								Properties map[string]any `yaml:"properties"`
								Required   []string       `yaml:"required"`
							} `yaml:"spec"`
						} `yaml:"properties"`
					} `yaml:"openAPIV3Schema"`
				} `yaml:"schema"`
			} `yaml:"versions"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(crdBytes, &crd); err != nil {
		t.Fatal(err)
	}
	var known map[string]any
	var required []string
	for _, v := range crd.Spec.Versions {
		if v.Name == "v1alpha1" {
			known = v.Schema.OpenAPIV3Schema.Properties.Spec.Properties
			required = v.Schema.OpenAPIV3Schema.Properties.Spec.Required
		}
	}
	if len(known) == 0 {
		t.Fatal("no v1alpha1 spec properties found in the vendored CRD")
	}

	var rendered struct {
		Spec map[string]any `yaml:"spec"`
	}
	if err := yaml.Unmarshal(helmTemplate(t, "templates/boothmodule.yaml"), &rendered); err != nil {
		t.Fatal(err)
	}
	var fields []string
	for f := range rendered.Spec {
		fields = append(fields, f)
		if _, ok := known[f]; !ok {
			t.Errorf("spec.%s is not in booth-core's BoothModule CRD and would be pruned on apply", f)
		}
	}
	sort.Strings(fields)
	for _, f := range required {
		if _, ok := rendered.Spec[f]; !ok {
			t.Errorf("booth-core's CRD requires spec.%s, which the chart does not render (rendered: %v)", f, fields)
		}
	}
}

// The path core polls and the readiness probe hit the same endpoint; liveness deliberately doesn't.
func TestManifest_HealthPathMatchesProbe(t *testing.T) {
	m := renderBoothModule(t)
	dep := helmTemplate(t, "templates/deployment.yaml")
	if !regexp.MustCompile(`readinessProbe:\s+httpGet:\s+path: ` + regexp.QuoteMeta(m.Spec.HealthCheckPath) + `\b`).Match(dep) {
		t.Errorf("readinessProbe does not use the manifest's healthCheckPath %q:\n%s", m.Spec.HealthCheckPath, dep)
	}
	if !regexp.MustCompile(`livenessProbe:\s+httpGet:\s+path: /livez\b`).Match(dep) {
		t.Error("livenessProbe should use /livez: restarting the pod can't fix a database outage")
	}
}

// The credential core writes for the declared events reaches the pod: mounted as a directory (so
// in-place renewal propagates), not optional, and the bus address comes from the same Secret.
func TestChart_MountsTheEventBusCredential(t *testing.T) {
	dep := string(helmTemplate(t, "templates/deployment.yaml"))
	for _, want := range []string{
		"BOOTH_NATS_CREDS_FILE",
		"/etc/booth/event-bus/nats.creds",
		"secretName: booth-event-bus-credentials",
		"mountPath: /etc/booth/event-bus",
		"readOnly: true",
	} {
		if !strings.Contains(dep, want) {
			t.Errorf("deployment lacks %q:\n%s", want, dep)
		}
	}
	if !regexp.MustCompile(`BOOTH_NATS_URL\s+valueFrom:\s+secretKeyRef:\s+name: booth-event-bus-credentials\s+key: url`).MatchString(dep) {
		t.Errorf("the bus URL should come from the credentials Secret's url key:\n%s", dep)
	}
	if strings.Contains(dep, "subPath:") {
		t.Error("a subPath mount is never updated by the kubelet: a renewed credential would not arrive")
	}
	if strings.Contains(dep, "optional: true") {
		t.Error("the credentials must not be optional: the pod should wait for them, not run without")
	}
}

// The event bus is either wired or explicitly off; there is no state where a forgotten value
// quietly leaves the module unable to publish.
func TestChart_EventBusIsWiredOrExplicitlyOff(t *testing.T) {
	off := []string{"--set", "eventBus.enabled=false"}
	if bytes.Contains(helmTemplate(t, "templates/boothmodule.yaml", off...), []byte("events:")) {
		t.Error("events declared with the event bus off")
	}
	dep := helmTemplate(t, "templates/deployment.yaml", off...)
	for _, unwanted := range []string{"BOOTH_NATS", "event-bus-credentials", "volumes:"} {
		if bytes.Contains(dep, []byte(unwanted)) {
			t.Errorf("%q rendered with the event bus off:\n%s", unwanted, dep)
		}
	}

	out, err := helm(t, "template", "x", chartDir(), "--set", "eventBus.credentialsSecret.enabled=false")
	if err == nil {
		t.Fatalf("rendered with no credentials and no bus address:\n%s", out)
	}
	if !bytes.Contains(out, []byte("nats.url is required")) {
		t.Errorf("failure message not actionable:\n%s", out)
	}

	stand := helmTemplate(t, "templates/deployment.yaml", "--set", "eventBus.credentialsSecret.enabled=false", "--set", "nats.url=nats://nats:4222")
	if bytes.Contains(stand, []byte("BOOTH_NATS_CREDS_FILE")) || bytes.Contains(stand, []byte("event-bus-credentials")) {
		t.Errorf("credential wiring rendered with the credentials Secret disabled:\n%s", stand)
	}
	if !bytes.Contains(stand, []byte(`"nats://nats:4222"`)) {
		t.Errorf("stand-in bus URL not rendered:\n%s", stand)
	}
}

// Every app request is authenticated against core's iframe-identity issuer (ADR 0069). The default
// must be spelled exactly as core's chart publishes it, and an empty value must refuse to render.
func TestChart_IframeIdentityIssuer(t *testing.T) {
	dep := string(helmTemplate(t, "templates/deployment.yaml"))
	if !regexp.MustCompile(`BOOTH_IFRAME_IDENTITY_ISSUER_URL\s+value: "http://booth-core\.booth-system\.svc\.cluster\.local:8080/iframe-identity"`).MatchString(dep) {
		t.Errorf("default issuer is not core's exact spelling:\n%s", dep)
	}
	if !regexp.MustCompile(`BOOTH_OIDC_GROUPS_CLAIM\s+value: "groups"`).MatchString(dep) {
		t.Error("default groups claim should be \"groups\", matching booth-core")
	}
	out, err := helm(t, "template", "x", chartDir(), "--set", "identity.issuerUrl=")
	if err == nil {
		t.Fatalf("rendered with no identity issuer:\n%s", out)
	}
	if !bytes.Contains(out, []byte("identity.issuerUrl is required")) {
		t.Errorf("failure message not actionable:\n%s", out)
	}
}
