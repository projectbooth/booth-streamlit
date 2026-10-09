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
	"strings"
	"syscall"
	"time"

	"github.com/projectbooth/booth-streamlit/internal/api"
	"github.com/projectbooth/booth-streamlit/internal/apps"
	"github.com/projectbooth/booth-streamlit/internal/config"
	"github.com/projectbooth/booth-streamlit/internal/dataaccess"
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

	// One token cache per backend: the gates' refresh, the file proxy and the warehouse lookup all
	// use an app's same current token.
	var tokens *dataaccess.Tokens
	if cfg.Data.Enabled {
		tokens = &dataaccess.Tokens{Minter: &dataaccess.CoreMinter{URL: cfg.Data.MintURL, Credential: cfg.Data.MintCredential}, MaxAge: cfg.Data.RefreshMax}
	}
	ctrl, err := newLifecycle(ctx, cfg.Lifecycle, cfg.Data, svc, tokens)
	if err != nil {
		return err
	}
	if cfg.Data.Enabled {
		// The internal port: the gates' token refresh and the sidecars' broker forwarder. App pods
		// reach it; nothing else should (NetworkPolicy), and it never goes through core.
		in := &dataaccess.Internal{
			Apps:    svc,
			Tokens:  tokens,
			CoreURL: cfg.Data.CoreURL,
			// The file read proxy: storage and catalog files through core's gateway, as the app.
			Files: &dataaccess.Files{
				GatewayURL:     cfg.Data.CoreURL,
				MaxObjectBytes: cfg.Data.FilesMaxObjectBytes,
				ObjectTimeout:  cfg.Data.FilesObjectTimeout,
				MetaTimeout:    cfg.Data.FilesMetaTimeout,
				MaxConcurrent:  cfg.Data.FilesMaxConcurrent,
			},
		}
		internal := &http.Server{Addr: cfg.Data.InternalAddr, Handler: in.Router(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			log.Printf("booth-streamlit internal port listening on %s", cfg.Data.InternalAddr)
			if err := internal.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("internal port: %v", err)
			}
		}()
		go func() {
			<-ctx.Done()
			_ = internal.Close()
		}()
	} else {
		log.Print("data access is off (dataAccess.enabled=false): apps run without database or file access")
	}
	svc.OnChange = ctrl.OnAppChange
	go ctrl.Run(ctx)

	server := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: api.NewRouter(api.Deps{
			DB: pool, Bus: bus, Web: api.WebDir(cfg.WebDir),
			Verifier: verifier, Apps: svc, Status: ctrl.Status, DataAccess: cfg.Data.Enabled,
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
func newLifecycle(ctx context.Context, l config.Lifecycle, d config.Data, svc *apps.Service, tokens *dataaccess.Tokens) (*lifecycle.Controller, error) {
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
	var data *lifecycle.DataConfig
	if d.Enabled {
		var sideRes corev1.ResourceRequirements
		if d.SidecarResources != "" {
			if err := json.Unmarshal([]byte(d.SidecarResources), &sideRes); err != nil {
				return nil, fmt.Errorf("BOOTH_APP_SIDECAR_RESOURCES: %w", err)
			}
		}
		data = &lifecycle.DataConfig{
			TokenURL:         strings.TrimRight(d.InternalURL, "/") + "/internal/token",
			BrokerURL:        strings.TrimRight(d.InternalURL, "/") + "/internal/broker",
			FilesURL:         strings.TrimRight(d.InternalURL, "/"),
			SidecarImage:     d.SidecarImage,
			SidecarResources: sideRes,
			Database:         d.Database,
		}
		if d.RefreshMax > 0 {
			data.RefreshMax = d.RefreshMax.String()
		}
		if d.Lakehouse {
			lh := &dataaccess.Lakehouse{GatewayURL: d.CoreURL, Tokens: tokens}
			data.Lakehouse = func(ctx context.Context, a apps.App) (*lifecycle.Warehouse, error) {
				wh, err := lh.Lookup(ctx, a)
				if wh == nil || err != nil {
					return nil, err
				}
				return &lifecycle.Warehouse{BackendID: wh.BackendID, Path: wh.Path, StorageRoot: wh.StorageRoot}, nil
			}
		}
	}
	return lifecycle.New(lifecycle.Config{
		Data:      data,
		Namespace: l.Namespace, RuntimeImage: l.RuntimeImage, GateImage: l.GateImage,
		PullPolicy: corev1.PullPolicy(l.PullPolicy), ServiceAccount: l.ServiceAccount,
		AppResources: appRes, GateResources: gateRes, TmpSizeLimit: l.TmpSizeLimit,
		IdleTimeout: l.IdleTimeout, Owner: self,
	}, client, svc), nil
}
