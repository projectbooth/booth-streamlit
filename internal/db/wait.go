package db

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Startup retry for the first connection (ARCHITECTURE.md item 66 (c)): on the homelab a new pod's
// first connections to PostgreSQL are refused for about 3 seconds while k3s's kube-router adds the
// pod's IP to the NetworkPolicy allow sets, and the backend used to exit on that first refusal.
// The pattern is booth-core's internal/dbwait (copied, not imported: core's package is internal),
// narrowed to connection-level failures: a wrong password, a missing database or an SQL error is
// not a scheduling race and fails at once.

// DefaultMaxElapsed bounds the whole wait: well past the few-second window item 66 observed, short
// enough that a server that is really down still fails loudly within about a minute.
const DefaultMaxElapsed = 60 * time.Second

// Variables, not constants, only so tests can shorten them.
var (
	initialBackoff = 500 * time.Millisecond
	maxBackoff     = 8 * time.Second
	// attemptTimeout bounds one attempt, so a dropped (not refused) connection can't hang startup
	// when the DSN sets no connect_timeout.
	attemptTimeout = 10 * time.Second
)

// Retry calls fn (one connection attempt) until it succeeds, retrying connection-level failures
// with exponential backoff (500ms doubling to 8s) until maxElapsed has passed since the first
// attempt. Every retry is logged with what, the attempt, the error and the delay. Any other error
// is returned at once. A non-nil result is ctx's error, or fn's last error with the attempt count.
func Retry(ctx context.Context, what string, maxElapsed time.Duration, fn func(context.Context) error) error {
	deadline := time.Now().Add(maxElapsed)
	backoff := initialBackoff
	for attempt := 1; ; attempt++ {
		actx, cancel := context.WithTimeout(ctx, attemptTimeout)
		err := fn(actx)
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%s: %w (attempt %d: %v)", what, ctx.Err(), attempt, err)
		}
		if !retryable(err) {
			return fmt.Errorf("%s: failed, not retried (attempt %d): %w", what, attempt, err)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s: still failing after %s (%d attempts): %w", what, maxElapsed, attempt, err)
		}
		log.Printf("%s: not reachable yet (attempt %d: %v); retrying in %s", what, attempt, err, backoff)
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %w (after %d attempts, last: %v)", what, ctx.Err(), attempt, err)
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// retryable reports whether err is a connection-level failure: refused, reset or unreachable, a
// timeout, DNS, or a server that is still starting up (SQLSTATE 57P03). Any other server error
// (authentication 28P01/28000, missing database 3D000, any SQL error) and anything unrecognised is
// not.
func retryable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "57P03" // cannot_connect_now: "the database system is starting up"
	}
	var dnsErr *net.DNSError
	var opErr *net.OpError
	switch {
	case errors.As(err, &dnsErr), errors.As(err, &opErr):
		return true
	case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return true
	case errors.Is(err, context.DeadlineExceeded): // the attempt's own timeout (the caller's ctx is checked first)
		return true
	}
	return pgconn.Timeout(err)
}
