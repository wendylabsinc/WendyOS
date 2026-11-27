package node

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/proposalreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/taskhistory"
)

// Exercise the shared Sync entry point with all three repair channels present.
// A failure in proposal export must not suppress world or task-history progress.
func TestCombinedWorldProposalAndTaskHistoryRepair(t *testing.T) {
	r, operator, spec := nativeReviewFixture(t)
	r.now.Store(time.Now().UnixMilli())
	a := r.f
	exporter := missionCredentials(t, a, "device", "origin-gateway", "", "replicate", "read")
	a.n.cfg.ProposalExportCredentials = &exporter
	if err := a.n.configureProposalInbox(); err != nil {
		t.Fatal(err)
	}
	a.n.cfg.TaskStatusCredentials = relayStatusCredentials(t, a, "combined-history-status")
	spec.Proposal = nativeProposal(t, r, spec, `{"target":[1,0.2,0]}`)
	b, c := newFixture(t, a.root, "combined-b"), newFixture(t, a.root, "combined-c")
	for _, f := range []*fixture{a, b, c} {
		history, err := taskhistory.Open(filepath.Join(t.TempDir(), "history"), taskhistory.Config{
			Tenant: "tenant", Site: "office", Verifier: f.v,
			Origins: map[string]taskhistory.Origin{a.audience: {Principal: "robot-status", Assignee: r.cfg.Assignee, RunID: r.cfg.RunID}},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { history.Close() })
		f.n.cfg.TaskHistory = history
		f.n.cfg.ExportTaskHistory = f == a
		if err := f.n.configureTaskHistory(); err != nil {
			t.Fatal(err)
		}
		if f == a {
			continue
		}
		inbox, err := proposalreplica.Open(filepath.Join(t.TempDir(), "inbox"), proposalreplica.Config{
			Tenant: "tenant", Site: "office", Verifier: f.v,
			Origins: map[string]proposalreplica.Origin{a.audience: {Principal: exporter.Grant.Grant.Principal, Assignee: r.cfg.Assignee, RunID: r.cfg.RunID, Catalog: r.cfg.Catalog}},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { inbox.Close() })
		f.n.cfg.ProposalInbox = inbox
	}
	code, reply := call(t, a, operator, "POST", "/v1/tasks", "mission", instructionBody(t, a, operator, "mission", spec))
	if code != 200 {
		t.Fatalf("acceptance: %d %s", code, reply)
	}
	epoch := allocate(t, a)
	publish(t, a, component(a, epoch, 1))
	ctx := context.Background()
	pull := func(to, from *fixture) error {
		return to.n.Sync(ctx, Peer{URL: from.server.URL, Audience: from.audience}, to.credentials)
	}
	check := func(f *fixture, sequence uint64, minimumEvents int) {
		t.Helper()
		world := query(t, f)
		if len(world.Components) != 1 || world.Components[0].Version.Sequence != sequence {
			t.Fatalf("world repair: %+v", world)
		}
		proposal, err := f.n.cfg.ProposalInbox.Get(ctx, a.audience, spec.Proposal.ID)
		if err != nil || proposal.Record.BodySHA256 != spec.Proposal.BodySHA256 || proposal.Executable {
			t.Fatalf("proposal repair: %+v %v", proposal, err)
		}
		history, err := f.n.cfg.TaskHistory.List(ctx, a.audience, spec.ID, 100, "")
		if err != nil || len(history.Events) < minimumEvents || !history.Historical {
			t.Fatalf("history repair: %+v %v", history, err)
		}
		for _, event := range history.Events {
			if event.Executable {
				t.Fatal("history became executable")
			}
		}
		if f.n.cfg.Tasks != nil || f.n.cfg.Proposals != nil {
			t.Fatal("reader gained an authoritative task/proposal store")
		}
	}
	if err := pull(b, a); err != nil {
		t.Fatal(err)
	}
	if err := pull(c, b); err != nil {
		t.Fatal(err)
	}
	check(b, 1, 1)
	check(c, 1, 1)
	// Revoke only the export configuration, preserving previously imported notes.
	// The HTTP route refuses proposal export while the other two routes remain valid.
	a.n.cfg.ProposalExportCredentials = nil
	publish(t, a, component(a, epoch, 2))
	code, reply = call(t, a, operator, "POST", "/v1/tasks/stop", "stop", instructionBody(t, a, operator, "stop", StopRequest{TaskID: spec.ID}))
	if code != 200 {
		t.Fatalf("stop: %d %s", code, reply)
	}
	if err := pull(b, a); err == nil {
		t.Fatal("proposal export failure was hidden")
	}
	check(b, 2, 2)
	if err := pull(c, b); err != nil {
		t.Fatal(err)
	}
	check(c, 2, 2)
}
