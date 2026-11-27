package node

import (
	"context"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/presence"
	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
	"github.com/wendylabsinc/wendy/go/world/internal/store"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/internal/zonepub"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func zoneComponent(t *testing.T, f *fixture, max float64) world.Component {
	t.Helper()
	now := time.Now()
	d := zonepub.Catalog{SchemaVersion: zonepub.Type, Name: "office", Revision: "same-label", Site: f.cfg.Site, RunID: "live", Frame: world.FrameRef{ID: "floor", Revision: "1"}, ValidFrom: now.Add(-time.Minute).Truncate(time.Minute), ValidUntil: now.Add(time.Hour).Truncate(time.Minute), Zones: []presence.Zone{{ID: "room", MinXZ: [2]float64{0, 0}, MaxXZ: [2]float64{max, 2}, DwellSeconds: 2}}}
	raw, _ := json.Marshal(d)
	c, e := zonepub.Build(raw, sitepub.Config{Tenant: f.cfg.Tenant, Site: f.cfg.Site, RunID: "live", Source: f.credentials.Grant.Grant.Source, Epoch: allocate(t, f), Sequence: 1, Now: now, TTL: 30 * time.Minute})
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func zoneList(t *testing.T, f *fixture) []authority.SignedRecord {
	t.Helper()
	status, b := call(t, f, f.credentials, http.MethodGet, "/v1/zones", "read", nil)
	if status != 200 {
		t.Fatalf("zones: %d %s", status, b)
	}
	var r zonepub.Reply
	if e := json.Unmarshal(b, &r); e != nil {
		t.Fatal(e)
	}
	return r.Records
}
func zoneSync(t *testing.T, to, from *fixture) {
	t.Helper()
	if e := to.n.Sync(context.Background(), Peer{from.server.URL, from.audience}, to.credentials); e != nil {
		t.Fatal(e)
	}
}
func zoneRestart(t *testing.T, f *fixture) {
	t.Helper()
	f.server.Close()
	if e := f.s.Close(); e != nil {
		t.Fatal(e)
	}
	var e error
	f.s, e = store.Open(filepath.Join(filepath.Dir(f.cfg.ProofPath), "store.json"), f.wc)
	if e != nil {
		t.Fatal(e)
	}
	f.n, e = New(f.cfg, f.s, f.v)
	if e != nil {
		t.Fatal(e)
	}
	f.start(t)
}
func TestZoneThreeReplicaPartitionRestartDeleteReplay(t *testing.T) {
	a := newFixture(t, nil, "a")
	siteAdmin(t, a)
	b := newFixture(t, a.root, "b")
	c := newFixture(t, a.root, "c")
	for _, f := range []*fixture{b, c} {
		if e := f.s.SetFrame(sitepub.MetadataFrame()); e != nil {
			t.Fatal(e)
		}
	}
	first := zoneComponent(t, a, 2)
	original := signed(t, a, first)
	if _, e := a.n.apply(original, true); e != nil {
		t.Fatal(e)
	}
	zoneSync(t, b, a)
	zoneSync(t, c, b)
	if len(zoneList(t, c)) != 1 {
		t.Fatal("catalog not repaired")
	}
	second := zoneComponent(t, a, 3)
	if _, e := a.n.apply(signed(t, a, second), true); e != nil {
		t.Fatal(e)
	}
	a.unavailable.Store(true)
	if e := b.n.Sync(context.Background(), Peer{a.server.URL, a.audience}, b.credentials); e == nil {
		t.Fatal("partition did not block")
	}
	if len(zoneList(t, b)) != 1 {
		t.Fatal("partition invented revision")
	}
	zoneRestart(t, b)
	if len(zoneList(t, b)) != 1 {
		t.Fatal("restart lost revision")
	}
	a.unavailable.Store(false)
	zoneSync(t, b, a)
	zoneSync(t, c, b)
	if len(zoneList(t, c)) != 2 {
		t.Fatal("edit failed to retain both revisions")
	}
	deletion, e := zonepub.Delete(first, allocate(t, a), time.Now(), time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = a.n.apply(signed(t, a, deletion), true); e != nil {
		t.Fatal(e)
	}
	zoneSync(t, b, a)
	zoneSync(t, c, b)
	zoneRestart(t, c)
	if _, e = c.n.apply(original, false); e != nil {
		t.Fatal(e)
	}
	rows := zoneList(t, c)
	deleted, live := 0, 0
	for _, r := range rows {
		if r.Component.Tombstone {
			deleted++
		} else {
			live++
		}
	}
	if deleted != 1 || live != 1 {
		t.Fatalf("replay resurrected catalog: %d deleted %d live", deleted, live)
	}
	// Another source can publish identical geometry but cannot remove this key.
	siteAdmin(t, b)
	foreign := first
	foreign.Key.Source = b.credentials.Grant.Grant.Source
	foreign.Version.Epoch = allocate(t, b)
	foreign.Timing.SampleTime = time.Now()
	if _, e = b.n.apply(signed(t, b, foreign), true); e != nil {
		t.Fatal(e)
	}
	wrong := deletion
	wrong.Version.Epoch = allocate(t, b)
	if _, e = authority.SignRecord(b.credentials.Grant, b.credentials.PrivateKey, wrong, time.Now()); e == nil {
		t.Fatal("source deleted another publisher")
	}
	zoneSync(t, c, b)
	if len(zoneList(t, c)) != 3 {
		t.Fatal("independent publisher lost")
	}
}
func TestZoneAuthorityAndTamperAcrossEveryAdmission(t *testing.T) {
	a := newFixture(t, nil, "a")
	c := zoneComponent(t, a, 2)
	r := signed(t, a, c)
	for _, local := range []bool{true, false} {
		if _, e := a.n.apply(r, local); e == nil {
			t.Fatal("app published zone")
		}
	}
	if _, e := a.n.verifyRepairRecords([]authority.SignedRecord{r}); e == nil {
		t.Fatal("repair elevated app")
	}
	siteAdmin(t, a)
	c = zoneComponent(t, a, 2)
	r = signed(t, a, c)
	if _, e := a.n.apply(r, true); e != nil {
		t.Fatal(e)
	}
	c.Payload = json.RawMessage(`{}`)
	r = signed(t, a, c)
	if _, e := a.n.apply(r, false); e == nil {
		t.Fatal("invalid signed payload admitted")
	}
	if _, e := a.n.verifyRepairRecords([]authority.SignedRecord{r}); e == nil {
		t.Fatal("invalid payload repaired")
	}
	if len(zoneList(t, a)) != 1 {
		t.Fatal("invalid update changed prior state")
	}
	now := time.Now()
	rev, e := authority.SignRevocations(authority.Revocations{Version: authority.Version, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 2, IssuedAtUnixMS: now.UnixMilli(), ExpiresAtUnixMS: now.Add(time.Hour).UnixMilli(), Principals: []string{"a"}}, a.root)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.v.UpdateRevocations(rev); e != nil {
		t.Fatal(e)
	}
	reader := issueCredentials(t, a.root, "reader")
	status, body := call(t, a, reader, http.MethodGet, "/v1/zones", "read", nil)
	var reply zonepub.Reply
	if status != 200 || json.Unmarshal(body, &reply) != nil || len(reply.Records) != 0 {
		t.Fatal("revoked zone visible", status, string(body))
	}
}
