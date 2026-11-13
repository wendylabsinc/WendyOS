package commands

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// deployedFixture is stepsFixture with line-3 deployed to packer-01 at
// revision 1, running with the release's defaults.
func deployedFixture(t *testing.T) (*registryOp, *fakeRegistry, string, string) {
	t.Helper()
	op, fake, releaseID := stepsFixture(t)
	a := allFlags(releaseID)
	a.config, a.installOnly, a.run = nil, false, true
	if err := runDeploy(context.Background(), op, a, io.Discard, false); err != nil {
		t.Fatal(err)
	}
	for id := range fake.deploys {
		return op, fake, releaseID, id
	}
	return nil, nil, "", ""
}

// An update is one revision at expected_revision = the shown one; a new
// configuration reaches every device at the new revision, and a dropped
// device is sent its removal at that revision.
func TestUpdateSignsTheNextRevision(t *testing.T) {
	op, fake, _, depID := deployedFixture(t)
	ctx := context.Background()
	if err := runUpdate(ctx, op, "sh.wendy.example", "line-3", changeFlags{yes: true}, io.Discard, false); err == nil || !strings.Contains(err.Error(), "Nothing to change") {
		t.Fatalf("no change: %v", err)
	}
	if err := runUpdate(ctx, op, "sh.wendy.example", "line-3", changeFlags{config: []string{"LOG_LEVEL=debug"}}, io.Discard, false); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("unconfirmed: %v", err)
	}
	if err := runUpdate(ctx, op, "sh.wendy.example", "line-3", changeFlags{config: []string{"LOG_LEVEL=debug"}, yes: true}, io.Discard, false); err != nil {
		t.Fatal(err)
	}
	upd := fake.signed(cloudpb.DeploymentService_UpdateDeployment_FullMethodName)
	if len(upd) != 1 || upd[0].payload.(*cloudpb.UpdateDeploymentRequest).GetExpectedRevision() != 1 || upd[0].resource != "org/"+testOperatorTenant+"/deployment/"+depID {
		t.Fatalf("UpdateDeployment %+v", upd)
	}
	last := fake.assigned[len(fake.assigned)-1]
	if len(fake.assigned) != 2 || last.GetRevision() != 2 || !strings.Contains(last.GetConfigJson(), `"LOG_LEVEL":"debug"`) {
		t.Fatalf("statements %v", fake.assigned)
	}
	if err := runUpdate(ctx, op, "sh.wendy.example", "line-3", changeFlags{removeTags: []string{"line-3"}, addDevices: []string{"packer-01"}, yes: true}, io.Discard, false); err != nil {
		t.Fatal(err)
	}
	if err := runUpdate(ctx, op, "sh.wendy.example", "line-3", changeFlags{removeDevices: []string{"packer-01"}, yes: true}, io.Discard, false); err != nil {
		t.Fatal(err)
	}
	if len(fake.removed) != 1 || fake.removed[0].GetRevision() != 4 {
		t.Fatalf("removals %v", fake.removed)
	}
}

// A new release is sealed under a new deployment key, and the statements
// pin the new release's digest.
func TestUpdateToANewReleaseUsesANewKey(t *testing.T) {
	op, fake, _, _ := deployedFixture(t)
	newRelease, m := publishFixture(t, op)
	if err := runUpdate(context.Background(), op, "sh.wendy.example", "line-3", changeFlags{release: newRelease, yes: true}, io.Discard, false); err != nil {
		t.Fatal(err)
	}
	last := fake.assigned[len(fake.assigned)-1]
	if len(fake.sealed) != 2 || last.GetAppReleaseId() != newRelease || last.GetManifestDigest() != m.GetManifestDigest() || last.GetDeploymentKeyId() != fake.sealed[1].GetDeploymentKeyId() {
		t.Fatalf("statement %v", last)
	}
}

// A rollback shows the release and configuration it restores, and signs
// exactly that content at the new revision.
func TestRollbackShowsAndSignsTheRestoredRevision(t *testing.T) {
	op, fake, _, _ := deployedFixture(t)
	ctx := context.Background()
	if err := runUpdate(ctx, op, "sh.wendy.example", "line-3", changeFlags{config: []string{"LOG_LEVEL=debug"}, yes: true}, io.Discard, false); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runRollback(ctx, op, "sh.wendy.example", "line-3", changeFlags{to: 1, yes: true}, &out, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `Configuration: {"LOG_LEVEL":"info","UPLOAD_INTERVAL_SECONDS":"30"}`) || !strings.Contains(out.String(), "as revision 3") {
		t.Fatalf("review:\n%s", out.String())
	}
	last := fake.assigned[len(fake.assigned)-1]
	if last.GetRevision() != 3 || !strings.Contains(last.GetConfigJson(), `"LOG_LEVEL":"info"`) {
		t.Fatalf("restored %v", last)
	}
	n := len(fake.assigned)
	fake.tamper = func(d *cloudpb.Deployment) { d.ConfigJson = `{"LOG_LEVEL":"other"}` }
	if err := runRollback(ctx, op, "sh.wendy.example", "line-3", changeFlags{to: 2, yes: true}, io.Discard, false); err == nil || !strings.Contains(err.Error(), "differs") || len(fake.assigned) != n {
		t.Fatalf("cloud recorded other content: %v", err)
	}
}

// Remove sends each device a terminal removal at the maximum revision.
func TestRemoveSignsTerminalRemovals(t *testing.T) {
	op, fake, _, depID := deployedFixture(t)
	if err := runRemove(context.Background(), op, "sh.wendy.example", "line-3", changeFlags{yes: true}, io.Discard, false); err != nil {
		t.Fatal(err)
	}
	if len(fake.removed) != 1 || fake.removed[0].GetRevision() != maxRevision || fake.removed[0].GetDeploymentId() != depID {
		t.Fatalf("removals %v", fake.removed)
	}
}

// Resume signs what cloud still lists, only at the current revision with
// the current content shown; a statement for another revision is refused.
func TestResumeSignsOnlyTheCurrentRevision(t *testing.T) {
	op, fake, _, depID := deployedFixture(t)
	device := deviceURI(testOperatorTenant, vectorDevice)
	fake.pending[depID] = []*cloudpb.PendingDeviceStatement{{Kind: cloudpb.PendingDeviceStatement_KIND_ASSIGN, DeploymentId: depID, Device: device, Revision: 1}}
	var out bytes.Buffer
	if err := runResume(context.Background(), op, "sh.wendy.example", "line-3", changeFlags{yes: true}, &out, false); err != nil {
		t.Fatal(err)
	}
	if len(fake.assigned) != 2 || fake.assigned[1].GetRevision() != 1 || !strings.Contains(out.String(), "packer-01: get revision 1") {
		t.Fatalf("resume signed %v\n%s", fake.assigned, out.String())
	}
	fake.pending[depID][0].Revision = 7
	if err := runResume(context.Background(), op, "sh.wendy.example", "line-3", changeFlags{yes: true}, io.Discard, false); err == nil || len(fake.assigned) != 2 {
		t.Fatalf("a statement for another revision: %v", err)
	}
}
