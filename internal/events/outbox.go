package events

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/projectbooth/booth-streamlit/internal/apps"
)

// Outbox is where the app model leaves its dashboard events (apps.Store).
type Outbox interface {
	PublishNext(ctx context.Context, maxAttempts int, publish func(apps.OutboxRow) error) (found bool, err error)
	OutboxStats(ctx context.Context) (apps.OutboxStats, error)
	PruneOutbox(ctx context.Context, olderThan time.Duration) error
}

// Publisher is the bus (*Bus).
type Publisher interface {
	Connected() bool
	Publish(ctx context.Context, subject string, data []byte, msgID string) error
}

// Drainer publishes the outbox to the bus, at least once (docs/design-data-access.md item 6).
//
//   - Rows are published oldest first, each with a JetStream acknowledgement; a row is marked
//     published only after its ack, so a crash in between republishes it (and JetStream's
//     de-duplication by message id usually absorbs that). booth-catalog's upsert and tombstone
//     rules make a duplicate harmless anyway (ADR 0046).
//   - Nothing is attempted while the bus is disconnected, so an outage of any length costs no
//     attempts: the rows wait in the database and go out when it is back, including across a
//     backend restart.
//   - A row that fails while connected is retried with back-off, and after MaxAttempts is marked
//     failed for good, logged, and counted on /healthz: a poison row never publishes forever and
//     never holds up the rows after it.
type Drainer struct {
	Store          Outbox
	Bus            Publisher
	MaxAttempts    int           // default 20 (with the 5-minute back-off cap, over an hour of trying)
	Interval       time.Duration // between passes; default 1s
	PublishTimeout time.Duration // per publish; default 10s
	KeepPublished  time.Duration // published rows are pruned after this; default 7 days
	Logf           func(format string, args ...any)
}

func (d *Drainer) defaults() {
	if d.MaxAttempts <= 0 {
		d.MaxAttempts = 20
	}
	if d.Interval <= 0 {
		d.Interval = time.Second
	}
	if d.PublishTimeout <= 0 {
		d.PublishTimeout = 10 * time.Second
	}
	if d.KeepPublished <= 0 {
		d.KeepPublished = 7 * 24 * time.Hour
	}
	if d.Logf == nil {
		d.Logf = log.Printf
	}
}

// Run drains until ctx ends.
func (d *Drainer) Run(ctx context.Context) {
	d.defaults()
	t := time.NewTicker(d.Interval)
	defer t.Stop()
	lastPrune := time.Time{}
	for {
		d.Pass(ctx)
		if time.Since(lastPrune) > time.Hour {
			if err := d.Store.PruneOutbox(ctx, d.KeepPublished); err != nil {
				d.Logf("events: pruning the outbox: %v", err)
			}
			lastPrune = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Pass publishes every due row, stopping at the first failure (the bus is likely unwell; the next
// pass tries again). It returns how many rows it published.
func (d *Drainer) Pass(ctx context.Context) int {
	d.defaults()
	n := 0
	for ctx.Err() == nil && d.Bus.Connected() {
		var perr error
		found, err := d.Store.PublishNext(ctx, d.MaxAttempts, func(r apps.OutboxRow) error {
			perr = d.publish(ctx, r)
			if perr != nil && r.Attempts+1 >= d.MaxAttempts {
				d.Logf("events: giving up on %s for app %s in %s (outbox row %d) after %d attempts: %v; it stays in the outbox, marked failed",
					r.Type, r.AppID, r.Workspace, r.ID, r.Attempts+1, perr)
			}
			return perr
		})
		switch {
		case err != nil:
			d.Logf("events: reading the outbox: %v", err)
			return n
		case !found:
			return n
		case perr != nil:
			d.Logf("events: publishing failed, will retry: %v", perr)
			return n
		}
		n++
	}
	return n
}

func (d *Drainer) publish(ctx context.Context, r apps.OutboxRow) error {
	env, err := apps.Envelope(r)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, d.PublishTimeout)
	defer cancel()
	msgID := fmt.Sprintf("streamlit-%s-%d-%d", r.Workspace, r.ID, r.CreatedAt.UnixNano())
	return d.Bus.Publish(ctx, apps.Subject(r), env, msgID)
}
