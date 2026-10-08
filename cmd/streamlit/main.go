// Command streamlit is booth-streamlit's module backend: it will own app definitions, run each app
// in its own container, and proxy the shell's iframe traffic to it (ADR 0016). Streamlit itself
// runs only inside those per-app containers, never in this process.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/projectbooth/booth-streamlit/internal/api"
	"github.com/projectbooth/booth-streamlit/internal/apps"
	"github.com/projectbooth/booth-streamlit/internal/config"
	"github.com/projectbooth/booth-streamlit/internal/db"
	"github.com/projectbooth/booth-streamlit/internal/events"
	"github.com/projectbooth/booth-streamlit/internal/identity"
	"github.com/projectbooth/booth-streamlit/internal/lifecycle"
	"github.com/projectbooth/booth-streamlit/internal/proxy"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const shutdownTimeout = 10 * time.Second

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	pool, err := db.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		return err
	}
	defer pool.Close()

	// The bus runs beside the HTTP server, not in its startup path: a NATS outage must never keep
	// the module from serving. Its state shows on /healthz.
	bus := events.New(events.Config{URL: cfg.NATSURL, CredentialsFile: cfg.NATSCredsFile})
	go bus.Run(ctx)

	verifier, err := identity.New(ctx, identity.Config{IssuerURL: cfg.IframeIssuerURL, GroupsClaim: cfg.GroupsClaim})
	if err != nil {
		return err
	}
	store, err := apps.NewPostgresStore(ctx, pool)
	if err != nil {
		return err
	}
	svc := apps.NewService(store, cfg.MaxSourceBytes)
	svc.SetCaps(apps.Caps{MaxRunning: cfg.Lifecycle.MaxRunning, MaxRunningPerWorkspace: cfg.Lifecycle.MaxPerWS})

	ctrl, err := newLifecycle(ctx, cfg.Lifecycle, svc)
	if err != nil {
		return err
	}
	svc.OnChange = ctrl.OnAppChange
	go ctrl.Run(ctx)

	server := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: api.NewRouter(api.Deps{
			DB: pool, Bus: bus, Web: api.WebDir(cfg.WebDir),
			Verifier: verifier, Apps: svc, Status: ctrl.Status,
			Proxy: &proxy.Handler{
				Verifier:     verifier,
				Apps:         proxy.LifecycleResolver{Apps: svc, Lifecycle: ctrl, Namespace: cfg.Lifecycle.Namespace},
				Activity:     ctrl,
				MaxWebsocket: cfg.Lifecycle.MaxWebsocket,
			},
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Printf("booth-streamlit listening on %s", cfg.HTTPAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

// newLifecycle builds the per-app controller from the in-cluster Kubernetes API. The backend's own
// Deployment is read once, to own every app Deployment (so uninstalling removes all apps).
func newLifecycle(ctx context.Context, l config.Lifecycle, svc *apps.Service) (*lifecycle.Controller, error) {
	rc, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("kubernetes config: %w (the backend must run in the cluster it starts apps in)", err)
	}
	client, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	self, err := client.AppsV1().Deployments(l.Namespace).Get(ctx, l.SelfDeployment, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading the backend's own Deployment %s/%s: %w", l.Namespace, l.SelfDeployment, err)
	}
	var appRes, gateRes corev1.ResourceRequirements
	for _, r := range []struct {
		name, raw string
		dst       *corev1.ResourceRequirements
	}{{"BOOTH_APP_RESOURCES", l.AppResources, &appRes}, {"BOOTH_APP_GATE_RESOURCES", l.GateResources, &gateRes}} {
		if r.raw == "" {
			continue
		}
		if err := json.Unmarshal([]byte(r.raw), r.dst); err != nil {
			return nil, fmt.Errorf("%s: %w", r.name, err)
		}
	}
	if _, err := resource.ParseQuantity(l.TmpSizeLimit); err != nil {
		return nil, fmt.Errorf("BOOTH_APP_TMP_SIZE_LIMIT: %w", err)
	}
	return lifecycle.New(lifecycle.Config{
		Namespace: l.Namespace, RuntimeImage: l.RuntimeImage, GateImage: l.GateImage,
		PullPolicy: corev1.PullPolicy(l.PullPolicy), ServiceAccount: l.ServiceAccount,
		AppResources: appRes, GateResources: gateRes, TmpSizeLimit: l.TmpSizeLimit,
		IdleTimeout: l.IdleTimeout, Owner: self,
	}, client, svc), nil
}
