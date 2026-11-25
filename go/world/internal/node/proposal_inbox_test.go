package node

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/proposalreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/proposals"
)

func TestProposalInboxThreeNodesKeepsOriginApprovalSeparate(t *testing.T) {
	r, operator, spec := nativeReviewFixture(t)
	// This integration uses real HTTP/export wall time, not the older fixture
	// clock pinned 100 ms ahead of the process.
	r.now.Store(time.Now().UnixMilli())
	a := r.f
	exporter := missionCredentials(t, a, "device", "origin-gateway", "", "replicate", "read")
	a.n.cfg.ProposalExportCredentials = &exporter
	if e := a.n.configureProposalInbox(); e != nil {
		t.Fatal(e)
	}
	spec.Proposal = nativeProposal(t, r, spec, `{"target":[1,0.2,0]}`)
	b, c := newFixture(t, a.root, "viewer-b"), newFixture(t, a.root, "viewer-c")
	setup := func(f *fixture) (string, proposalreplica.Config) {
		cfg := proposalreplica.Config{Tenant: "tenant", Site: "office", Verifier: f.v, Origins: map[string]proposalreplica.Origin{a.audience: {Principal: exporter.Grant.Grant.Principal, Assignee: r.cfg.Assignee, RunID: r.cfg.RunID, Catalog: r.cfg.Catalog}}}
		path := filepath.Join(t.TempDir(), "inbox")
		var e error
		f.n.cfg.ProposalInbox, e = proposalreplica.Open(path, cfg)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { f.n.cfg.ProposalInbox.Close() })
		return path, cfg
	}
	bpath, bcfg := setup(b)
	setup(c)
	ctx := context.Background()
	sync := func(to, from *fixture) {
		t.Helper()
		if e := to.n.SyncProposalInbox(ctx, Peer{URL: from.server.URL, Audience: from.audience}, to.credentials); e != nil {
			t.Fatal(e)
		}
	}
	sync(b, a)
	sync(c, b)
	a.unavailable.Store(true)
	if e := b.n.SyncProposalInbox(ctx, Peer{URL: a.server.URL, Audience: a.audience}, b.credentials); e == nil {
		t.Fatal("partition not observed")
	}
	b.n.cfg.ProposalInbox.Close()
	var e error
	b.n.cfg.ProposalInbox, e = proposalreplica.Open(bpath, bcfg)
	if e != nil {
		t.Fatal(e)
	}
	sync(c, b)
	for _, f := range []*fixture{b, c} {
		code, body := call(t, f, operator, "GET", "/v1/proposal-inbox?origin="+a.audience+"&id="+spec.Proposal.ID, "read", nil)
		if code != 200 {
			t.Fatal(code, string(body))
		}
		var view proposalreplica.View
		if json.Unmarshal(body, &view) != nil || view.Executable || view.Record.BodySHA256 != spec.Proposal.BodySHA256 || view.Progress.Through != 1 {
			t.Fatal(view)
		}
		code, _ = call(t, f, operator, "POST", "/v1/proposal-inbox", "mission", instructionBody(t, a, operator, "mission", spec))
		if code != http.StatusMethodNotAllowed {
			t.Fatal("inbox accepted mutation", code)
		}
		if f.n.cfg.Tasks != nil || f.n.cfg.Proposals != nil {
			t.Fatal("viewer gained execution or origin proposal store")
		}
	}
	tasks, e := a.n.cfg.Tasks.List()
	if e != nil || len(tasks) != 0 {
		t.Fatal("replication created task", tasks, e)
	}
	a.unavailable.Store(false)
	body := instructionBody(t, a, operator, "mission", spec)
	code, reply := call(t, a, operator, "POST", "/v1/tasks", "mission", body)
	if code != 200 {
		t.Fatal(code, string(reply))
	}
	code, again := call(t, a, operator, "POST", "/v1/tasks", "mission", body)
	if code != 200 {
		t.Fatal(code, string(again))
	}
	tasks, e = a.n.cfg.Tasks.List()
	if e != nil || len(tasks) != 1 {
		t.Fatal("approval not idempotent", tasks, e)
	}
	retained, e := a.n.cfg.Proposals.Get(spec.Proposal.ID)
	if e != nil {
		t.Fatal(e)
	}
	remote, e := c.n.cfg.ProposalInbox.Get(ctx, a.audience, spec.Proposal.ID)
	if e != nil || !reflect.DeepEqual(remote.Record, retained.Record) {
		t.Fatal("remote altered origin proof", e)
	}
	// Changing only a remote inbox never changes origin-side eligibility.
	r.now.Store(time.Now().Add(20 * time.Minute).UnixMilli())
	spec.ID = "expired-new-approval"
	code, _ = call(t, a, operator, "POST", "/v1/tasks", "mission", instructionBody(t, a, operator, "mission", spec))
	if code == 200 {
		t.Fatal("expired proposal gained new approval")
	}
}
func TestProposalExportRequiresCurrentOriginGrant(t *testing.T) {
	r := newProposalReviewFixture(t)
	q := r.request("one")
	code, body := reviewProposalPost(t, r, r.agent, jsonBody(t, q))
	if code != 201 {
		t.Fatal(code, string(body))
	}
	var v proposals.View
	if json.Unmarshal(body, &v) != nil {
		t.Fatal("bad view")
	}
	code, _ = call(t, r.f, r.f.credentials, "GET", "/v1/proposal-inbox/replica?origin="+r.f.audience+"&after=0", "replicate", nil)
	if code == 200 {
		t.Fatal("unconfigured export signer accepted")
	}
}
