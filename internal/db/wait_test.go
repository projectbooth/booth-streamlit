package db

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// fast shortens the backoff for a test and captures the retry log.
func fast(t *testing.T) *bytes.Buffer {
	t.Helper()
	ib, mb := initialBackoff, maxBackoff
	initialBackoff, maxBackoff = 10*time.Millisecond, 40*time.Millisecond
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { initialBackoff, maxBackoff = ib, mb; log.SetOutput(os.Stderr) })
	return &buf
}

// refusedErr is what pgx returns for a refused dial: a net.OpError wrapping ECONNREFUSED.
var refusedErr = fmt.Errorf("failed to connect: %w", &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)})

func TestRetry_FailsThenSucceeds(t *testing.T) {
	logs := fast(t)
	attempts := 0
	err := Retry(context.Background(), "pinging postgres", time.Minute, func(context.Context) error {
		attempts++
		if attempts <= 3 {
			return refusedErr
		}
		return nil
	})
	if err != nil || attempts != 4 {
		t.Fatalf("err = %v, attempts = %d; want nil after 4", err, attempts)
	}
	// Every retry logged: what, the attempt, the error and the delay.
	for _, want := range []string{
		"pinging postgres: not reachable yet (attempt 1: failed to connect: dial tcp: connect: connection refused); retrying in 10ms",
		"(attempt 2: ", "retrying in 20ms", "(attempt 3: ", "retrying in 40ms",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
}

func TestRetry_NeverSucceedsFailsLoudlyAtTheBound(t *testing.T) {
	fast(t)
	attempts := 0
	start := time.Now()
	err := Retry(context.Background(), "pinging postgres", 100*time.Millisecond, func(context.Context) error {
		attempts++
		return refusedErr
	})
	if err == nil {
		t.Fatal("Retry succeeded with fn always failing")
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("err = %v, want the last error wrapped", err)
	}
	if want := fmt.Sprintf("still failing after 100ms (%d attempts)", attempts); !strings.Contains(err.Error(), want) || attempts < 2 {
		t.Errorf("err = %q, attempts = %d; want %q", err, attempts, want)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %s, want about the 100ms bound", d)
	}
}

func TestRetry_NonRetryableFailsAtOnce(t *testing.T) {
	logs := fast(t)
	for _, code := range []string{"28P01", "28000", "3D000", "42P01"} { // auth, auth, no database, SQL
		attempts := 0
		pgErr := &pgconn.PgError{Code: code, Message: "nope"}
		err := Retry(context.Background(), "pinging postgres", time.Minute, func(context.Context) error {
			attempts++
			return fmt.Errorf("failed to connect: %w", pgErr)
		})
		if attempts != 1 || !errors.Is(err, pgErr) || !strings.Contains(err.Error(), "not retried (attempt 1)") {
			t.Errorf("%s: attempts = %d, err = %v; want one attempt and the error", code, attempts, err)
		}
	}
	if logs.Len() != 0 {
		t.Errorf("a non-retryable error logged a retry:\n%s", logs)
	}
}

func TestRetry_ContextCancelledMidWait(t *testing.T) {
	fast(t)
	initialBackoff = 10 * time.Second // the cancel must cut the wait short
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	err := Retry(ctx, "pinging postgres", time.Minute, func(context.Context) error {
		attempts++
		return refusedErr
	})
	if !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatalf("err = %v, attempts = %d; want context.Canceled after 1", err, attempts)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %s to notice the cancel", d)
	}
}

func TestRetryable(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"refused", refusedErr, true},
		{"dns", &net.DNSError{Err: "no such host", Name: "db"}, true},
		{"attempt timeout", fmt.Errorf("ping: %w", context.DeadlineExceeded), true},
		{"starting up 57P03", &pgconn.PgError{Code: "57P03"}, true},
		{"bad password 28P01", &pgconn.PgError{Code: "28P01"}, false},
		{"no database 3D000", &pgconn.PgError{Code: "3D000"}, false},
		{"sql error", &pgconn.PgError{Code: "42601"}, false},
		{"unknown", errors.New("tls: bad certificate"), false},
	} {
		if got := retryable(c.err); got != c.want {
			t.Errorf("%s: retryable = %v, want %v", c.name, got, c.want)
		}
	}
}

// A real refused socket (nothing listens on port 1) is classified as retryable through pgx's own
// error wrapping, and Open gives up at its bound with the attempt count.
func TestOpen_RefusedRetriesThenFails(t *testing.T) {
	fast(t)
	_, err := open(context.Background(), "postgres://booth:x@127.0.0.1:1/none?sslmode=disable&connect_timeout=2", 200*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "still failing after 200ms") {
		t.Fatalf("err = %v, want a retried refusal", err)
	}
}

// Against the real Postgres, a wrong password is not retried.
func TestOpen_WrongPasswordFailsAtOnce(t *testing.T) {
	logs := fast(t)
	u, err := url.Parse(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(u.User.Username(), "wrong-password")
	_, err = open(context.Background(), u.String(), time.Minute)
	if err == nil || !strings.Contains(err.Error(), "not retried (attempt 1)") || !strings.Contains(err.Error(), "28P01") {
		t.Fatalf("err = %v, want a single attempt failing on authentication (28P01)", err)
	}
	if logs.Len() != 0 {
		t.Errorf("a wrong password was retried:\n%s", logs)
	}
}
