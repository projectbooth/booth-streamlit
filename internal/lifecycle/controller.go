// Package lifecycle runs each app as its own Deployment and keeps the cluster matching the app
// model (design note (a)). The database is the source of truth: a reconcile loop lists the apps
// and makes every app's ConfigMap (its source), Secret (the gate's bearer), Service and Deployment
// match, scaling the Deployment to 1 only while the app is Active (its owner chose Running and it
// is not idle-suspended). Objects whose app no longer exists are deleted.
//
// Ownership chain: each app's Deployment is owned by the module backend's own Deployment, and the
// app's ConfigMap, Secret and Service are owned by its Deployment. So deleting an app's
// Deployment removes everything of that app, and `helm uninstall` (which deletes the backend's
// Deployment) removes every app, although Helm never created them.
//
// The backend's Role (charts/.../rbac.yaml) is exactly what this needs and no more: it can create
// and delete Secrets but never read them, which is why the bearer is also kept in the database.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/projectbooth/booth-streamlit/internal/apps"
)

// Config configures the lifecycle.
type Config struct {
	Namespace      string
	RuntimeImage   string
	GateImage      string
	PullPolicy     corev1.PullPolicy
	ServiceAccount string
	AppResources   corev1.ResourceRequirements
	GateResources  corev1.ResourceRequirements
	TmpSizeLimit   string
	// IdleTimeout: an Active app with no open websocket and no request for this long is suspended.
	// 0 disables idle shutdown.
	IdleTimeout time.Duration
	// Interval between reconciles when nothing triggers one sooner.
	Interval time.Duration
	// Owner is the module backend's own Deployment; every app Deployment is owned by it.
	Owner *appsv1.Deployment
	// Data, if set, gives app pods data access (ADR 0104/0107); nil runs apps without any.
	Data *DataConfig
}

// DataConfig is the per-app data-access wiring (docs/design-data-access.md).
type DataConfig struct {
	// TokenURL is the backend's POST /internal/token, which the gate calls with its bearer.
	TokenURL string
	// BrokerURL is the backend's broker forwarder base; the sidecars' --core-url.
	BrokerURL string
	// FilesURL is the backend's internal base the gate forwards /files/... to (the file read proxy).
	FilesURL string
	// SidecarImage is booth-core's credential sidecar, digest-pinned.
	SidecarImage     string
	SidecarResources corev1.ResourceRequirements
	// Database adds the postgres sidecar and DATABASE_URL (booth-database installed).
	Database bool
	// RefreshMax caps the gate's refresh interval (a test knob; empty = two thirds of a token's life).
	RefreshMax string
}

// State is an app's observed state, as the API and the proxy report it.
type State string

const (
	StateStopped   State = "stopped"   // owner stopped it
	StateSuspended State = "suspended" // idle shutdown; opening it wakes it
	StateStarting  State = "starting"
	StateRunning   State = "running"
	StateFailed    State = "failed"
)

// Status is an app's observed state plus, when failed, why.
type Status struct {
	State  State  `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// Controller reconciles apps to Kubernetes objects and tracks activity for idle shutdown.
type Controller struct {
	cfg     Config
	client  kubernetes.Interface
	apps    *apps.Service
	now     func() time.Time
	trigger chan struct{}

	mu       sync.Mutex
	status   map[string]Status
	activity map[string]*activity
	// gen counts Invalidate calls per app. A reconcile records it before it lists the apps and
	// stores an app's observation only if it hasn't moved: an observation taken from a snapshot
	// older than the latest change is never written back.
	gen map[string]uint64
}

type activity struct {
	last time.Time
	ws   int // open websockets
}

// New returns a Controller. Call Run to start reconciling.
func New(cfg Config, client kubernetes.Interface, svc *apps.Service) *Controller {
	if cfg.Interval == 0 {
		cfg.Interval = 5 * time.Second
	}
	return &Controller{
		cfg: cfg, client: client, apps: svc, now: time.Now,
		trigger:  make(chan struct{}, 1),
		status:   map[string]Status{},
		activity: map[string]*activity{},
		gen:      map[string]uint64{},
	}
}

// Invalidate forgets what was observed about an app, synchronously, because it just changed (a
// start, stop, suspend, wake, edit or delete). Until the next reconcile observes it afresh, an
// Active app reports StateStarting, never a "running" that described the pod before the change.
// Without this the proxy could forward to a pod being scaled away (a real race, found by the
// lifecycle integration test: a viewer visiting two seconds after idle shutdown got a 502).
func (c *Controller) Invalidate(id string) {
	c.mu.Lock()
	delete(c.status, id)
	c.gen[id]++
	c.mu.Unlock()
}

// OnAppChange is the apps.Service hook: invalidate, then reconcile soon.
func (c *Controller) OnAppChange(id string) {
	c.Invalidate(id)
	c.Trigger()
}

// Trigger asks for a reconcile soon; it never blocks.
func (c *Controller) Trigger() {
	select {
	case c.trigger <- struct{}{}:
	default:
	}
}

// Run reconciles until ctx ends.
func (c *Controller) Run(ctx context.Context) {
	t := time.NewTicker(c.cfg.Interval)
	defer t.Stop()
	for {
		if err := c.Reconcile(ctx); err != nil && ctx.Err() == nil {
			log.Printf("lifecycle: reconcile: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-c.trigger:
		}
	}
}

// Status reports an app's last observed state. An app the controller hasn't observed yet is
// reported as starting if Active, stopped otherwise; callers pass the app they already loaded.
func (c *Controller) Status(a apps.App) Status {
	c.mu.Lock()
	st, ok := c.status[a.ID]
	c.mu.Unlock()
	switch {
	case a.DesiredState != apps.Running:
		return Status{State: StateStopped}
	case a.Suspended:
		return Status{State: StateSuspended}
	case !ok || st.State == StateStopped || st.State == StateSuspended:
		return Status{State: StateStarting}
	}
	return st
}

// Touch records a request to an app (for idle shutdown).
func (c *Controller) Touch(id string) {
	c.mu.Lock()
	c.act(id).last = c.now()
	c.mu.Unlock()
}

// WebsocketOpened and WebsocketClosed bracket a proxied websocket. An app with an open websocket
// is never idle: an open browser tab is activity.
func (c *Controller) WebsocketOpened(id string) {
	c.mu.Lock()
	a := c.act(id)
	a.ws++
	a.last = c.now()
	c.mu.Unlock()
}

func (c *Controller) WebsocketClosed(id string) {
	c.mu.Lock()
	a := c.act(id)
	if a.ws > 0 {
		a.ws--
	}
	a.last = c.now()
	c.mu.Unlock()
}

func (c *Controller) act(id string) *activity {
	a, ok := c.activity[id]
	if !ok {
		a = &activity{last: c.now()}
		c.activity[id] = a
	}
	return a
}

// Reconcile makes the cluster match the app model once, then applies idle shutdown.
func (c *Controller) Reconcile(ctx context.Context) error {
	c.mu.Lock()
	gens := make(map[string]uint64, len(c.gen))
	for id, g := range c.gen {
		gens[id] = g
	}
	c.mu.Unlock()

	all, err := c.apps.All(ctx)
	if err != nil {
		return fmt.Errorf("listing apps: %w", err)
	}
	ns := c.cfg.Namespace
	opts := metav1.ListOptions{LabelSelector: Selector}

	deps, err := c.client.AppsV1().Deployments(ns).List(ctx, opts)
	if err != nil {
		return fmt.Errorf("listing deployments: %w", err)
	}
	existing := map[string]*appsv1.Deployment{}
	for i := range deps.Items {
		existing[deps.Items[i].Labels[LabelAppID]] = &deps.Items[i]
	}
	pods, err := c.client.CoreV1().Pods(ns).List(ctx, opts)
	if err != nil {
		return fmt.Errorf("listing pods: %w", err)
	}
	podsByApp := map[string][]corev1.Pod{}
	for _, p := range pods.Items {
		podsByApp[p.Labels[LabelAppID]] = append(podsByApp[p.Labels[LabelAppID]], p)
	}

	var errs []error
	wanted := map[string]bool{}
	statuses := map[string]Status{}
	for _, a := range all {
		wanted[a.ID] = true
		d, err := c.ensure(ctx, a, existing[a.ID])
		if err != nil {
			errs = append(errs, fmt.Errorf("app %s: %w", a.ID, err))
			continue
		}
		statuses[a.ID] = observe(a, d, podsByApp[a.ID])
	}

	// Garbage: an app deleted from the model. Deleting its Deployment cascades to its ConfigMap,
	// Secret and Service through their owner references; the explicit deletes below cover objects
	// left by a reconcile that failed between creating them and setting the owner.
	for id, d := range existing {
		if !wanted[id] {
			if err := c.client.AppsV1().Deployments(ns).Delete(ctx, d.Name, metav1.DeleteOptions{PropagationPolicy: ptr(metav1.DeletePropagationForeground)}); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("deleting deployment %s: %w", d.Name, err))
			}
		}
	}
	if err := c.deleteOrphans(ctx, wanted); err != nil {
		errs = append(errs, err)
	}

	c.store(gens, statuses, wanted)

	c.idle(ctx, all)
	return errors.Join(errs...)
}

// store records a reconcile's observations, except for apps invalidated since gens was taken, and
// forgets apps that no longer exist.
func (c *Controller) store(gens map[string]uint64, statuses map[string]Status, wanted map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, st := range statuses {
		if c.gen[id] == gens[id] {
			c.status[id] = st
		}
	}
	for id := range c.status {
		if !wanted[id] {
			delete(c.status, id)
		}
	}
	for id := range c.activity {
		if !wanted[id] {
			delete(c.activity, id)
		}
	}
	for id := range c.gen {
		if !wanted[id] && c.gen[id] == gens[id] {
			delete(c.gen, id)
		}
	}
}

// ensure makes one app's objects match, returning its Deployment as it now stands.
func (c *Controller) ensure(ctx context.Context, a apps.App, cur *appsv1.Deployment) (*appsv1.Deployment, error) {
	ns := c.cfg.Namespace
	want := deployment(c.cfg, a)
	want.OwnerReferences = []metav1.OwnerReference{ownerRef(c.cfg.Owner)}

	var d *appsv1.Deployment
	var err error
	switch {
	case cur == nil:
		d, err = c.client.AppsV1().Deployments(ns).Create(ctx, want, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			d, err = c.client.AppsV1().Deployments(ns).Get(ctx, want.Name, metav1.GetOptions{})
		}
	case cur.Annotations[annoSpecHash] != want.Annotations[annoSpecHash]:
		upd := cur.DeepCopy()
		upd.Labels, upd.Annotations, upd.Spec, upd.OwnerReferences = want.Labels, want.Annotations, want.Spec, want.OwnerReferences
		d, err = c.client.AppsV1().Deployments(ns).Update(ctx, upd, metav1.UpdateOptions{})
	default:
		d = cur
	}
	if err != nil {
		return nil, fmt.Errorf("deployment: %w", err)
	}

	owned := []metav1.OwnerReference{ownerRef(d)}
	cm := configMap(ns, a)
	cm.OwnerReferences = owned
	if err := c.ensureConfigMap(ctx, cm); err != nil {
		return nil, err
	}
	sec := secret(ns, a)
	sec.OwnerReferences = owned
	// Create only: the Role cannot read Secrets, and the bearer never changes for an app's life.
	if _, err := c.client.CoreV1().Secrets(ns).Create(ctx, sec, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("secret: %w", err)
	}
	svc := service(ns, a)
	svc.OwnerReferences = owned
	if _, err := c.client.CoreV1().Services(ns).Create(ctx, svc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("service: %w", err)
	}
	return d, nil
}

func (c *Controller) ensureConfigMap(ctx context.Context, want *corev1.ConfigMap) error {
	cms := c.client.CoreV1().ConfigMaps(c.cfg.Namespace)
	cur, err := cms.Get(ctx, want.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = cms.Create(ctx, want, metav1.CreateOptions{})
		return wrap("configmap", err)
	}
	if err != nil {
		return wrap("configmap", err)
	}
	if cur.Data[sourceKey] == want.Data[sourceKey] && len(cur.OwnerReferences) == 1 && cur.OwnerReferences[0].UID == want.OwnerReferences[0].UID {
		return nil
	}
	upd := cur.DeepCopy()
	upd.Data, upd.Labels, upd.OwnerReferences = want.Data, want.Labels, want.OwnerReferences
	_, err = cms.Update(ctx, upd, metav1.UpdateOptions{})
	return wrap("configmap", err)
}

func (c *Controller) deleteOrphans(ctx context.Context, wanted map[string]bool) error {
	ns := c.cfg.Namespace
	opts := metav1.ListOptions{LabelSelector: Selector}
	var errs []error
	if cms, err := c.client.CoreV1().ConfigMaps(ns).List(ctx, opts); err != nil {
		errs = append(errs, err)
	} else {
		for _, o := range cms.Items {
			if !wanted[o.Labels[LabelAppID]] {
				if err := c.client.CoreV1().ConfigMaps(ns).Delete(ctx, o.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
					errs = append(errs, err)
				}
			}
		}
	}
	if svcs, err := c.client.CoreV1().Services(ns).List(ctx, opts); err != nil {
		errs = append(errs, err)
	} else {
		for _, o := range svcs.Items {
			if !wanted[o.Labels[LabelAppID]] {
				if err := c.client.CoreV1().Services(ns).Delete(ctx, o.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
					errs = append(errs, err)
				}
			}
		}
	}
	return errors.Join(errs...)
}

// idle suspends Active apps with no open websocket and no request for IdleTimeout. An app first
// seen Active (or after a backend restart) gets a full timeout from now.
func (c *Controller) idle(ctx context.Context, all []apps.App) {
	if c.cfg.IdleTimeout <= 0 {
		return
	}
	now := c.now()
	var idle []apps.App
	c.mu.Lock()
	for _, a := range all {
		if !apps.Active(a) {
			delete(c.activity, a.ID) // a fresh timeout next time it becomes Active
			continue
		}
		act := c.act(a.ID)
		if act.ws == 0 && now.Sub(act.last) >= c.cfg.IdleTimeout {
			idle = append(idle, a)
		}
	}
	c.mu.Unlock()
	for _, a := range idle {
		log.Printf("lifecycle: app %s idle for %s: suspending", a.ID, c.cfg.IdleTimeout)
		if err := c.apps.Suspend(ctx, a.Workspace, a.ID); err != nil {
			log.Printf("lifecycle: suspending %s: %v", a.ID, err)
		}
	}
}

// fatalWaiting are container waiting reasons that won't resolve on their own.
var fatalWaiting = map[string]bool{
	"ErrImagePull": true, "ImagePullBackOff": true, "InvalidImageName": true,
	"CreateContainerConfigError": true, "CreateContainerError": true, "CrashLoopBackOff": true,
}

func observe(a apps.App, d *appsv1.Deployment, pods []corev1.Pod) Status {
	switch {
	case a.DesiredState != apps.Running:
		return Status{State: StateStopped}
	case a.Suspended:
		return Status{State: StateSuspended}
	}
	for _, p := range pods {
		for _, cs := range p.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil && fatalWaiting[w.Reason] {
				return Status{State: StateFailed, Reason: fmt.Sprintf("%s: %s", cs.Name, w.Reason)}
			}
		}
	}
	if d != nil && d.Status.ReadyReplicas >= 1 && d.Status.ObservedGeneration >= d.Generation && d.Status.UpdatedReplicas >= 1 {
		return Status{State: StateRunning}
	}
	return Status{State: StateStarting}
}

// ownerRef points at d. BlockOwnerDeletion is false on purpose: setting it true requires update on
// deployments/finalizers wherever the OwnerReferencesPermissionEnforcement admission plugin runs,
// which the backend's Role deliberately does not grant. Garbage collection works without it; only
// foreground deletion of the owner stops waiting for these dependents.
func ownerRef(d *appsv1.Deployment) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "apps/v1", Kind: "Deployment", Name: d.Name, UID: d.UID,
		BlockOwnerDeletion: ptr(false), Controller: ptr(true),
	}
}

func wrap(what string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", what, err)
}

func ptr[T any](v T) *T { return &v }
