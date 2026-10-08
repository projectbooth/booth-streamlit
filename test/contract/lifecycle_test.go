package contract

import (
	"bytes"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// docs renders the whole chart and returns its documents of one kind.
func docs(t *testing.T, kind string, extra ...string) []map[string]any {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(helmTemplate(t, "", extra...)))
	var out []map[string]any
	for {
		var d map[string]any
		if err := dec.Decode(&d); err != nil {
			break
		}
		if d != nil && d["kind"] == kind {
			out = append(out, d)
		}
	}
	return out
}

type rule struct {
	group, resource string
	verbs           string // sorted, comma-joined
}

// The backend's Kubernetes API access is exactly docs/design-v0.md (a) "Backend RBAC", rule for
// rule, namespace-scoped, bound to the backend's account only. Any widening has to change this
// test. Notably: Secrets are create/delete only (never get/list/watch/update), so the backend can't
// read core's credentials in its namespace, and nothing is cluster-scoped.
func TestChart_BackendRoleIsExactlyTheDesignNotes(t *testing.T) {
	for _, kind := range []string{"ClusterRole", "ClusterRoleBinding"} {
		if n := len(docs(t, kind)); n != 0 {
			t.Errorf("chart renders %d %s; the backend's access must be namespace-scoped", n, kind)
		}
	}
	roles := docs(t, "Role")
	if len(roles) != 1 {
		t.Fatalf("want exactly one Role, got %d", len(roles))
	}
	var got []rule
	for _, r := range roles[0]["rules"].([]any) {
		m := r.(map[string]any)
		var verbs []string
		for _, v := range m["verbs"].([]any) {
			verbs = append(verbs, v.(string))
		}
		sort.Strings(verbs)
		for _, g := range m["apiGroups"].([]any) {
			for _, res := range m["resources"].([]any) {
				got = append(got, rule{g.(string), res.(string), strings.Join(verbs, ",")})
			}
		}
		if _, has := m["resourceNames"]; has {
			t.Error("unexpected resourceNames")
		}
	}
	sort.Slice(got, func(i, j int) bool { return got[i].group+"/"+got[i].resource < got[j].group+"/"+got[j].resource })
	crud := "create,delete,get,list,update,watch"
	want := []rule{
		{"", "configmaps", crud},
		{"", "pods", "get,list,watch"},
		{"", "secrets", "create,delete"},
		{"", "services", crud},
		{"apps", "deployments", crud},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Role rules:\n got %v\nwant %v", got, want)
	}

	bindings := docs(t, "RoleBinding")
	if len(bindings) != 1 {
		t.Fatalf("want exactly one RoleBinding, got %d", len(bindings))
	}
	b := bindings[0]
	if ref := b["roleRef"].(map[string]any); ref["kind"] != "Role" || ref["name"] != roles[0]["metadata"].(map[string]any)["name"] {
		t.Errorf("RoleBinding binds %v", ref)
	}
	subjects := b["subjects"].([]any)
	if len(subjects) != 1 || subjects[0].(map[string]any)["name"] != "booth-streamlit" || subjects[0].(map[string]any)["kind"] != "ServiceAccount" {
		t.Errorf("RoleBinding subjects %v; only the backend's own account", subjects)
	}

	// App pods' account: no token, and bound to nothing (checked above: the only binding is the
	// backend's).
	var appSA map[string]any
	for _, sa := range docs(t, "ServiceAccount") {
		if sa["metadata"].(map[string]any)["name"] == "booth-streamlit-app" {
			appSA = sa
		}
	}
	if appSA == nil || appSA["automountServiceAccountToken"] != false {
		t.Errorf("app service account missing or mounts a token: %v", appSA)
	}

	dep := string(helmTemplate(t, "templates/deployment.yaml"))
	if !strings.Contains(dep, "automountServiceAccountToken: true") {
		t.Error("the backend needs its token to manage app Deployments")
	}
	if !regexp.MustCompile(`BOOTH_APP_SERVICE_ACCOUNT\s+value: "booth-streamlit-app"`).MatchString(dep) {
		t.Error("app pods are not pointed at the token-less app service account")
	}
}

// ADR 0104 item 4 and design note (b): app pods accept traffic only from the backend on the gate's
// port; egress is DNS plus (mode open) the internet minus every private, CGNAT and link-local range,
// which includes the cloud metadata address; mode closed is DNS only.
func TestChart_AppNetworkPolicy(t *testing.T) {
	pols := docs(t, "NetworkPolicy")
	if len(pols) != 1 {
		t.Fatalf("want one NetworkPolicy, got %d", len(pols))
	}
	spec := pols[0]["spec"].(map[string]any)
	if sel := spec["podSelector"].(map[string]any)["matchLabels"].(map[string]any); sel["booth.projectbooth.io/component"] != "app" {
		t.Errorf("policy selects %v, want the app pods", sel)
	}
	if types := spec["policyTypes"].([]any); len(types) != 2 {
		t.Errorf("policyTypes %v, want Ingress and Egress", types)
	}
	ing := spec["ingress"].([]any)
	if len(ing) != 1 {
		t.Fatalf("ingress rules %v", ing)
	}
	from := ing[0].(map[string]any)["from"].([]any)[0].(map[string]any)
	if _, ns := from["namespaceSelector"]; ns || from["podSelector"].(map[string]any)["matchLabels"].(map[string]any)["app.kubernetes.io/name"] != "booth-streamlit" {
		t.Errorf("ingress from %v; only the backend in this namespace", from)
	}
	if port := ing[0].(map[string]any)["ports"].([]any)[0].(map[string]any)["port"]; port != 8080 {
		t.Errorf("ingress port %v, want the gate's 8080", port)
	}

	open := string(helmTemplate(t, "templates/networkpolicy.yaml"))
	for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16"} {
		if !strings.Contains(open, cidr) {
			t.Errorf("open egress does not exclude %s", cidr)
		}
	}
	if !strings.Contains(open, "cidr: 0.0.0.0/0") {
		t.Error("open egress has no internet rule")
	}
	closed := string(helmTemplate(t, "templates/networkpolicy.yaml", "--set", "apps.egress.mode=closed"))
	if strings.Contains(closed, "ipBlock") || !strings.Contains(closed, "port: 53") {
		t.Errorf("closed egress should be DNS only:\n%s", closed)
	}
	out, err := helm(t, "template", "x", chartDir(), "--set", "apps.egress.mode=wide")
	if err == nil || !bytes.Contains(out, []byte("apps.egress.mode must be open or closed")) {
		t.Errorf("an unknown egress mode rendered: %v\n%s", err, out)
	}
}

// The quota backstop is on by default, and every pod the chart renders declares the limits a
// limits quota requires (the app pods' limits come from apps.resources and apps.gateResources).
func TestChart_QuotaAndLimits(t *testing.T) {
	q := docs(t, "ResourceQuota")
	if len(q) != 1 {
		t.Fatalf("want a ResourceQuota by default, got %d", len(q))
	}
	hard := q[0]["spec"].(map[string]any)["hard"].(map[string]any)
	for _, k := range []string{"pods", "limits.cpu", "limits.memory", "limits.ephemeral-storage"} {
		if _, ok := hard[k]; !ok {
			t.Errorf("quota lacks %s", k)
		}
	}
	if len(docs(t, "ResourceQuota", "--set", "quota.enabled=false")) != 0 {
		t.Error("quota.enabled=false still renders a quota")
	}
	dep := string(helmTemplate(t, "templates/deployment.yaml"))
	for _, want := range []string{"cpu: 500m", "memory: 256Mi", "ephemeral-storage: 256Mi"} {
		if !strings.Contains(dep, want) {
			t.Errorf("backend limits lack %q", want)
		}
	}
	for _, env := range []string{"BOOTH_APP_RESOURCES", "BOOTH_APP_GATE_RESOURCES"} {
		m := regexp.MustCompile(env + `\s+value: "(.*)"`).FindStringSubmatch(dep)
		if m == nil || !strings.Contains(m[1], `\"limits\"`) || !strings.Contains(m[1], `ephemeral-storage`) {
			t.Errorf("%s does not carry limits including ephemeral-storage: %v", env, m)
		}
	}
}

// The backend and every pod it would render keep the scaffold's hardening.
func TestChart_PodHardening(t *testing.T) {
	dep := string(helmTemplate(t, "templates/deployment.yaml"))
	for _, want := range []string{
		"runAsNonRoot: true",
		"readOnlyRootFilesystem: true",
		"allowPrivilegeEscalation: false",
		"- ALL",
		"type: RuntimeDefault",
	} {
		if !strings.Contains(dep, want) {
			t.Errorf("deployment lacks %q", want)
		}
	}
}

// The lifecycle's configuration reaches the backend, with the defaults the design note gives.
func TestChart_LifecycleConfiguration(t *testing.T) {
	dep := string(helmTemplate(t, "templates/deployment.yaml"))
	for name, want := range map[string]string{
		"BOOTH_SELF_DEPLOYMENT":               `"booth-streamlit"`,
		"BOOTH_APP_RUNTIME_IMAGE":             `"ghcr.io/projectbooth/booth-streamlit-app-runtime:0.1.0"`,
		"BOOTH_APP_GATE_IMAGE":                `"ghcr.io/projectbooth/booth-streamlit:0.1.0"`,
		"BOOTH_APP_MAX_RUNNING":               `"5"`,
		"BOOTH_APP_MAX_RUNNING_PER_WORKSPACE": `"0"`,
		"BOOTH_APP_IDLE_TIMEOUT":              `"30m"`,
		"BOOTH_APP_MAX_WEBSOCKET":             `"8h"`,
	} {
		if !regexp.MustCompile(name + `\s+value: ` + regexp.QuoteMeta(want)).MatchString(dep) {
			t.Errorf("%s is not %s", name, want)
		}
	}
	if !regexp.MustCompile(`BOOTH_NAMESPACE\s+valueFrom:\s+fieldRef: \{fieldPath: metadata.namespace\}`).MatchString(dep) {
		t.Error("BOOTH_NAMESPACE must come from the pod's own namespace")
	}
}
