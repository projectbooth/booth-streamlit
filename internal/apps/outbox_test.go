package apps

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

// drain publishes every due row, collecting them in order.
func drain(t *testing.T, s Store) []OutboxRow {
	t.Helper()
	var got []OutboxRow
	for {
		found, err := s.PublishNext(context.Background(), 5, func(r OutboxRow) error { got = append(got, r); return nil })
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			return got
		}
	}
}

func types(rows []OutboxRow) []string {
	out := []string{}
	for _, r := range rows {
		out = append(out, r.Type)
	}
	return out
}

// Plan item 6 / ADR 0046: only shared apps are dashboards. Sharing publishes created, a change to
// what the event carries publishes updated (re-owning included), a code-only edit publishes
// nothing, unsharing and deleting publish deleted. Each event is in the outbox once the change is
// committed, in order, with the database clock as publishedAt.
func TestOutbox_WhichChangesPublishWhat(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			drain(t, s) // anything left by other tests
			a := app("e1", "acme", "Sales", false)
			a.Owner = "alice"
			if err := s.Create(ctx, a); err != nil {
				t.Fatal(err)
			}
			if got := drain(t, s); len(got) != 0 {
				t.Fatalf("an unshared app published %v", types(got))
			}
			a.Shared, a.Sources = true, []string{"ds-1", "ds-2"}
			step := func(want ...string) []OutboxRow {
				t.Helper()
				got := drain(t, s)
				if !reflect.DeepEqual(types(got), append([]string{}, want...)) {
					t.Fatalf("published %v, want %v", types(got), want)
				}
				return got
			}
			if err := s.Update(ctx, a); err != nil {
				t.Fatal(err)
			}
			created := step(EventCreated)[0]
			var data map[string]any
			_ = json.Unmarshal(created.Data, &data)
			want := map[string]any{"dashboardId": "e1", "name": "Sales", "description": "", "owner": "alice", "path": "/streamlit",
				"lineageComplete": false, "sources": []any{map[string]any{"type": "dataset", "datasetId": "ds-1"}, map[string]any{"type": "dataset", "datasetId": "ds-2"}}}
			if !reflect.DeepEqual(data, want) {
				t.Errorf("created data %v\nwant %v", data, want)
			}
			env, _ := Envelope(created)
			var e map[string]any
			_ = json.Unmarshal(env, &e)
			if e["workspace"] != "acme" || e["eventType"] != EventCreated || e["publishedBy"] != "streamlit" || Subject(created) != "booth.acme.dashboard.created" {
				t.Errorf("envelope %v subject %s", e, Subject(created))
			}
			if _, err := time.Parse(time.RFC3339Nano, e["publishedAt"].(string)); err != nil || created.CreatedAt.IsZero() {
				t.Errorf("publishedAt %v: %v", e["publishedAt"], err)
			}

			a.Source = "print('only code changed')"
			_ = s.Update(ctx, a)
			step()
			a.Name = "Sales v2"
			_ = s.Update(ctx, a)
			upd := step(EventUpdated)[0]
			if !upd.CreatedAt.After(created.CreatedAt) {
				t.Errorf("publishedAt went backwards: %s then %s", created.CreatedAt, upd.CreatedAt)
			}
			if _, err := s.TakeOwnership(ctx, "acme", "e1", "bob", "test", time.Now()); err != nil {
				t.Fatal(err)
			}
			reowned := step(EventUpdated)[0]
			_ = json.Unmarshal(reowned.Data, &data)
			if data["owner"] != "bob" {
				t.Errorf("re-owned event's owner %v", data["owner"])
			}
			a.Sources = nil
			_ = s.Update(ctx, a)
			cleared := step(EventUpdated)[0]
			_ = json.Unmarshal(cleared.Data, &data)
			if src, ok := data["sources"].([]any); !ok || len(src) != 0 {
				t.Errorf("clearing sources must publish \"sources\": [], got %v", data["sources"])
			}
			a.Shared = false
			_ = s.Update(ctx, a)
			gone := step(EventDeleted)[0]
			if string(gone.Data) != `{"dashboardId":"e1"}` && string(gone.Data) != `{"dashboardId": "e1"}` {
				t.Errorf("deleted data %s", gone.Data)
			}
			a.Shared = true
			_ = s.Update(ctx, a)
			step(EventCreated)
			if err := s.Delete(ctx, "acme", "e1"); err != nil {
				t.Fatal(err)
			}
			step(EventDeleted)
		})
	}
}

// Delivery: a failed publish is retried after a back-off; a row that keeps failing is marked
// failed after maxAttempts and never published again (a poison row can't block or loop), and the
// failure is visible in the stats. A later row isn't held up by it.
func TestOutbox_RetriesAndPoison(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			drain(t, s)
			for _, id := range []string{"p1", "p2"} {
				a := app(id, "acme", id, true)
				if err := s.Create(ctx, a); err != nil {
					t.Fatal(err)
				}
			}
			boom := errors.New("nats: maximum payload exceeded")
			// p1 fails once (maxAttempts 1): failed for good. p2 then publishes.
			found, err := s.PublishNext(ctx, 1, func(r OutboxRow) error {
				if r.AppID != "p1" {
					t.Fatalf("oldest first: got %s", r.AppID)
				}
				return boom
			})
			if !found || err != nil {
				t.Fatalf("found %v err %v", found, err)
			}
			st, _ := s.OutboxStats(ctx)
			if st.Failed != 1 || st.Pending != 1 || st.LastError == "" {
				t.Errorf("stats %+v: want 1 failed, 1 pending, the error", st)
			}
			got := drain(t, s)
			if len(got) != 1 || got[0].AppID != "p2" {
				t.Fatalf("after the poison row: %v", got)
			}
			if again := drain(t, s); len(again) != 0 {
				t.Fatalf("a failed row was offered again: %v", again)
			}

			// A transient failure with attempts to spare: backed off, not offered at once, not failed.
			a := app("p3", "acme", "p3", true)
			_ = s.Create(ctx, a)
			_, _ = s.PublishNext(ctx, 5, func(OutboxRow) error { return errors.New("nats: timeout") })
			if again := drain(t, s); len(again) != 0 {
				t.Fatalf("a failed row was retried without a back-off: %v", again)
			}
			st, _ = s.OutboxStats(ctx)
			if st.Pending != 1 || st.Failed != 1 {
				t.Errorf("stats %+v", st)
			}
		})
	}
}
