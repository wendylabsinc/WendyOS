package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/people"
	"github.com/wendylabsinc/wendy/go/world/internal/presence"
	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/internal/zonepub"
)

type catalogFixture struct {
	m         *catalogMonitor
	mu        sync.Mutex
	reply     zonepub.Reply
	status    int
	root, key ed25519.PrivateKey
	grant     authority.SignedGrant
	trust     catalogTrust
	trustPath string
}

func catalogTest(t *testing.T) *catalogFixture {
	t.Helper()
	root, rk, _ := ed25519.GenerateKey(rand.Reader)
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	source := world.Source{Device: "a", App: "world", Service: "zones", Stream: "catalog"}
	g, e := authority.SignGrant(authority.Grant{Version: authority.Version, ID: "operator", PublishAudience: "a", Tenant: "tenant", Site: "office", Domain: "simulation", Principal: "operator", Kind: "operator", Source: source, PublicKey: pub, Scopes: []string{"publish", "read", "site-publish"}, NotBeforeUnixMS: now.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: now.Add(time.Hour).UnixMilli()}, rk)
	if e != nil {
		t.Fatal(e)
	}
	rev, e := authority.SignRevocations(authority.Revocations{Version: authority.Version, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(59 * time.Minute).UnixMilli()}, rk)
	if e != nil {
		t.Fatal(e)
	}
	d := zonepub.Catalog{SchemaVersion: zonepub.Type, Name: "office", Revision: "v1", Site: "office", RunID: "run", Frame: world.FrameRef{ID: "floor", Revision: "1"}, ValidFrom: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour), Zones: []presence.Zone{{ID: "room", MinXZ: [2]float64{-2, -2}, MaxXZ: [2]float64{2, 2}, DwellSeconds: 1}}}
	raw, _ := json.Marshal(d)
	component, e := zonepub.Build(raw, sitepub.Config{Tenant: "tenant", Site: "office", RunID: "run", Source: source, Epoch: 1, Sequence: 1, Now: now, TTL: 30 * time.Minute})
	if e != nil {
		t.Fatal(e)
	}
	record, e := authority.SignRecord(g, key, component, now)
	if e != nil {
		t.Fatal(e)
	}
	f := &catalogFixture{reply: zonepub.Reply{Records: []authority.SignedRecord{record}}, status: 200, root: rk, key: key, grant: g, trust: catalogTrust{Root: root, Revocations: rev}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.WriteHeader(f.status)
		json.NewEncoder(w).Encode(f.reply)
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	f.trustPath = filepath.Join(dir, "trust.json")
	write := func(name string, value any) string {
		path := filepath.Join(dir, name)
		raw, _ := json.Marshal(value)
		if e := os.WriteFile(path, raw, 0600); e != nil {
			t.Fatal(e)
		}
		return path
	}
	credentials := write("credentials.json", client.Config{URL: server.URL, Audience: "a", Grant: g, PrivateKey: key, AllowInsecureSimulation: true})
	write("trust.json", f.trust)
	sourcePath := write("source.json", source)
	p, _ := zonepub.Validate(component)
	f.m, e = openCatalog(credentials, f.trustPath, p.SHA256, sourcePath)
	if e != nil {
		t.Fatal(e)
	}
	return f
}
func TestCatalogSessionPinsRevisionAndLabelsPartition(t *testing.T) {
	f := catalogTest(t)
	m := f.m
	req := people.Request{ParticipantID: "people", RunID: "run", Frame: world.FrameRef{ID: "floor", Revision: "1"}}
	if e := m.bind(&req); e != nil || len(req.Zones) != 1 {
		t.Fatal(e)
	}
	wrong := req
	wrong.Frame.Revision = "2"
	if e := m.bind(&wrong); e == nil {
		t.Fatal("wrong frame accepted")
	}
	wrong = req
	wrong.RunID = "other"
	if e := m.bind(&wrong); e == nil {
		t.Fatal("wrong run accepted")
	}
	wrong = req
	wrong.Zones = append([]presence.Zone(nil), req.Zones...)
	wrong.Zones[0].MaxXZ[0] = 4
	if e := m.bind(&wrong); e == nil {
		t.Fatal("changed session geometry")
	}
	if !m.check() {
		t.Fatal(m.status)
	}
	f.mu.Lock()
	f.status = 503
	f.mu.Unlock()
	if !m.check() || m.status.State != "cached_snapshot" || m.status.CurrentRemoteState != "unknown" {
		t.Fatal(m.status)
	}
	f.mu.Lock()
	f.status = 200
	f.mu.Unlock()
	if !m.check() || m.status.State != "available_at_replica" {
		t.Fatal(m.status)
	}
	old := m.selected.Record
	tomb, e := zonepub.Delete(old.Component, 2, time.Now(), time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	r, e := authority.SignRecord(f.grant, f.key, tomb, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	f.reply.Records = []authority.SignedRecord{r}
	f.mu.Unlock()
	if m.check() || m.status.State != "deleted_at_replica" {
		t.Fatal(m.status)
	}
	f.mu.Lock()
	f.reply.Records = []authority.SignedRecord{old}
	f.mu.Unlock()
	if m.check() {
		t.Fatal("deleted session reopened")
	}
}
func TestCatalogAccessRevocationExpiryAndTrustChangesLatchClosed(t *testing.T) {
	for _, which := range []string{"denial", "revocation", "expiry", "root", "missing"} {
		t.Run(which, func(t *testing.T) {
			f := catalogTest(t)
			m := f.m
			switch which {
			case "denial":
				f.mu.Lock()
				f.status = 403
				f.mu.Unlock()
			case "missing":
				f.mu.Lock()
				f.reply.Records = []authority.SignedRecord{}
				f.mu.Unlock()
			case "expiry":
				c := m.selected.Record.Component
				c.Timing.SampleTime = time.Now()
				c.TTL = 20 * time.Millisecond
				r, e := authority.SignRecord(f.grant, f.key, c, time.Now())
				if e != nil {
					t.Fatal(e)
				}
				m.selected.Record = r
				time.Sleep(25 * time.Millisecond)
			case "root":
				f.trust.Root = make([]byte, 32)
				raw, _ := json.Marshal(f.trust)
				os.WriteFile(f.trustPath, raw, 0600)
			case "revocation":
				r := f.trust.Revocations.Revocations
				r.Revision++
				r.Principals = []string{"operator"}
				r.IssuedAtUnixMS = time.Now().UnixMilli()
				var e error
				f.trust.Revocations, e = authority.SignRevocations(r, f.root)
				if e != nil {
					t.Fatal(e)
				}
				raw, _ := json.Marshal(f.trust)
				os.WriteFile(f.trustPath, raw, 0600)
			}
			if m.check() || !m.terminal {
				t.Fatal("invalid catalog stayed current", m.status)
			}
		})
	}
}
func TestCatalogServeRejectsGeometryChangeWithoutResettingHistory(t *testing.T) {
	f := catalogTest(t)
	req := people.Request{ParticipantID: "people", RunID: "run", Frame: world.FrameRef{ID: "floor", Revision: "1"}, NowNS: 1}
	first, _ := json.Marshal(req)
	req.Zones = []presence.Zone{{ID: "different", MinXZ: [2]float64{-1, -1}, MaxXZ: [2]float64{1, 1}, DwellSeconds: 1}}
	bad, _ := json.Marshal(req)
	var out bytes.Buffer
	if e := serveCatalog(bytes.NewReader(append(append(first, '\n'), append(bad, '\n')...)), &out, f.m); e != nil {
		t.Fatal(e)
	}
	lines := bytes.Split(bytes.TrimSpace(out.Bytes()), []byte{'\n'})
	if len(lines) != 2 || !bytes.Contains(lines[0], []byte(`"zone_catalog"`)) || !bytes.Contains(lines[1], []byte(`"error"`)) {
		t.Fatal(out.String())
	}
}

func TestCatalogFetchCannotReserveAuthorityAcrossExpiry(t *testing.T) {
	for _, status := range []int{200, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := catalogTest(t)
			m := f.m
			c := m.selected.Record.Component
			c.Timing.SampleTime = time.Now()
			c.TTL = 80 * time.Millisecond
			r, e := authority.SignRecord(f.grant, f.key, c, time.Now())
			if e != nil {
				t.Fatal(e)
			}
			m.selected.Record = r
			f.mu.Lock()
			f.status = status
			go func() { time.Sleep(150 * time.Millisecond); f.mu.Unlock() }()
			if m.check() || m.status.State != "expired_or_revoked" {
				t.Fatal("fetch outlived pinned authority", m.status)
			}
		})
	}
}
