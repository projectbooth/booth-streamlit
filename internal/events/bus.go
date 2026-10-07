// Package events holds booth-streamlit's event-bus connection (NATS, ADR 0021), over which it
// publishes dashboard.created/updated/deleted for each app (ADR 0018, payload per ADR 0046).
//
// The scaffold only connects and reports its state through /healthz; the publisher arrives with
// the app model. Connecting now is still worth it: it proves end to end that the manifest's
// `events` declaration made booth-core mint a credential (ADR 0050) and that the credential works,
// which is the step that silently failed for booth-catalog before it declared `events`.
package events

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// State is the bus connection's observable state, reported verbatim on /healthz.
type State string

const (
	// StateDisabled: no bus URL configured, so nothing will ever be published.
	StateDisabled State = "disabled"
	// StateConnecting: not yet connected, or reconnecting after a drop.
	StateConnecting State = "connecting"
	// StateConnected: a live, authenticated connection.
	StateConnected State = "connected"
)

// Config says where the bus is and how to authenticate to it.
type Config struct {
	URL string
	// CredentialsFile is booth-core's minted ".creds" file (ADR 0050). nats.go re-reads it on
	// every connect and reconnect, so core's in-place renewal of the mounted Secret is picked up.
	CredentialsFile string
	// Name is the client name the server logs; defaults to "booth-streamlit".
	Name string
}

// Bus owns one NATS connection for the life of the process. The zero value is not usable; use
// New. It is safe for concurrent use.
type Bus struct {
	cfg Config

	mu    sync.Mutex
	state State
}

// New returns a Bus that has not connected yet. An empty URL yields a permanently disabled Bus.
func New(cfg Config) *Bus {
	if cfg.Name == "" {
		cfg.Name = "booth-streamlit"
	}
	b := &Bus{cfg: cfg, state: StateConnecting}
	if cfg.URL == "" {
		b.state = StateDisabled
	}
	return b
}

// State reports the current connection state.
func (b *Bus) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

func (b *Bus) setState(s State) {
	b.mu.Lock()
	b.state = s
	b.mu.Unlock()
}

// Run connects and keeps the connection alive until ctx ends, then closes it. It never returns an
// error: a bus outage, or booth-core not having written the credential yet, must not keep the
// module from serving apps. The state is visible through State instead.
func (b *Bus) Run(ctx context.Context) {
	if b.cfg.URL == "" {
		log.Print("BOOTH_NATS_URL is not set: dashboard events are disabled, so booth-catalog will never index any app")
		return
	}
	opts := []nats.Option{
		nats.Name(b.cfg.Name),
		// Keep trying forever, including the very first connect: the bus may come up after us.
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * time.Second),
		nats.ConnectHandler(func(*nats.Conn) { b.setState(StateConnected); log.Print("event bus connected") }),
		nats.ReconnectHandler(func(*nats.Conn) { b.setState(StateConnected); log.Print("event bus reconnected") }),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			b.setState(StateConnecting)
			if err != nil {
				log.Printf("event bus disconnected: %v", err)
			}
		}),
	}
	if b.cfg.CredentialsFile != "" {
		opts = append(opts, nats.UserCredentials(b.cfg.CredentialsFile))
	}
	nc, err := nats.Connect(b.cfg.URL, opts...)
	if err != nil {
		// With RetryOnFailedConnect this only happens for a configuration error (a malformed URL,
		// an unreadable credentials file), which retrying cannot fix.
		log.Printf("event bus: %v", err)
		return
	}
	if nc.IsConnected() {
		b.setState(StateConnected)
	}

	<-ctx.Done()
	nc.Close()
}
