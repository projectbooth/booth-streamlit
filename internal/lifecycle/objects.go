package lifecycle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/projectbooth/booth-streamlit/internal/apps"
)

// Labels on every object the lifecycle creates. LabelComponent=app is how it finds its own objects
// (and the chart's NetworkPolicy finds app pods); LabelWorkspace is set from the app's stored
// workspace, never from anything its code can influence (ADR 0077).
const (
	LabelModule    = "booth.projectbooth.io/module"
	LabelComponent = "booth.projectbooth.io/component"
	LabelAppID     = "booth.projectbooth.io/app-id"
	LabelWorkspace = "booth.projectbooth.io/workspace"

	componentApp = "app"

	annoSourceHash = "booth.projectbooth.io/source-hash"
	annoSpecHash   = "booth.projectbooth.io/spec-hash"

	gatePort      = 8080
	streamlitPort = 8501
	bearerKey     = "bearer"
	sourceKey     = "app.py"
)

// Selector matches every object the lifecycle owns.
var Selector = LabelComponent + "=" + componentApp

// Names of an app's objects. App ids are "a" + 12 base-32 characters, so these are valid DNS
// labels well under the length limits.
func deploymentName(id string) string { return "app-" + id }
func serviceName(id string) string    { return "app-" + id }
func configMapName(id string) string  { return "app-" + id + "-src" }
func secretName(id string) string     { return "app-" + id + "-gate" }

// ServiceURL is where the backend reaches an app's gate.
func ServiceURL(namespace, id string) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", serviceName(id), namespace, gatePort)
}

func basePath(id string) string { return "apps/" + id }

func labels(a apps.App) map[string]string {
	return map[string]string{
		LabelModule:    "streamlit",
		LabelComponent: componentApp,
		LabelAppID:     a.ID,
		LabelWorkspace: a.Workspace,
	}
}

func hashOf(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

func sourceHash(a apps.App) string { return hashOf(a.Source) }

func configMap(ns string, a apps.App) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configMapName(a.ID), Namespace: ns, Labels: labels(a)},
		Data:       map[string]string{sourceKey: a.Source},
	}
}

func secret(ns string, a apps.App) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName(a.ID), Namespace: ns, Labels: labels(a)},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{bearerKey: a.GateBearer},
	}
}

func service(ns string, a apps.App) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: serviceName(a.ID), Namespace: ns, Labels: labels(a)},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{LabelComponent: componentApp, LabelAppID: a.ID},
			Ports:    []corev1.ServicePort{{Name: "http", Port: gatePort, TargetPort: intstr.FromString("http")}},
		},
	}
}

// deployment builds an app's Deployment: Streamlit on loopback plus the gate as its only network
// entry point (design note (a)/(b)). Replicas is 1 only while the app is Active.
func deployment(cfg Config, a apps.App) *appsv1.Deployment {
	f := false
	t := true
	uid := int64(65532)
	replicas := int32(0)
	if apps.Active(a) {
		replicas = 1
	}
	podLabels := labels(a)
	tmp := resource.MustParse(cfg.TmpSizeLimit)
	readOnly := corev1.SecurityContext{
		AllowPrivilegeEscalation: &f,
		ReadOnlyRootFilesystem:   &t,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
	tpl := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      podLabels,
			Annotations: map[string]string{annoSourceHash: sourceHash(a)},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName:           cfg.ServiceAccount,
			AutomountServiceAccountToken: &f,
			// No *_SERVICE_HOST variables for every Service in the namespace in app code's env.
			EnableServiceLinks: &f,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   &t,
				RunAsUser:      &uid,
				RunAsGroup:     &uid,
				FSGroup:        &uid,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{
				{
					Name:            "streamlit",
					Image:           cfg.RuntimeImage,
					ImagePullPolicy: cfg.PullPolicy,
					Env: []corev1.EnvVar{
						{Name: "STREAMLIT_SERVER_BASE_URL_PATH", Value: basePath(a.ID)},
						// Loopback only: the gate is the pod's one way in.
						{Name: "STREAMLIT_SERVER_ADDRESS", Value: "127.0.0.1"},
						{Name: "STREAMLIT_SERVER_PORT", Value: fmt.Sprint(streamlitPort)},
					},
					Resources:       cfg.AppResources,
					SecurityContext: &readOnly,
					VolumeMounts: []corev1.VolumeMount{
						{Name: "source", MountPath: "/app", ReadOnly: true},
						{Name: "tmp", MountPath: "/tmp"},
					},
				},
				{
					Name:            "gate",
					Image:           cfg.GateImage,
					ImagePullPolicy: cfg.PullPolicy,
					Command:         []string{"/booth-streamlit-gate"},
					Env: []corev1.EnvVar{
						{Name: "BOOTH_GATE_LISTEN", Value: fmt.Sprintf(":%d", gatePort)},
						{Name: "BOOTH_GATE_UPSTREAM", Value: fmt.Sprintf("http://127.0.0.1:%d", streamlitPort)},
						{Name: "BOOTH_GATE_UPSTREAM_HEALTH", Value: "/" + basePath(a.ID) + "/_stcore/health"},
						{Name: "BOOTH_GATE_BEARER_FILE", Value: "/etc/booth/gate/" + bearerKey},
					},
					Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: gatePort}},
					Resources:       cfg.GateResources,
					SecurityContext: &readOnly,
					ReadinessProbe: &corev1.Probe{
						ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/_booth/gate/healthz", Port: intstr.FromString("http")}},
						PeriodSeconds: 2, FailureThreshold: 3,
					},
					VolumeMounts: []corev1.VolumeMount{{Name: "gate", MountPath: "/etc/booth/gate", ReadOnly: true}},
				},
			},
			Volumes: []corev1.Volume{
				{Name: "source", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: configMapName(a.ID)}}}},
				{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &tmp}}},
				// Mounted into the gate only: the bearer never reaches the Streamlit container.
				{Name: "gate", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secretName(a.ID), DefaultMode: int32Ptr(0o440)}}},
			},
		},
	}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: deploymentName(a.ID), Namespace: cfg.Namespace, Labels: labels(a)},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{LabelComponent: componentApp, LabelAppID: a.ID}},
			// Recreate, not RollingUpdate: a code change must never briefly run two pods of one app,
			// which would be one more than the running-app cap accounts for.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: tpl,
		},
	}
	d.Annotations = map[string]string{annoSpecHash: hashOf(d.Spec)}
	return d
}

func int32Ptr(v int32) *int32 { return &v }
