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
	annoDataEpoch  = "booth.projectbooth.io/data-epoch"
	// annoWarehouse, on the Deployment, is the lakehouse warehouse its pod's s3 sidecar is scoped to
	// (JSON), or empty for none. Looked up when the app starts and kept for that pod's life.
	annoWarehouse = "booth.projectbooth.io/warehouse"

	tokenDir     = "/var/run/booth/token"
	tokenFile    = tokenDir + "/token"
	statusAddr   = "127.0.0.1:8090"
	pgListen     = "127.0.0.1:5432"
	annoSpecHash = "booth.projectbooth.io/spec-hash"

	// The s3 sidecar's output: AWS's two standard files, the keys in s3CredFile and the endpoint
	// in s3CredFile + ".config" (contracts/credential-sidecar.md). Off the default health port
	// 8080, which is the gate's.
	s3Dir          = "/var/run/booth/s3"
	s3CredFile     = s3Dir + "/credentials"
	s3HealthListen = "127.0.0.1:8091"

	// appUID is the uid every container of an app pod runs as, the Streamlit container's included.
	// The s3 sidecar writes its files 0600, so it must run as the uid that reads them (the
	// contract's requirement); the image's own user is the same 65532, set here explicitly.
	appUID = int64(65532)

	gatePort      = 8080
	streamlitPort = 8501
	bearerKey     = "bearer"
	sourceKey     = "app.py"
	reqKey        = "requirements.txt"

	// Per-app packages: the pip init container installs into siteDir, which the Streamlit
	// container mounts read only, after the image's own /opt/booth/lib on PYTHONPATH.
	siteDir    = "/opt/booth/site"
	pythonPath = "/opt/booth/lib:" + siteDir
)

// Warehouse is a workspace's lakehouse warehouse, as booth-lakehouse's GET /api/warehouse answers
// it: the s3 sidecar's scope is {BackendID, Path}; StorageRoot (s3://bucket/prefix) tells app code
// where the warehouse's files are.
type Warehouse struct {
	BackendID   string `json:"backendId"`
	Path        string `json:"path"`
	StorageRoot string `json:"storageRoot"`
}

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

// sourceHash covers everything delivered with the source: a change to either rolls the pod.
func sourceHash(a apps.App) string {
	if a.Requirements == "" {
		return hashOf(a.Source) // unchanged for apps without requirements, so they don't roll
	}
	return hashOf([]string{a.Source, a.Requirements})
}

func configMap(ns string, a apps.App) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configMapName(a.ID), Namespace: ns, Labels: labels(a)},
		Data:       configData(a),
	}
}

func configData(a apps.App) map[string]string {
	d := map[string]string{sourceKey: a.Source}
	if a.Requirements != "" {
		d[reqKey] = a.Requirements
	}
	return d
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
// entry point (design note (a)/(b)). Replicas is 1 only while the app is Active. wh, if not nil,
// adds the s3 sidecar for that warehouse (with data access on).
func deployment(cfg Config, a apps.App, wh *Warehouse) *appsv1.Deployment {
	f := false
	t := true
	uid := appUID
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
			Annotations: map[string]string{annoSourceHash: sourceHash(a), annoDataEpoch: fmt.Sprint(a.DataEpoch)},
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
	whAnno := ""
	if a.Requirements != "" {
		addPip(cfg, &tpl.Spec, readOnly)
	}
	if cfg.Data != nil {
		addDataAccess(cfg, a, &tpl.Spec, readOnly)
		if wh != nil {
			addLakehouse(cfg, a, *wh, &tpl.Spec, readOnly)
			b, _ := json.Marshal(wh)
			whAnno = string(b)
		}
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
	d.Annotations = map[string]string{annoSpecHash: hashOf(d.Spec), annoWarehouse: whAnno}
	return d
}

// addPip installs the app's requirements.txt on every pod start (ADR 0107; docs/design-data-access.md
// item 5): an init container from the runtime image runs booth_streamlit.pip_install into the
// "site" volume, which the Streamlit container then mounts read only, on PYTHONPATH.
//
// The init container runs unvetted packages' install steps, so it mounts exactly two things: the
// app's source (read only, for requirements.txt) and the empty site volume. No token, no bearer,
// no credentials, and the pod has no service-account token. Data access only ever adds to the
// regular containers, never to this one.
func addPip(cfg Config, spec *corev1.PodSpec, sc corev1.SecurityContext) {
	pip := cfg.Pip
	size := resource.MustParse(pip.SiteSizeLimit)
	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name:         "site",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &size}},
	})
	env := []corev1.EnvVar{
		{Name: "BOOTH_PIP_REQUIREMENTS", Value: "/app/" + reqKey},
		{Name: "BOOTH_PIP_TARGET", Value: siteDir},
		{Name: "BOOTH_PIP_DEADLINE_SECONDS", Value: fmt.Sprint(int(pip.Deadline.Seconds()))},
		{Name: "BOOTH_PIP_EGRESS_CLOSED", Value: fmt.Sprint(pip.EgressClosed)},
	}
	if pip.IndexURL != "" {
		env = append(env, corev1.EnvVar{Name: "PIP_INDEX_URL", Value: pip.IndexURL})
	}
	uid := appUID
	pipSC := sc
	pipSC.RunAsUser, pipSC.RunAsGroup = &uid, &uid
	spec.InitContainers = append(spec.InitContainers, corev1.Container{
		Name:            "pip",
		Image:           cfg.RuntimeImage,
		ImagePullPolicy: cfg.PullPolicy,
		Command:         []string{"python", "-m", "booth_streamlit.pip_install"},
		Env:             env,
		// The app's own limits: a pod's effective limit is the larger of its init containers' and
		// the sum of its containers', so the install costs the quota nothing extra.
		Resources:                cfg.AppResources,
		SecurityContext:          &pipSC,
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "source", MountPath: "/app", ReadOnly: true},
			{Name: "site", MountPath: siteDir},
		},
	})
	for i := range spec.Containers {
		c := &spec.Containers[i]
		if c.Name != "streamlit" {
			continue
		}
		c.Env = append(c.Env, corev1.EnvVar{Name: "PYTHONPATH", Value: pythonPath})
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "site", MountPath: siteDir, ReadOnly: true})
		// The site volume is on the node's disk and counts against the pod's ephemeral storage, so
		// the container's limit (and with it the namespace quota) grows by its size limit.
		c.Resources = *c.Resources.DeepCopy()
		if c.Resources.Limits == nil {
			c.Resources.Limits = corev1.ResourceList{}
		}
		eph := c.Resources.Limits[corev1.ResourceEphemeralStorage]
		eph.Add(size)
		c.Resources.Limits[corev1.ResourceEphemeralStorage] = eph
	}
}

// warehouseOf reads back the warehouse a Deployment was built with (nil for none).
func warehouseOf(d *appsv1.Deployment) *Warehouse {
	if d == nil || d.Annotations[annoWarehouse] == "" {
		return nil
	}
	var wh Warehouse
	if json.Unmarshal([]byte(d.Annotations[annoWarehouse]), &wh) != nil || wh.BackendID == "" {
		return nil
	}
	return &wh
}

// addLakehouse adds the s3 credential sidecar for the workspace's warehouse (ADR 0107;
// docs/design-data-access.md item 3). Unlike the token, the keys it writes ARE readable by user
// code: that is how ADR 0095's s3 mode works (any S3 client reads the standard AWS files), and
// ADR 0107 accepts it (item 5 of the plan's section 8). The lease is read only, so the keys can
// read the warehouse's prefix and nothing else, and expire with the lease.
func addLakehouse(cfg Config, a apps.App, wh Warehouse, spec *corev1.PodSpec, sc corev1.SecurityContext) {
	data := cfg.Data
	mem := resource.MustParse("1Mi")
	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name:         "s3",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: &mem}},
	})
	for i := range spec.Containers {
		c := &spec.Containers[i]
		if c.Name != "streamlit" {
			continue
		}
		c.Env = append(c.Env,
			// Read by every AWS SDK; booth_streamlit.pyarrow_fs() and duckdb_secret() pass the
			// endpoint on to the engines that ignore the config file (ADR 0095 Finding 3).
			corev1.EnvVar{Name: "AWS_SHARED_CREDENTIALS_FILE", Value: s3CredFile},
			corev1.EnvVar{Name: "AWS_CONFIG_FILE", Value: s3CredFile + ".config"},
			corev1.EnvVar{Name: "BOOTH_WAREHOUSE_ROOT", Value: wh.StorageRoot},
		)
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "s3", MountPath: s3Dir, ReadOnly: true})
	}
	uid := appUID
	s3SC := sc
	s3SC.RunAsUser = &uid
	s3SC.RunAsGroup = &uid
	scope, _ := json.Marshal(map[string]string{"backendId": wh.BackendID, "path": wh.Path})
	spec.Containers = append(spec.Containers, corev1.Container{
		Name:            "s3-sidecar",
		Image:           data.SidecarImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args: []string{
			"--kind=s3", "--access=read",
			"--scope=" + string(scope), "--workspace=" + a.Workspace,
			"--token-file=" + tokenFile,
			"--credentials-file=" + s3CredFile, "--health-listen=" + s3HealthListen,
			"--core-url=" + data.BrokerURL,
		},
		Resources:       data.SidecarResources,
		SecurityContext: &s3SC,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "token", MountPath: tokenDir, ReadOnly: true},
			{Name: "s3", MountPath: s3Dir},
		},
	})
}

func int32Ptr(v int32) *int32 { return &v }

// addDataAccess wires an app pod for reading data as its owner (ADR 0107; docs/design-data-access.md
// item 2). The rule that matters: the token volume is mounted into the gate (which writes it) and the
// sidecars (which read it), and never into the Streamlit container, where user code runs. Containers
// share the pod's network but not their filesystems, and the pod doesn't share process namespaces.
func addDataAccess(cfg Config, a apps.App, spec *corev1.PodSpec, sc corev1.SecurityContext) {
	data := cfg.Data
	mem := resource.MustParse("1Mi")
	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name:         "token",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: &mem}},
	})
	for i := range spec.Containers {
		c := &spec.Containers[i]
		switch c.Name {
		case "gate":
			c.Env = append(c.Env,
				corev1.EnvVar{Name: "BOOTH_GATE_TOKEN_URL", Value: data.TokenURL},
				corev1.EnvVar{Name: "BOOTH_GATE_TOKEN_FILE", Value: tokenFile},
				corev1.EnvVar{Name: "BOOTH_GATE_STATUS_LISTEN", Value: statusAddr},
			)
			if data.RefreshMax != "" {
				c.Env = append(c.Env, corev1.EnvVar{Name: "BOOTH_GATE_REFRESH_MAX", Value: data.RefreshMax})
			}
			if data.FilesURL != "" {
				c.Env = append(c.Env, corev1.EnvVar{Name: "BOOTH_GATE_FILES_URL", Value: data.FilesURL})
			}
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "token", MountPath: tokenDir})
		case "streamlit":
			c.Env = append(c.Env, corev1.EnvVar{Name: "BOOTH_DATA_STATUS_URL", Value: "http://" + statusAddr + "/_booth/data/status"})
			if data.FilesURL != "" {
				// The file read proxy, through the gate on loopback (booth_streamlit.files).
				c.Env = append(c.Env, corev1.EnvVar{Name: "BOOTH_FILES_URL", Value: "http://" + statusAddr + "/files"})
			}
			if data.Database {
				c.Env = append(c.Env, corev1.EnvVar{Name: "DATABASE_URL", Value: "postgresql://localhost:5432/" + WorkspaceDatabase(a.Workspace)})
			}
		}
	}
	if data.Database {
		uid := appUID
		pgSC := sc
		pgSC.RunAsUser = &uid
		scope, _ := json.Marshal(map[string]string{"workspace": a.Workspace})
		spec.Containers = append(spec.Containers, corev1.Container{
			Name:            "pg-sidecar",
			Image:           data.SidecarImage,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Args: []string{
				"--kind=postgres", "--access=read",
				"--scope=" + string(scope), "--workspace=" + a.Workspace,
				"--token-file=" + tokenFile, "--listen=" + pgListen,
				"--core-url=" + data.BrokerURL,
			},
			Resources:       data.SidecarResources,
			SecurityContext: &pgSC,
			VolumeMounts:    []corev1.VolumeMount{{Name: "token", MountPath: tokenDir, ReadOnly: true}},
		})
	}
}

// WorkspaceDatabase is the workspace's database name in booth-database (its
// internal/naming.ForWorkspace; booth-notebooks computes it the same way). Informational: the sidecar
// connects to whatever database its credential names; this makes DATABASE_URL and
// current_database() agree.
func WorkspaceDatabase(workspace string) string {
	sum := sha256.Sum256([]byte("booth-database/workspace/" + workspace))
	return "bdb_ws_" + hex.EncodeToString(sum[:])[:24]
}
