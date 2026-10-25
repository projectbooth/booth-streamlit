package dataaccess

import (
	"reflect"
	"testing"
	"time"
)

func uses(u *Uses, app, viewer string) []Use {
	out := []Use{}
	for _, x := range u.For(app, viewer) {
		out = append(out, x.Use)
	}
	return out
}

// The "Write allowed" prompt (ADR 0117 item 2): a successful write or upload is remembered for the
// viewer who made it; a refused one, or one with no viewer, isn't.
func TestUses_TheProxiesRecordASuccessfulWriteForItsViewer(t *testing.T) {
	wr := newWritesRig(t)
	wr.do(t, "POST", "/writes/household/rows", wr.a.GateBearer, `{"data":{}}`, map[string]string{"X-Booth-App-User": "bob"})
	wr.do(t, "POST", "/writes/household/rows", wr.a.GateBearer, `{"data":{}}`, nil)
	wr.do(t, "DELETE", "/writes/household/rows/1", wr.a.GateBearer, "", map[string]string{"X-Booth-App-User": "carol"}) // not declared
	wr.api.status, wr.api.body = 403, `{"error":{"code":"not_granted","message":"no"}}`
	wr.do(t, "PATCH", "/writes/household/rows/1", wr.a.GateBearer, `{"data":{}}`, map[string]string{"X-Booth-App-User": "dave"})
	if got := uses(wr.uses, wr.a.ID, "bob"); !reflect.DeepEqual(got, []Use{{Kind: "write", API: "household"}}) {
		t.Errorf("bob: %v", got)
	}
	for _, v := range []string{"", "carol", "dave"} {
		if got := uses(wr.uses, wr.a.ID, v); len(got) != 0 {
			t.Errorf("%q: %v", v, got)
		}
	}
	if got := uses(wr.uses, wr.b.ID, "bob"); len(got) != 0 {
		t.Errorf("another app: %v", got)
	}

	u := newUploadsRig(t, 0)
	u.do(t, "POST", "/uploads/local?prefix=finance/inbox/&name=a.csv", u.a.GateBearer, "x", map[string]string{"X-Booth-App-User": "bob"})
	u.storage.status = 412
	u.do(t, "POST", "/uploads/local?prefix=finance/inbox/&name=a.csv", u.a.GateBearer, "x", map[string]string{"X-Booth-App-User": "carol"})
	if got := uses(u.uses, u.a.ID, "bob"); !reflect.DeepEqual(got, []Use{{Kind: "upload", Backend: "local", Prefix: "finance/inbox/"}}) {
		t.Errorf("upload, bob: %v", got)
	}
	if got := uses(u.uses, u.a.ID, "carol"); len(got) != 0 {
		t.Errorf("refused upload, carol: %v", got)
	}
	if up, _ := u.storage.last(); up.Header.Get("X-Booth-App-User") != "" {
		t.Error("X-Booth-App-User was sent to booth-storage")
	}
}

func TestUses_ExpireAndStayBounded(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	u := &Uses{now: func() time.Time { return now }}
	u.Record("a1", "bob", Use{Kind: "write", API: "household"})
	now = now.Add(UseTTL - time.Minute)
	if len(u.For("a1", "bob")) != 1 {
		t.Fatal("forgotten too early")
	}
	now = now.Add(2 * time.Minute)
	if got := u.For("a1", "bob"); len(got) != 0 {
		t.Errorf("after the TTL: %v", got)
	}

	u = &Uses{now: func() time.Time { return now }}
	for i := 0; i < maxUsesPerViewerApp+5; i++ {
		u.Record("a1", "bob", Use{Kind: "write", API: string(rune('a'+i%26)) + string(rune('a'+i/26))})
	}
	if n := len(u.For("a1", "bob")); n != maxUsesPerViewerApp {
		t.Errorf("per viewer and app: %d targets kept", n)
	}
	for i := 0; i < maxUseKeys+1; i++ {
		now = now.Add(time.Millisecond)
		u.Record("a2", string(rune(0x4e00+i)), Use{Kind: "write", API: "x"})
	}
	if len(u.seen) > maxUseKeys {
		t.Errorf("%d (app, viewer) pairs kept", len(u.seen))
	}
	if len(u.For("a1", "bob")) != 0 {
		t.Error("the least recently used pair wasn't the one dropped")
	}
	var nilUses *Uses
	nilUses.Record("a", "b", Use{})
	if got := nilUses.For("a", "b"); got == nil || len(got) != 0 {
		t.Errorf("nil Uses: %v", got)
	}
}
