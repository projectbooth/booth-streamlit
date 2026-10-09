package events

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/projectbooth/booth-streamlit/internal/apps"
	"github.com/projectbooth/booth-streamlit/internal/identity"
)

// jsServer runs a real nats-server with JetStream, its store in dir (so a restart keeps the stream,
// as booth-core's file-backed stream does), and creates booth-core's BOOTH_EVENTS stream on first
// start.
func jsServer(t *testing.T, port int, dir string, createStream bool) *natsserver.Server {
	t.Helper()
	srv, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: port, JetStream: true, StoreDir: dir, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats-server did not start")
	}
	if createStream {
		nc, err := nats.Connect(srv.ClientURL())
		if err != nil {
			t.Fatal(err)
		}
		defer nc.Close()
		js, _ := jetstream.New(nc)
		if _, err := js.CreateOrUpdateStream(context.Background(), jetstream.StreamConfig{
			Name: "BOOTH_EVENTS", Subjects: []string{"booth.>"}, Storage: jetstream.FileStorage,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return srv
}

// streamMessages reads everything in BOOTH_EVENTS: subject and envelope.
func streamMessages(t *testing.T, url string) []map[string]any {
	t.Helper()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, _ := jetstream.New(nc)
	ctx := context.Background()
	st, err := js.Stream(ctx, "BOOTH_EVENTS")
	if err != nil {
		t.Fatal(err)
	}
	info, _ := st.Info(ctx)
	var out []map[string]any
	for seq := uint64(1); seq <= info.State.LastSeq; seq++ {
		m, err := st.GetMsg(ctx, seq)
		if err != nil {
			continue
		}
		var env map[string]any
		_ = json.Unmarshal(m.Data, &env)
		env["_subject"] = m.Subject
		out = append(out, env)
	}
	return out
}

var owner = identity.Caller{Subject: "alice", Workspace: "acme", Role: identity.RoleOwner}

func waitConnected(t *testing.T, b *Bus) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !b.Connected() {
		if time.Now().After(deadline) {
			t.Fatal("bus never connected")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// End to end against real JetStream: a shared app's events reach booth-core's stream with the
// ADR 0046 envelope; while the bus is down nothing is attempted and the events wait in the outbox;
// after the bus comes back (same stream, as booth-core's file storage keeps it) they arrive.
func TestDrainer_PublishesAndSurvivesAnOutage(t *testing.T) {
	port, dir := freePort(t), t.TempDir()
	srv := jsServer(t, port, dir, true)
	url := srv.ClientURL()

	store := apps.NewMemoryStore()
	svc := apps.NewService(store, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bus := New(Config{URL: url})
	go bus.Run(ctx)
	waitConnected(t, bus)
	d := &Drainer{Store: store, Bus: bus}

	a, err := svc.Create(ctx, owner, apps.Input{Name: "Sales", Source: "x", Sources: []string{"ds-1"}, Shared: true})
	if err != nil {
		t.Fatal(err)
	}
	if n := d.Pass(ctx); n != 1 {
		t.Fatalf("published %d, want 1", n)
	}
	msgs := streamMessages(t, url)
	if len(msgs) != 1 || msgs[0]["_subject"] != "booth.acme.dashboard.created" || msgs[0]["publishedBy"] != "streamlit" {
		t.Fatalf("stream %v", msgs)
	}
	data := msgs[0]["data"].(map[string]any)
	if data["dashboardId"] != a.ID || data["path"] != "/streamlit" || data["lineageComplete"] != false {
		t.Errorf("data %v", data)
	}

	// The bus goes away; a change is made; nothing is attempted, nothing is lost.
	srv.Shutdown()
	srv.WaitForShutdown()
	deadline := time.Now().Add(10 * time.Second)
	for bus.Connected() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := svc.Update(ctx, owner, a.ID, apps.Input{Name: "Sales v2", Source: "x", Sources: []string{"ds-1"}, Shared: true}); err != nil {
		t.Fatal(err)
	}
	if n := d.Pass(ctx); n != 0 {
		t.Fatalf("published %d with the bus down", n)
	}
	rows := store.Outbox()
	if last := rows[len(rows)-1]; last.Attempts != 0 {
		t.Errorf("an outage cost the row %d attempts", last.Attempts)
	}
	if st, _ := store.OutboxStats(ctx); st.Pending != 1 || st.Failed != 0 {
		t.Errorf("stats during the outage %+v", st)
	}

	// The bus comes back at the same address with its stream; the event arrives.
	srv2 := jsServer(t, port, dir, false)
	defer srv2.Shutdown()
	waitConnected(t, bus)
	if n := d.Pass(ctx); n != 1 {
		t.Fatalf("published %d after the outage, want 1", n)
	}
	msgs = streamMessages(t, url)
	if len(msgs) != 2 || msgs[1]["_subject"] != "booth.acme.dashboard.updated" || msgs[1]["data"].(map[string]any)["name"] != "Sales v2" {
		t.Fatalf("stream after the outage %v", msgs)
	}
}

// fakeBus is connected and fails every publish with err.
type fakeBus struct {
	mu    sync.Mutex
	err   error
	calls int
}

func (f *fakeBus) Connected() bool { return true }
func (f *fakeBus) Publish(context.Context, string, []byte, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.err
}

// A row that can never be published (connected, but the bus refuses it every time) is retried
// with back-off, marked failed after MaxAttempts, logged, and never offered again; the row after
// it still goes out.
func TestDrainer_PoisonRowIsBoundedAndVisible(t *testing.T) {
	clock := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	store := apps.NewMemoryStore()
	store.Now = func() time.Time { return clock }
	svc := apps.NewService(store, 0)
	ctx := context.Background()
	if _, err := svc.Create(ctx, owner, apps.Input{Name: "Poison", Source: "x", Shared: true}); err != nil {
		t.Fatal(err)
	}
	bus := &fakeBus{err: errors.New("nats: maximum payload exceeded")}
	var logs []string
	d := &Drainer{Store: store, Bus: bus, MaxAttempts: 3, Logf: func(f string, a ...any) { logs = append(logs, f) }}
	for i := 0; i < 20; i++ {
		d.Pass(ctx)
		clock = clock.Add(10 * time.Minute) // past any back-off
	}
	if bus.calls != 3 {
		t.Fatalf("the poison row was attempted %d times, want exactly MaxAttempts (3)", bus.calls)
	}
	st, _ := store.OutboxStats(ctx)
	if st.Failed != 1 || st.Pending != 0 || st.LastError == "" {
		t.Errorf("stats %+v: want it counted as failed, with its error", st)
	}
	gaveUp := false
	for _, l := range logs {
		if len(l) > 18 && l[:18] == "events: giving up " {
			gaveUp = true
		}
	}
	if !gaveUp {
		t.Errorf("no log line saying the row was given up on: %v", logs)
	}

	// The next row still publishes.
	bus.err = nil
	if _, err := svc.Create(ctx, owner, apps.Input{Name: "Fine", Source: "x", Shared: true}); err != nil {
		t.Fatal(err)
	}
	if n := d.Pass(ctx); n != 1 {
		t.Fatalf("published %d after the poison row, want 1", n)
	}
}
