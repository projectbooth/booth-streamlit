package db

import (
	"context"
	"os"
	"testing"
	"time"
)

// testDSN returns the real Postgres from hack/docker-compose.emulators.yml. Without it the test
// skips locally, but CI sets BOOTH_TEST_REQUIRE_EMULATORS so a missing database fails the build
// instead of passing green with nothing exercised.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("BOOTH_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("BOOTH_TEST_REQUIRE_EMULATORS") == "1" {
			t.Fatal("BOOTH_TEST_POSTGRES_DSN is not set but BOOTH_TEST_REQUIRE_EMULATORS=1")
		}
		t.Skip("BOOTH_TEST_POSTGRES_DSN not set; see hack/docker-compose.emulators.yml")
	}
	return dsn
}

func TestOpen_RealPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := Open(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var one int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("SELECT 1 = %d, %v", one, err)
	}
}

func TestOpen_UnreachableFailsFast(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Port 1 on loopback: nothing listens there, so the ping must fail rather than hang.
	if _, err := Open(ctx, "postgres://booth:x@127.0.0.1:1/none?sslmode=disable&connect_timeout=2"); err == nil {
		t.Fatal("Open succeeded against a closed port")
	}
}
