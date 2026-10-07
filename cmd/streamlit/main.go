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
	"github.com/projectbooth/booth-streamlit/internal/config"
	"github.com/projectbooth/booth-streamlit/internal/db"
	"github.com/projectbooth/booth-streamlit/internal/events"
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

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.NewRouter(api.Deps{DB: pool, Bus: bus, Web: api.WebDir(cfg.WebDir)}),
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
