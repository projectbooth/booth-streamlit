package lifecycle

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/projectbooth/booth-streamlit/internal/apps"
	"github.com/projectbooth/booth-streamlit/internal/identity"
)

const ns = "booth-streamlit"

var owner = identity.Caller{Subject: "o", Workspace: "acme", Role: identity.RoleOwner}

type rig struct {
	t      *testing.T
	ctx    context.Context
	client *fake.Clientset
	svc    *apps.Service
	c      *Controller
	clock  time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	backend := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "booth-streamlit", Namespace: ns, UID: types.UID("backend-uid")}}
	client := fake.NewSimpleClientset(backend)
	svc := apps.NewService(apps.NewMemoryStore(), 0)
	r := &rig{t: t, ctx: context.Background(), client: client, svc: svc, clock: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	r.c = New(Config{
		Namespace: ns, RuntimeImage: "runtime:1", GateImage: "backend:1", PullPolicy: corev1.PullIfNotPresent,
		ServiceAccount: "booth-streamlit-app", TmpSizeLimit: "64Mi", IdleTimeout: 30 * time.Minute, Owner: backend,
		AppResources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")}},
	}, client, svc)
	r.c.now = func() time.Time { return r.clock }
	return r
}

func (r *rig) reconcile() {
	r.t.Helper()
	if err := r.c.Reconcile(r.ctx); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) deployment(id string) *appsv1.Deployment {
	r.t.Helper()
	d, err := r.client.AppsV1().Deployments(ns).Get(r.ctx, "app-"+id, metav1.GetOptions{})
	if err != nil {
		r.t.Fatal(err)
	}
	return d
}

func (r *rig) create(name string) apps.App {
	r.t.Helper()
	a, err := r.svc.Create(r.ctx, owner, apps.Input{Name: name, Source: "import streamlit as st\nst.write('hi')", Shared: true})
	if err != nil {
		r.t.Fatal(err)
	}
	return a
}

func container(t *testing.T, d *appsv1.Deployment, name string) corev1.Container {
	t.Helper()
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no container %q", name)
	return corev1.Container{}
}

func TestReconcile_CreatesAppObjectsStoppedByDefault(t *testing.T) {
	r := newRig(t)
	a := r.create("Sales")
	r.reconcile()

	d := r.deployment(a.ID)
	if *d.Spec.Replicas != 0 {
		t.Errorf("a new (stopped) app has %d replicas, want 0", *d.Spec.Replicas)
	}
	cm, err := r.client.CoreV1().ConfigMaps(ns).Get(r.ctx, "app-"+a.ID+"-src", metav1.GetOptions{})
	if err != nil || cm.Data["app.py"] != a.Source {
		t.Fatalf("configmap: %v %v", cm, err)
	}
	sec, err := r.client.CoreV1().Secrets(ns).Get(r.ctx, "app-"+a.ID+"-gate", metav1.GetOptions{})
	if err != nil || sec.StringData["bearer"] != a.GateBearer {
		t.Fatalf("secret does not carry the app's bearer: %v", err)
	}
	if _, err := r.client.CoreV1().Services(ns).Get(r.ctx, "app-"+a.ID, metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	if st := r.c.Status(a); st.State != StateStopped {
		t.Errorf("status %+v", st)
	}

	// Ownership: backend owns the Deployment (uninstall removes every app); the Deployment owns the
	// rest (deleting an app removes all of it).
	if or := d.OwnerReferences; len(or) != 1 || or[0].UID != "backend-uid" || or[0].Kind != "Deployment" {
		t.Errorf("deployment owners %+v", or)
	}
	for _, o := range []metav1.Object{cm, sec} {
		if or := o.GetOwnerReferences(); len(or) != 1 || or[0].Name != "app-"+a.ID {
			t.Errorf("%s owners %+v", o.GetName(), or)
		}
	}
	if d.Labels[LabelWorkspace] != "acme" || d.Spec.Template.Labels[LabelWorkspace] != "acme" {
		t.Error("workspace label (ADR 0077) missing")
	}
}

// Design note (a)/(b): the pod's only way in is the gate; Streamlit binds loopback; the bearer is
// mounted into the gate only; no API token, no service-link env, non-root, read-only, limited.
func TestDeployment_PodShape(t *testing.T) {
	r := newRig(t)
	a := r.create("Sales")
	r.reconcile()
	d := r.deployment(a.ID)
	spec := d.Spec.Template.Spec

	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Error("app pods must not mount a service-account token")
	}
	if spec.EnableServiceLinks == nil || *spec.EnableServiceLinks {
		t.Error("app pods must not get service-link env vars")
	}
	if spec.ServiceAccountName != "booth-streamlit-app" {
		t.Errorf("service account %q", spec.ServiceAccountName)
	}
	if d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Error("strategy must be Recreate so an app never briefly runs twice")
	}
	if sc := spec.SecurityContext; sc == nil || !*sc.RunAsNonRoot || *sc.RunAsUser != 65532 {
		t.Errorf("pod security context %+v", sc)
	}

	st := container(t, d, "streamlit")
	env := map[string]string{}
	for _, e := range st.Env {
		env[e.Name] = e.Value
	}
	if env["STREAMLIT_SERVER_ADDRESS"] != "127.0.0.1" || env["STREAMLIT_SERVER_BASE_URL_PATH"] != "apps/"+a.ID {
		t.Errorf("streamlit env %v", env)
	}
	if len(st.Ports) != 0 {
		t.Error("streamlit must expose no port: the gate is the only way in")
	}
	for _, m := range st.VolumeMounts {
		if m.Name == "gate" {
			t.Fatal("the bearer is mounted into the Streamlit container, where app code could read it")
		}
	}
	if st.Resources.Limits.Cpu().String() != "1" || st.Resources.Limits.Memory().String() != "1Gi" {
		t.Errorf("app limits %v", st.Resources.Limits)
	}

	g := container(t, d, "gate")
	if len(g.Command) != 1 || g.Command[0] != "/booth-streamlit-gate" || g.Image != "backend:1" {
		t.Errorf("gate %v %s", g.Command, g.Image)
	}
	if g.ReadinessProbe == nil || g.ReadinessProbe.HTTPGet.Path != "/_booth/gate/healthz" {
		t.Error("readiness must go through the gate's health endpoint")
	}
	for _, c := range []corev1.Container{st, g} {
		sc := c.SecurityContext
		if sc == nil || !*sc.ReadOnlyRootFilesystem || *sc.AllowPrivilegeEscalation || len(sc.Capabilities.Drop) != 1 {
			t.Errorf("%s security context %+v", c.Name, sc)
		}
	}
}

func TestReconcile_StartStopAndCodeChange(t *testing.T) {
	r := newRig(t)
	a := r.create("Sales")
	r.reconcile()
	before := r.deployment(a.ID).Spec.Template.Annotations[annoSourceHash]

	if _, err := r.svc.SetDesiredState(r.ctx, owner, a.ID, apps.Running); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	if *r.deployment(a.ID).Spec.Replicas != 1 {
		t.Fatal("Start did not scale to 1")
	}
	if st := r.c.Status(mustGet(t, r, a.ID)); st.State != StateStarting {
		t.Errorf("before ready: %+v", st)
	}

	// Ready.
	d := r.deployment(a.ID)
	d.Status = appsv1.DeploymentStatus{ReadyReplicas: 1, UpdatedReplicas: 1, ObservedGeneration: d.Generation}
	if _, err := r.client.AppsV1().Deployments(ns).UpdateStatus(r.ctx, d, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	if st := r.c.Status(mustGet(t, r, a.ID)); st.State != StateRunning {
		t.Errorf("after ready: %+v", st)
	}

	// Code change: the ConfigMap and the pod template change, so the pod rolls.
	if _, err := r.svc.Update(r.ctx, owner, a.ID, apps.Input{Name: "Sales", Source: "print(2)", Shared: true}); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	cm, _ := r.client.CoreV1().ConfigMaps(ns).Get(r.ctx, "app-"+a.ID+"-src", metav1.GetOptions{})
	if cm.Data["app.py"] != "print(2)" {
		t.Error("configmap not updated")
	}
	if r.deployment(a.ID).Spec.Template.Annotations[annoSourceHash] == before {
		t.Error("pod template unchanged after a code change: the pod would keep the old code")
	}

	if _, err := r.svc.SetDesiredState(r.ctx, owner, a.ID, apps.Stopped); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	if *r.deployment(a.ID).Spec.Replicas != 0 {
		t.Error("Stop did not scale to 0")
	}
}

func TestReconcile_DeletedAppIsRemoved(t *testing.T) {
	r := newRig(t)
	a := r.create("Sales")
	r.reconcile()
	if err := r.svc.Delete(r.ctx, owner, a.ID); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	if _, err := r.client.AppsV1().Deployments(ns).Get(r.ctx, "app-"+a.ID, metav1.GetOptions{}); err == nil {
		t.Error("deployment of a deleted app still exists")
	}
	if _, err := r.client.CoreV1().ConfigMaps(ns).Get(r.ctx, "app-"+a.ID+"-src", metav1.GetOptions{}); err == nil {
		t.Error("configmap of a deleted app still exists")
	}
	if _, err := r.client.CoreV1().Services(ns).Get(r.ctx, "app-"+a.ID, metav1.GetOptions{}); err == nil {
		t.Error("service of a deleted app still exists")
	}
	// The fake clientset has no garbage collector, so the Secret's owner reference (checked in
	// TestReconcile_CreatesAppObjectsStoppedByDefault) is what removes it in a real cluster; the
	// integration test checks that.
}

func TestReconcile_LeavesOtherObjectsAlone(t *testing.T) {
	r := newRig(t)
	other := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "not-ours", Namespace: ns}}
	if _, err := r.client.CoreV1().ConfigMaps(ns).Create(r.ctx, other, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	if _, err := r.client.CoreV1().ConfigMaps(ns).Get(r.ctx, "not-ours", metav1.GetOptions{}); err != nil {
		t.Error("reconcile deleted an object it doesn't own")
	}
}

func TestIdleShutdown(t *testing.T) {
	r := newRig(t)
	a := r.create("Sales")
	b := r.create("Ops")
	for _, x := range []apps.App{a, b} {
		if _, err := r.svc.SetDesiredState(r.ctx, owner, x.ID, apps.Running); err != nil {
			t.Fatal(err)
		}
	}
	r.reconcile() // both first seen Active now: a full timeout starts

	r.c.WebsocketOpened(b.ID) // b has an open browser tab
	r.clock = r.clock.Add(29 * time.Minute)
	r.c.Touch(a.ID) // a had a request a minute before the timeout
	r.reconcile()
	if mustGet(t, r, a.ID).Suspended || mustGet(t, r, b.ID).Suspended {
		t.Fatal("suspended before the idle timeout")
	}

	r.clock = r.clock.Add(31 * time.Minute)
	r.reconcile()
	if !mustGet(t, r, a.ID).Suspended {
		t.Error("a: no request for 31m and no websocket, not suspended")
	}
	if mustGet(t, r, b.ID).Suspended {
		t.Error("b: an open websocket is activity, but it was suspended")
	}
	r.reconcile()
	if *r.deployment(a.ID).Spec.Replicas != 0 {
		t.Error("a suspended app still has a replica")
	}
	if got := mustGet(t, r, a.ID); got.DesiredState != apps.Running {
		t.Error("idle shutdown changed the owner's choice")
	}
	if st := r.c.Status(mustGet(t, r, a.ID)); st.State != StateSuspended {
		t.Errorf("status %+v", st)
	}

	// Closing b's tab starts its idle clock.
	r.c.WebsocketClosed(b.ID)
	r.clock = r.clock.Add(30 * time.Minute)
	r.reconcile()
	if !mustGet(t, r, b.ID).Suspended {
		t.Error("b: 30m after its last websocket closed, not suspended")
	}
}

func TestStatus_FailedPodReason(t *testing.T) {
	r := newRig(t)
	a := r.create("Sales")
	_, _ = r.svc.SetDesiredState(r.ctx, owner, a.ID, apps.Running)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "app-x", Namespace: ns, Labels: map[string]string{LabelComponent: componentApp, LabelAppID: a.ID}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "streamlit", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		}}},
	}
	if _, err := r.client.CoreV1().Pods(ns).Create(r.ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	st := r.c.Status(mustGet(t, r, a.ID))
	if st.State != StateFailed || !strings.Contains(st.Reason, "CrashLoopBackOff") {
		t.Errorf("status %+v", st)
	}
}

func mustGet(t *testing.T, r *rig, id string) apps.App {
	t.Helper()
	a, err := r.svc.Get(r.ctx, owner, id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// The race the lifecycle integration test found: an app observed "running", then suspended (or
// stopped, or edited) and woken before the next reconcile finished, must not report "running" from
// the observation of the pod that is being scaled away. A 2-second window in a real cluster.
func TestStatus_ChangeInvalidatesTheObservationAtOnce(t *testing.T) {
	r := newRig(t)
	r.svc.OnChange = r.c.OnAppChange
	a := r.create("Sales")
	if _, err := r.svc.SetDesiredState(r.ctx, owner, a.ID, apps.Running); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	d := r.deployment(a.ID)
	d.Status = appsv1.DeploymentStatus{ReadyReplicas: 1, UpdatedReplicas: 1, ObservedGeneration: d.Generation}
	if _, err := r.client.AppsV1().Deployments(ns).UpdateStatus(r.ctx, d, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	if st := r.c.Status(mustGet(t, r, a.ID)); st.State != StateRunning {
		t.Fatalf("precondition: %+v", st)
	}

	// Idle shutdown, then a viewer's wake, with no reconcile in between.
	if err := r.svc.Suspend(r.ctx, "acme", a.ID); err != nil {
		t.Fatal(err)
	}
	woken, err := r.svc.Wake(r.ctx, identity.Caller{Subject: "v", Workspace: "acme", Role: identity.RoleViewer}, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st := r.c.Status(woken); st.State != StateStarting {
		t.Errorf("right after suspend+wake: %+v, want starting (the old pod is going away)", st)
	}

	// An edit rolls the pod: same rule.
	r.reconcile()
	_, _ = r.client.AppsV1().Deployments(ns).UpdateStatus(r.ctx, d, metav1.UpdateOptions{})
	if _, err := r.svc.Update(r.ctx, owner, a.ID, apps.Input{Name: "Sales", Source: "print(3)", Shared: true}); err != nil {
		t.Fatal(err)
	}
	if st := r.c.Status(mustGet(t, r, a.ID)); st.State == StateRunning {
		t.Errorf("right after an edit: %+v, want not running until the new pod is observed", st)
	}
}

// A reconcile that listed the apps before a change must not write its (stale) observation back.
func TestStore_DropsObservationsOlderThanTheLastChange(t *testing.T) {
	r := newRig(t)
	a := r.create("Sales")
	_, _ = r.svc.SetDesiredState(r.ctx, owner, a.ID, apps.Running)

	gens := map[string]uint64{} // what a reconcile would have snapshotted at its start
	r.c.Invalidate(a.ID)        // a change lands while that reconcile runs
	r.c.store(gens, map[string]Status{a.ID: {State: StateRunning}}, map[string]bool{a.ID: true})
	if st := r.c.Status(mustGet(t, r, a.ID)); st.State != StateStarting {
		t.Errorf("a stale observation was stored: %+v", st)
	}

	// A reconcile that started after the change does store.
	r.c.mu.Lock()
	fresh := map[string]uint64{a.ID: r.c.gen[a.ID]}
	r.c.mu.Unlock()
	r.c.store(fresh, map[string]Status{a.ID: {State: StateRunning}}, map[string]bool{a.ID: true})
	if st := r.c.Status(mustGet(t, r, a.ID)); st.State != StateRunning {
		t.Errorf("a current observation was dropped: %+v", st)
	}
}

func dataRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	r.c.cfg.Data = &DataConfig{
		TokenURL:     "http://booth-streamlit.booth-streamlit.svc:8081/internal/token",
		BrokerURL:    "http://booth-streamlit.booth-streamlit.svc:8081/internal/broker",
		SidecarImage: "ghcr.io/projectbooth/credential-sidecar@sha256:6a0a795efd27f165e0714beb163d91f5c2feff55cfdc6f287aee979ae02cce14",
		Database:     true,
	}
	return r
}

// ADR 0107 item 1 / design-data-access item 2: the token volume is mounted into the gate (rw) and the
// postgres sidecar (ro) and NEVER into the Streamlit container; the pod does not share process
// namespaces; the sidecar asks for read only, on the app's own workspace, through the backend.
func TestDeployment_DataAccessKeepsTheTokenAwayFromUserCode(t *testing.T) {
	r := dataRig(t)
	a := r.create("Sales")
	r.reconcile()
	d := r.deployment(a.ID)
	spec := d.Spec.Template.Spec

	if spec.ShareProcessNamespace != nil && *spec.ShareProcessNamespace {
		t.Fatal("the pod shares process namespaces: user code could read the sidecar's /proc")
	}
	var tokenVol *corev1.Volume
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == "token" {
			tokenVol = &spec.Volumes[i]
		}
	}
	if tokenVol == nil || tokenVol.EmptyDir == nil || tokenVol.EmptyDir.Medium != corev1.StorageMediumMemory {
		t.Fatalf("token volume %+v: want a memory-backed emptyDir", tokenVol)
	}
	mounts := map[string]string{}
	for _, c := range spec.Containers {
		for _, m := range c.VolumeMounts {
			if m.Name == "token" {
				mode := "rw"
				if m.ReadOnly {
					mode = "ro"
				}
				mounts[c.Name] = mode
			}
		}
		for _, e := range c.Env {
			if strings.Contains(strings.ToLower(e.Name), "token") && c.Name != "gate" {
				t.Errorf("%s has env %s", c.Name, e.Name)
			}
		}
	}
	if want := map[string]string{"gate": "rw", "pg-sidecar": "ro"}; !reflect.DeepEqual(mounts, want) {
		t.Fatalf("token volume mounted into %v, want exactly %v (never the streamlit container)", mounts, want)
	}

	pg := container(t, d, "pg-sidecar")
	want := []string{
		"--kind=postgres", "--access=read", `--scope={"workspace":"acme"}`, "--workspace=acme",
		"--token-file=/var/run/booth/token/token", "--listen=127.0.0.1:5432",
		"--core-url=http://booth-streamlit.booth-streamlit.svc:8081/internal/broker",
	}
	if !reflect.DeepEqual(pg.Args, want) {
		t.Errorf("pg-sidecar args\n got %v\nwant %v", pg.Args, want)
	}
	if len(pg.Env) != 0 {
		t.Errorf("pg-sidecar has env %v; everything it needs is a flag", pg.Env)
	}
	if !strings.Contains(pg.Image, "@sha256:") {
		t.Error("sidecar image is not digest-pinned")
	}
	st := container(t, d, "streamlit")
	env := map[string]string{}
	for _, e := range st.Env {
		env[e.Name] = e.Value
	}
	if env["DATABASE_URL"] != "postgresql://localhost:5432/"+WorkspaceDatabase("acme") {
		t.Errorf("DATABASE_URL = %q", env["DATABASE_URL"])
	}
	if env["BOOTH_DATA_STATUS_URL"] != "http://127.0.0.1:8090/_booth/data/status" {
		t.Errorf("status URL %q", env["BOOTH_DATA_STATUS_URL"])
	}
	if WorkspaceDatabase("acme") != "bdb_ws_"+WorkspaceDatabase("acme")[7:] || len(WorkspaceDatabase("acme")) != 31 {
		t.Errorf("database name %q", WorkspaceDatabase("acme"))
	}
}

// A refused renewal bumps the app's data epoch; the pod template changes, so the pod rolls and
// Postgres connections opened under the old lease end (ADR 0107 item 6).
func TestDeployment_DataEpochRollsThePod(t *testing.T) {
	r := dataRig(t)
	a := r.create("Sales")
	r.reconcile()
	before := r.deployment(a.ID).Spec.Template.Annotations[annoDataEpoch]
	cur, _ := r.svc.Get(r.ctx, owner, a.ID)
	if _, err := r.svc.DataPaused(r.ctx, cur, "owner gone"); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	if after := r.deployment(a.ID).Spec.Template.Annotations[annoDataEpoch]; after == before {
		t.Fatalf("data epoch annotation unchanged (%s): the pod would keep its open connections", after)
	}
}

// Without data access configured, app pods get none of it.
func TestDeployment_NoDataAccessByDefault(t *testing.T) {
	r := newRig(t)
	a := r.create("Sales")
	r.reconcile()
	for _, c := range r.deployment(a.ID).Spec.Template.Spec.Containers {
		if c.Name == "pg-sidecar" {
			t.Fatal("pg-sidecar without data access")
		}
		for _, e := range c.Env {
			if e.Name == "DATABASE_URL" || e.Name == "BOOTH_GATE_TOKEN_URL" {
				t.Errorf("%s set without data access", e.Name)
			}
		}
	}
}
