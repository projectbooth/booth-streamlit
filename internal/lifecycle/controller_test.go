package lifecycle

import (
	"context"
	"errors"
	"fmt"
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
		FilesURL:     "http://booth-streamlit.booth-streamlit.svc:8081",
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
	// The file read proxy: app code calls its gate on loopback, never the backend directly.
	if env["BOOTH_FILES_URL"] != "http://127.0.0.1:8090/files" {
		t.Errorf("BOOTH_FILES_URL = %q", env["BOOTH_FILES_URL"])
	}
	gateEnv := map[string]string{}
	for _, e := range container(t, d, "gate").Env {
		gateEnv[e.Name] = e.Value
	}
	if gateEnv["BOOTH_GATE_FILES_URL"] != "http://booth-streamlit.booth-streamlit.svc:8081" {
		t.Errorf("BOOTH_GATE_FILES_URL = %q", gateEnv["BOOTH_GATE_FILES_URL"])
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

// lakeRig is dataRig with booth-lakehouse: the lookup answers wh (or err) and counts its calls.
func lakeRig(t *testing.T, wh *Warehouse, err error) (*rig, *int) {
	t.Helper()
	r := dataRig(t)
	calls := 0
	r.c.cfg.Data.Lakehouse = func(context.Context, apps.App) (*Warehouse, error) {
		calls++
		return wh, err
	}
	return r, &calls
}

var acmeWarehouse = &Warehouse{BackendID: "lake", Path: "acme-data", StorageRoot: "s3://lake/acme-data"}

func (r *rig) start(a apps.App) {
	r.t.Helper()
	if _, err := r.svc.SetDesiredState(r.ctx, owner, a.ID, apps.Running); err != nil {
		r.t.Fatal(err)
	}
	r.reconcile()
}

// ADR 0107 / design-data-access item 3: the s3 sidecar reads the token (ro) and writes the keys
// file; the Streamlit container gets the keys file read only (ADR 0107 accepts that user code can
// read it), never the token. The sidecar asks for read only, scoped to the warehouse, through the
// backend, on a health port off the gate's 8080, as the Streamlit container's uid.
func TestDeployment_S3SidecarForTheWarehouse(t *testing.T) {
	r, _ := lakeRig(t, acmeWarehouse, nil)
	a := r.create("Sales")
	r.start(a)
	d := r.deployment(a.ID)
	spec := d.Spec.Template.Spec

	mounts := map[string]map[string]string{"token": {}, "s3": {}}
	for _, c := range spec.Containers {
		for _, m := range c.VolumeMounts {
			if mounts[m.Name] != nil {
				mode := "rw"
				if m.ReadOnly {
					mode = "ro"
				}
				mounts[m.Name][c.Name] = mode + " " + m.MountPath
			}
		}
	}
	if want := map[string]string{"gate": "rw /var/run/booth/token", "pg-sidecar": "ro /var/run/booth/token", "s3-sidecar": "ro /var/run/booth/token"}; !reflect.DeepEqual(mounts["token"], want) {
		t.Errorf("token volume mounted into %v, want exactly %v (never the streamlit container)", mounts["token"], want)
	}
	if want := map[string]string{"s3-sidecar": "rw /var/run/booth/s3", "streamlit": "ro /var/run/booth/s3"}; !reflect.DeepEqual(mounts["s3"], want) {
		t.Errorf("s3 volume mounted into %v, want %v", mounts["s3"], want)
	}
	for _, v := range spec.Volumes {
		if v.Name == "s3" && (v.EmptyDir == nil || v.EmptyDir.Medium != corev1.StorageMediumMemory) {
			t.Errorf("s3 volume %+v: want a memory-backed emptyDir", v)
		}
	}

	s3 := container(t, d, "s3-sidecar")
	want := []string{
		"--kind=s3", "--access=read",
		`--scope={"backendId":"lake","path":"acme-data"}`, "--workspace=acme",
		"--token-file=/var/run/booth/token/token",
		"--credentials-file=/var/run/booth/s3/credentials", "--health-listen=127.0.0.1:8091",
		"--core-url=http://booth-streamlit.booth-streamlit.svc:8081/internal/broker",
	}
	if !reflect.DeepEqual(s3.Args, want) {
		t.Errorf("s3-sidecar args\n got %v\nwant %v", s3.Args, want)
	}
	if len(s3.Env) != 0 || !strings.Contains(s3.Image, "@sha256:") {
		t.Errorf("s3-sidecar env %v image %q", s3.Env, s3.Image)
	}
	// It writes its files 0600, so it must run as the uid that reads them.
	st := container(t, d, "streamlit")
	uid := *spec.SecurityContext.RunAsUser
	if st.SecurityContext.RunAsUser != nil {
		uid = *st.SecurityContext.RunAsUser
	}
	if s3.SecurityContext.RunAsUser == nil || *s3.SecurityContext.RunAsUser != uid || *s3.SecurityContext.RunAsGroup != uid {
		t.Errorf("s3-sidecar runs as %v, the streamlit container as %d", s3.SecurityContext.RunAsUser, uid)
	}
	if !*s3.SecurityContext.ReadOnlyRootFilesystem || *s3.SecurityContext.AllowPrivilegeEscalation {
		t.Error("s3-sidecar must be read-only, without privilege escalation")
	}
	env := map[string]string{}
	for _, e := range st.Env {
		env[e.Name] = e.Value
	}
	if env["AWS_SHARED_CREDENTIALS_FILE"] != "/var/run/booth/s3/credentials" || env["AWS_CONFIG_FILE"] != "/var/run/booth/s3/credentials.config" ||
		env["BOOTH_WAREHOUSE_ROOT"] != "s3://lake/acme-data" {
		t.Errorf("streamlit env %v", env)
	}
	for k := range env {
		if strings.Contains(k, "SECRET") || strings.Contains(k, "SESSION_TOKEN") || strings.Contains(k, "ACCESS_KEY") {
			t.Errorf("streamlit has key material in env: %s", k)
		}
	}
}

// The warehouse is looked up when a pod is about to start and kept for that pod's life: a running
// app is never rolled because booth-lakehouse changed or was briefly unreachable.
func TestDeployment_WarehouseLookedUpAtStartOnly(t *testing.T) {
	r, calls := lakeRig(t, acmeWarehouse, nil)
	a := r.create("Sales")
	r.reconcile()
	if *calls != 0 {
		t.Fatalf("looked up the warehouse of a stopped app (%d calls)", *calls)
	}
	r.start(a)
	if *calls != 1 {
		t.Fatalf("want one lookup at start, got %d", *calls)
	}
	hash := r.deployment(a.ID).Annotations[annoSpecHash]

	// Running: the lookup now fails, or finds nothing; the pod keeps its warehouse.
	r.c.cfg.Data.Lakehouse = func(context.Context, apps.App) (*Warehouse, error) { *calls++; return nil, nil }
	for i := 0; i < 3; i++ {
		r.reconcile()
	}
	if *calls != 1 || r.deployment(a.ID).Annotations[annoSpecHash] != hash {
		t.Fatalf("a running app was looked up again (%d calls) or its pod changed", *calls)
	}
	// An edit rolls the pod, which keeps the warehouse.
	cur := mustGet(t, r, a.ID)
	if _, err := r.svc.Update(r.ctx, owner, a.ID, apps.Input{Name: cur.Name, Source: "print(2)", Shared: cur.Shared}); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	if *calls != 1 || warehouseOf(r.deployment(a.ID)) == nil {
		t.Fatalf("an edit dropped the warehouse or looked it up (%d calls)", *calls)
	}
	// Stop and start: looked up afresh (none now), so no s3 sidecar.
	if _, err := r.svc.SetDesiredState(r.ctx, owner, a.ID, apps.Stopped); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	r.start(a)
	if *calls != 2 {
		t.Fatalf("want a fresh lookup at the next start, got %d calls", *calls)
	}
	for _, c := range r.deployment(a.ID).Spec.Template.Spec.Containers {
		if c.Name == "s3-sidecar" {
			t.Fatal("s3-sidecar kept after a start that found no warehouse")
		}
	}
}

// Any lookup failure (lakehouse down, a refused mint) means no s3 sidecar, and the app starts.
func TestDeployment_WarehouseLookupFailureStartsWithoutIt(t *testing.T) {
	r, _ := lakeRig(t, nil, errors.New("booth-lakehouse answered 502"))
	a := r.create("Sales")
	r.start(a)
	d := r.deployment(a.ID)
	if *d.Spec.Replicas != 1 {
		t.Fatal("the app did not start")
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == "s3-sidecar" {
			t.Fatal("s3-sidecar without a warehouse")
		}
		for _, e := range c.Env {
			if strings.HasPrefix(e.Name, "AWS_") || e.Name == "BOOTH_WAREHOUSE_ROOT" {
				t.Errorf("%s set without a warehouse", e.Name)
			}
		}
	}
}

func (r *rig) createWithRequirements(name, req string) apps.App {
	r.t.Helper()
	a, err := r.svc.Create(r.ctx, owner, apps.Input{Name: name, Source: "import streamlit as st\nst.write('hi')", Requirements: req, Shared: true})
	if err != nil {
		r.t.Fatal(err)
	}
	return a
}

// design-data-access item 5: the pip init container runs unvetted install steps, so it mounts the
// source (read only) and the empty site volume, and nothing else: no token, no bearer, no S3 keys,
// even with every data-access sidecar in the pod. Streamlit gets site read only, on PYTHONPATH.
func TestDeployment_PipInitContainer(t *testing.T) {
	r, _ := lakeRig(t, acmeWarehouse, nil)
	r.c.cfg.Pip = PipConfig{IndexURL: "http://pypi.booth-pypi.svc:8080/simple", TrustedHost: "pypi.booth-pypi.svc", Deadline: 90 * time.Second, SiteSizeLimit: "1Gi", EgressClosed: true}
	a := r.createWithRequirements("Sales", "humanize==4.12.0\n")
	r.start(a)
	d := r.deployment(a.ID)
	spec := d.Spec.Template.Spec

	if len(spec.InitContainers) != 1 || spec.InitContainers[0].Name != "pip" {
		t.Fatalf("init containers %v", spec.InitContainers)
	}
	pip := spec.InitContainers[0]
	mounts := map[string]string{}
	for _, m := range pip.VolumeMounts {
		mounts[m.Name] = fmt.Sprintf("%s ro=%v", m.MountPath, m.ReadOnly)
	}
	if want := map[string]string{"source": "/app ro=true", "site": "/opt/booth/site ro=false"}; !reflect.DeepEqual(mounts, want) {
		t.Errorf("pip mounts %v, want exactly %v (no token, bearer or credentials)", mounts, want)
	}
	if len(pip.EnvFrom) != 0 {
		t.Error("pip has envFrom")
	}
	env := map[string]string{}
	for _, e := range pip.Env {
		if e.ValueFrom != nil {
			t.Errorf("pip env %s comes from a secret or field", e.Name)
		}
		env[e.Name] = e.Value
	}
	if want := map[string]string{
		"BOOTH_PIP_REQUIREMENTS": "/app/requirements.txt", "BOOTH_PIP_TARGET": "/opt/booth/site",
		"BOOTH_PIP_DEADLINE_SECONDS": "90", "BOOTH_PIP_EGRESS_CLOSED": "true", "PIP_INDEX_URL": "http://pypi.booth-pypi.svc:8080/simple",
		"PIP_TRUSTED_HOST": "pypi.booth-pypi.svc",
	}; !reflect.DeepEqual(env, want) {
		t.Errorf("pip env %v, want %v", env, want)
	}
	if !reflect.DeepEqual(pip.Command, []string{"python", "-m", "booth_streamlit.pip_install"}) || pip.Image != "runtime:1" {
		t.Errorf("pip runs %v from %s", pip.Command, pip.Image)
	}
	if *pip.SecurityContext.RunAsUser != 65532 || !*pip.SecurityContext.ReadOnlyRootFilesystem || *pip.SecurityContext.AllowPrivilegeEscalation {
		t.Error("pip must run as 65532, read-only, without privilege escalation")
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Error("the pod mounts a service-account token")
	}

	st := container(t, d, "streamlit")
	stEnv := map[string]string{}
	for _, e := range st.Env {
		stEnv[e.Name] = e.Value
	}
	if stEnv["PYTHONPATH"] != "/opt/booth/lib:/opt/booth/site" {
		t.Errorf("PYTHONPATH %q", stEnv["PYTHONPATH"])
	}
	var site *corev1.VolumeMount
	for i := range st.VolumeMounts {
		if st.VolumeMounts[i].Name == "site" {
			site = &st.VolumeMounts[i]
		}
	}
	if site == nil || !site.ReadOnly || site.MountPath != "/opt/booth/site" {
		t.Errorf("streamlit site mount %+v: want read only at /opt/booth/site", site)
	}
	for _, v := range spec.Volumes {
		if v.Name == "site" && (v.EmptyDir == nil || v.EmptyDir.SizeLimit.String() != "1Gi" || v.EmptyDir.Medium != "") {
			t.Errorf("site volume %+v: want a 1Gi disk emptyDir", v)
		}
	}
	// The site volume counts: the streamlit container's ephemeral-storage limit grew by 1Gi (the
	// rig's app limits set none).
	if got := st.Resources.Limits[corev1.ResourceEphemeralStorage]; got.String() != "1Gi" {
		t.Errorf("streamlit ephemeral-storage limit %s, want 1Gi more than without requirements", got.String())
	}
	if _, set := r.c.cfg.AppResources.Limits[corev1.ResourceEphemeralStorage]; set {
		t.Error("growing the streamlit container's limit changed the shared AppResources")
	}
	cm, _ := r.client.CoreV1().ConfigMaps(ns).Get(r.ctx, "app-"+a.ID+"-src", metav1.GetOptions{})
	if cm.Data["requirements.txt"] != "humanize==4.12.0\n" {
		t.Errorf("configmap %v", cm.Data)
	}
}

// Without requirements there is no init container, and an existing app's pod hash is unchanged
// (so upgrading the module doesn't roll every app). Changing requirements rolls the pod.
func TestDeployment_RequirementsRollThePod(t *testing.T) {
	r := newRig(t)
	a := r.create("Sales")
	r.reconcile()
	d := r.deployment(a.ID)
	if len(d.Spec.Template.Spec.InitContainers) != 0 {
		t.Fatal("init container without requirements")
	}
	if d.Spec.Template.Annotations[annoSourceHash] != hashOf(a.Source) {
		t.Error("the source hash of an app without requirements changed")
	}
	before := d.Spec.Template.Annotations[annoSourceHash]
	if _, err := r.svc.Update(r.ctx, owner, a.ID, apps.Input{Name: a.Name, Source: a.Source, Requirements: "humanize\n", Shared: a.Shared}); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	d = r.deployment(a.ID)
	if d.Spec.Template.Annotations[annoSourceHash] == before || len(d.Spec.Template.Spec.InitContainers) != 1 {
		t.Fatal("adding requirements did not roll the pod into one with the pip init container")
	}
	cm, _ := r.client.CoreV1().ConfigMaps(ns).Get(r.ctx, "app-"+a.ID+"-src", metav1.GetOptions{})
	if cm.Data["requirements.txt"] != "humanize\n" {
		t.Errorf("configmap not updated with the requirements: %v", cm.Data)
	}
}

// Status while installing, and after a failed install: the termination message reaches the owner,
// and a failed install never sits in "starting".
func TestObserve_Pip(t *testing.T) {
	a := apps.App{ID: "a1", DesiredState: apps.Running}
	d := &appsv1.Deployment{}
	d.Spec.Template.Annotations = map[string]string{annoSourceHash: "h1"}
	pod := func(cs corev1.ContainerStatus, hash string) []corev1.Pod {
		p := corev1.Pod{}
		p.Annotations = map[string]string{annoSourceHash: hash}
		cs.Name = "pip"
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{cs}
		return []corev1.Pod{p}
	}
	running := corev1.ContainerStatus{State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
	if st := observe(a, d, pod(running, "h1")); st.State != StateInstalling {
		t.Errorf("installing: %+v", st)
	}
	failed := corev1.ContainerStatus{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "pip install failed (exit 1):\nERROR: No matching distribution found for nope\n"}}}
	if st := observe(a, d, pod(failed, "h1")); st.State != StateFailed || st.Reason != "pip install failed (exit 1):\nERROR: No matching distribution found for nope" {
		t.Errorf("failed: %+v", st)
	}
	// The kubelet retries with back-off: the last attempt failed. Still failed, never "installing".
	retry := corev1.ContainerStatus{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "pip install did not finish within 60s"}}}
	if st := observe(a, d, pod(retry, "h1")); st.State != StateFailed || !strings.HasPrefix(st.Reason, "pip install did not finish") {
		t.Errorf("retrying: %+v", st)
	}
	oom := corev1.ContainerStatus{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}}}
	if st := observe(a, d, pod(oom, "h1")); st.Reason != "pip install ran out of memory (the app's memory limit)" {
		t.Errorf("oom: %+v", st)
	}
	// A pod from an older source (being replaced) doesn't speak for the app.
	if st := observe(a, d, pod(failed, "old")); st.State != StateStarting {
		t.Errorf("an old pod's failure leaked: %+v", st)
	}
	done := corev1.ContainerStatus{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}
	if st := observe(a, d, pod(done, "h1")); st.State != StateStarting {
		t.Errorf("installed, streamlit starting: %+v", st)
	}
}
