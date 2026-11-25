package node

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/geography"
	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
	"github.com/wendylabsinc/wendy/go/world/internal/store"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func siteComponent(t *testing.T, f *fixture) world.Component {
	t.Helper()
	raw, e := os.ReadFile("../../examples/geography/office.json")
	if e != nil {
		t.Fatal(e)
	}
	var cfg geography.SiteConfig
	if e = json.Unmarshal(raw, &cfg); e != nil {
		t.Fatal(e)
	}
	cfg.Site = f.cfg.Site
	raw, e = json.Marshal(cfg)
	if e != nil {
		t.Fatal(e)
	}
	c, e := sitepub.Build(raw, sitepub.Config{Tenant: f.cfg.Tenant, Site: f.cfg.Site, RunID: "live", Source: f.credentials.Grant.Grant.Source, Epoch: allocate(t, f), Sequence: 1, Now: time.Now(), TTL: time.Minute})
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func siteAdmin(t *testing.T, f *fixture) {
	t.Helper()
	g := f.credentials.Grant.Grant
	g.Kind = "operator"
	g.Scopes = append(g.Scopes, "site-publish")
	grant, e := authority.SignGrant(g, f.root)
	if e != nil {
		t.Fatal(e)
	}
	f.credentials.Grant = grant
	if e = f.s.SetFrame(sitepub.MetadataFrame()); e != nil {
		t.Fatal(e)
	}
}
func siteList(t *testing.T, f *fixture) []authority.SignedRecord {
	t.Helper()
	status, b := call(t, f, f.credentials, http.MethodGet, "/v1/sites", "read", nil)
	if status != http.StatusOK {
		t.Fatal(status, string(b))
	}
	var result struct {
		Publications []authority.SignedRecord `json:"publications"`
	}
	if e := json.Unmarshal(b, &result); e != nil {
		t.Fatal(e)
	}
	return result.Publications
}
func TestSiteOriginAuthorityEnforcedOnLocalAndRelay(t *testing.T) {
	a := newFixture(t, nil, "a")
	c := siteComponent(t, a)
	r := signed(t, a, c)
	for _, local := range []bool{true, false} {
		if _, e := a.n.apply(r, local); e == nil {
			t.Fatal("ordinary publisher created site")
		}
	}
	if len(siteList(t, a)) != 0 {
		t.Fatal("rejected site became visible")
	}
	siteAdmin(t, a)
	c = siteComponent(t, a)
	r = signed(t, a, c)
	if _, e := a.n.apply(r, true); e != nil {
		t.Fatal(e)
	}
	if len(siteList(t, a)) != 1 {
		t.Fatal("authorized site missing")
	}
	// Even a valid operator signature cannot rename a document to another digest.
	c.Version.Sequence++
	c.Key.Entity = "site-georeference/" + string(make([]byte, 64))
	r = signed(t, a, c)
	if _, e := a.n.apply(r, false); e == nil {
		t.Fatal("false digest admitted")
	}
}
func TestSiteRepairsRestartsAndRevokesWithoutActivatingFrames(t *testing.T) {
	a := newFixture(t, nil, "a")
	siteAdmin(t, a)
	b := newFixture(t, a.root, "b")
	if e := b.s.SetFrame(sitepub.MetadataFrame()); e != nil {
		t.Fatal(e)
	}
	c := siteComponent(t, a)
	r := signed(t, a, c)
	if _, e := a.n.apply(r, true); e != nil {
		t.Fatal(e)
	}
	if e := b.n.Sync(context.Background(), Peer{a.server.URL, "a"}, b.credentials); e != nil {
		t.Fatal(e)
	}
	got := siteList(t, b)
	if len(got) != 1 {
		t.Fatal("site not repaired")
	}
	before, _ := json.Marshal(got[0].Component.Payload)
	b.server.Close()
	if e := b.s.Close(); e != nil {
		t.Fatal(e)
	}
	var e error
	b.s, e = store.Open(filepath.Join(filepath.Dir(b.cfg.ProofPath), "store.json"), b.wc)
	if e != nil {
		t.Fatal(e)
	}
	b.n, e = New(b.cfg, b.s, b.v)
	if e != nil {
		t.Fatal(e)
	}
	b.start(t)
	got = siteList(t, b)
	if len(got) != 1 {
		t.Fatal("site lost on reopen")
	}
	after, _ := json.Marshal(got[0].Component.Payload)
	if string(before) != string(after) {
		t.Fatal("site changed on reopen")
	}
	state, e := b.s.Records()
	if e != nil || len(state) != 1 {
		t.Fatal("unexpected state", e)
	}
	// A publication does not accept or rewrite the document's office frame.
	observation := component(b, allocate(t, b), 1)
	observation.Frame = world.FrameRef{ID: "office-floor", Revision: "office-frame-v1"}
	if _, e = b.n.apply(signed(t, b, observation), true); e != nil {
		t.Fatal(e)
	}
	if len(query(t, b).Components) != 1 {
		t.Fatal("publication autoactivated observation frame")
	}
	now := time.Now()
	rev, e := authority.SignRevocations(authority.Revocations{Version: authority.Version, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 2, IssuedAtUnixMS: now.Add(-time.Millisecond).UnixMilli(), ExpiresAtUnixMS: now.Add(20 * time.Minute).UnixMilli(), GrantIDs: []string{a.credentials.Grant.Grant.ID}}, a.root)
	if e != nil {
		t.Fatal(e)
	}
	if e = b.v.UpdateRevocations(rev); e != nil {
		t.Fatal(e)
	}
	if len(siteList(t, b)) != 0 {
		t.Fatal("revoked publication visible")
	}
	exported, e := b.n.export()
	if e != nil {
		t.Fatal(e)
	}
	for _, p := range exported {
		if p.Component.Key.Type == sitepub.Type {
			t.Fatal("revoked publication exported")
		}
	}
}
