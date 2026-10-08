// Command streamlit is booth-streamlit's module backend: it will own app definitions, run each app
// in its own container, and proxy the shell's iframe traffic to it (ADR 0016). Streamlit itself
// runs only inside those per-app containers, never in this process.
package main

import (
	"context"
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
	"github.com/projectbooth/booth-streamlit/internal/proxy"
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
	static, err := proxy.ParseStatic(cfg.StaticApps)
	if err != nil {
		return err
	}
	if len(static) > 0 {
		log.Printf("WARNING: serving %d app(s) from BOOTH_STREAMLIT_STATIC_APPS, a test seam that the app lifecycle replaces", len(static))
	}

	server := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: api.NewRouter(api.Deps{
			DB: pool, Bus: bus, Web: api.WebDir(cfg.WebDir),
			Verifier: verifier, Apps: svc,
			Proxy: &proxy.Handler{Verifier: verifier, Apps: proxy.Chain{static, proxy.AppsResolver{Apps: svc}}},
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
