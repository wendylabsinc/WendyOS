package commands

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
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
	if exp := time.Until(fake.assigned[len(fake.assigned)-1].GetExpiresAt().AsTime()); exp > 61*24*time.Hour {
		t.Fatalf("validity without --keep-trying is the default 60 days, got %v", exp)
	}
	if err := runUpdate(ctx, op, "sh.wendy.example", "line-3", changeFlags{config: []string{"LOG_LEVEL=trace"}, keepTrying: "7d", yes: true}, io.Discard, false); err != nil {
		t.Fatal(err)
	}
	if exp := time.Until(fake.assigned[len(fake.assigned)-1].GetExpiresAt().AsTime()); exp > 8*24*time.Hour {
		t.Fatalf("--keep-trying 7d signed %v", exp)
	}
	if err := runUpdate(ctx, op, "sh.wendy.example", "line-3", changeFlags{removeTags: []string{"line-3"}, addDevices: []string{"packer-01"}, yes: true}, io.Discard, false); err != nil {
		t.Fatal(err)
	}
	if err := runUpdate(ctx, op, "sh.wendy.example", "line-3", changeFlags{removeDevices: []string{"packer-01"}, yes: true}, io.Discard, false); err != nil {
		t.Fatal(err)
	}
	if len(fake.removed) != 1 || fake.removed[0].GetRevision() != 5 {
		t.Fatalf("removals %v", fake.removed)
	}
}

// --add-site puts the site's devices in the selection sent as the new
// revision, in union with the rest: with the tag removed the device stays
// through its site. An unknown site is refused.
func TestUpdateAddAndRemoveSite(t *testing.T) {
	op, fake, _, _ := deployedFixture(t)
	fake.sites = []*cloudpb.Site{{Id: "site-1", Name: "plant-a"}}
	fake.assets[0].SiteId = proto.String("site-1")
	ctx := context.Background()
	if err := runUpdate(ctx, op, "sh.wendy.example", "line-3", changeFlags{addSites: []string{"site-1"}, removeTags: []string{"line-3"}, yes: true}, io.Discard, false); err != nil {
		t.Fatal(err)
	}
	upd := fake.signed(cloudpb.DeploymentService_UpdateDeployment_FullMethodName)
	if got := upd[0].payload.(*cloudpb.UpdateDeploymentRequest).GetDevices().GetTargetSiteIds(); len(got) != 1 || got[0] != "site-1" {
		t.Fatalf("selection sites %v", got)
	}
	if len(fake.removed) != 0 {
		t.Fatalf("the site keeps packer-01, removed %v", fake.removed)
	}
	if err := runUpdate(ctx, op, "sh.wendy.example", "line-3", changeFlags{removeSites: []string{"nowhere"}, yes: true}, io.Discard, false); err == nil || !strings.Contains(err.Error(), `No site "nowhere"`) {
		t.Fatalf("unknown site: %v", err)
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
	device := certs.DeviceSPIFFEURI(testOperatorTenant, vectorDevice)
	fake.pending[depID] = []*cloudpb.PendingDeviceStatement{{Kind: cloudpb.PendingDeviceStatement_KIND_ASSIGN, DeploymentId: depID, Device: device, Revision: 1}}
	var out bytes.Buffer
	if err := runResume(context.Background(), op, "sh.wendy.example", "line-3", changeFlags{yes: true}, &out, false); err != nil {
		t.Fatal(err)
	}
	if len(fake.assigned) != 2 || fake.assigned[1].GetRevision() != 1 || !strings.Contains(out.String(), "packer-01: get revision 1") {
		t.Fatalf("resume signed %v\n%s", fake.assigned, out.String())
	}
	// Cloud recorded release B, but the newest key is still for A: resume
	// shows B and must not sign A's digest.
	releaseB, _ := publishFixture(t, op)
	fake.deploys[depID].AppReleaseId = releaseB
	if err := runResume(context.Background(), op, "sh.wendy.example", "line-3", changeFlags{yes: true}, io.Discard, false); err == nil || !strings.Contains(err.Error(), "nothing was signed") || len(fake.assigned) != 2 {
		t.Fatalf("a key for another release: %v", err)
	}
	fake.deploys[depID].AppReleaseId = fake.assigned[0].GetAppReleaseId()
	fake.pending[depID][0].Revision = 7
	if err := runResume(context.Background(), op, "sh.wendy.example", "line-3", changeFlags{yes: true}, io.Discard, false); err == nil || len(fake.assigned) != 2 {
		t.Fatalf("a statement for another revision: %v", err)
	}
}

// A rollback follows the same rule as deploy and update: the release rolled
// back to must get every setting it requires.
func TestRollbackRefusesAnEmptyRequiredSetting(t *testing.T) {
	op, fake, releaseID, _ := deployedFixture(t)
	ctx := context.Background()
	if err := runUpdate(ctx, op, "sh.wendy.example", "line-3", changeFlags{config: []string{"LOG_LEVEL="}, yes: true}, io.Discard, false); err != nil {
		t.Fatal(err)
	}
	if err := runUpdate(ctx, op, "sh.wendy.example", "line-3", changeFlags{config: []string{"LOG_LEVEL=info"}, yes: true}, io.Discard, false); err != nil {
		t.Fatal(err)
	}
	fake.required[releaseID] = []string{"LOG_LEVEL"}
	n := len(fake.assigned)
	err := runRollback(ctx, op, "sh.wendy.example", "line-3", changeFlags{to: 2, yes: true}, io.Discard, false)
	if err == nil || !strings.HasPrefix(err.Error(), "Revision 2 leaves LOG_LEVEL empty, which release") || len(fake.assigned) != n {
		t.Fatalf("rollback to an empty required setting: %v", err)
	}
	if err := runRollback(ctx, op, "sh.wendy.example", "line-3", changeFlags{to: 1, yes: true}, io.Discard, false); err != nil {
		t.Fatalf("rollback to a filled one: %v", err)
	}
}

// Update, rollback, resume and rotate sign with the deployment's stored
// keep-trying time when no --keep-trying is given, show it, and never sign
// past the 364-day ceiling. A --keep-trying that sends nothing says so.
func TestLaterChangesKeepTheStoredKeepTryingTime(t *testing.T) {
	op, fake, _, depID := deployedFixture(t) // deployed with --keep-trying 30d
	ctx := context.Background()
	days := func() int {
		return int(time.Until(fake.assigned[len(fake.assigned)-1].GetExpiresAt().AsTime()).Round(time.Hour).Hours() / 24)
	}
	var out bytes.Buffer
	if err := runUpdate(ctx, op, "sh.wendy.example", "line-3", changeFlags{config: []string{"LOG_LEVEL=debug"}, yes: true}, &out, false); err != nil {
		t.Fatal(err)
	}
	if days() != 30 || !strings.Contains(out.String(), "Offline devices keep trying for 30 days\n") {
		t.Fatalf("update signed %d days\n%s", days(), out.String())
	}
	out.Reset()
	n := len(fake.assigned)
	if err := runUpdate(ctx, op, "sh.wendy.example", "line-3", changeFlags{keepTrying: "7d", yes: true}, &out, false); err != nil {
		t.Fatal(err)
	}
	if len(fake.assigned) != n || !strings.Contains(out.String(), "7 days on instructions sent from now on") {
		t.Fatalf("--keep-trying alone sent %d statements\n%s", len(fake.assigned)-n, out.String())
	}
	out.Reset()
	if err := runRollback(ctx, op, "sh.wendy.example", "line-3", changeFlags{to: 1, yes: true}, &out, false); err != nil {
		t.Fatal(err)
	}
	if days() != 30 || !strings.Contains(out.String(), "Offline devices keep trying for 30 days") {
		t.Fatalf("rollback signed %d days\n%s", days(), out.String())
	}
	d := fake.deploys[depID]
	d.Validity = durationpb.New(7 * 24 * time.Hour)
	fake.pending[depID] = []*cloudpb.PendingDeviceStatement{{Kind: cloudpb.PendingDeviceStatement_KIND_ASSIGN, DeploymentId: depID,
		Device: certs.DeviceSPIFFEURI(testOperatorTenant, vectorDevice), Revision: d.GetRevision()}}
	out.Reset()
	if err := runResume(ctx, op, "sh.wendy.example", "line-3", changeFlags{yes: true}, &out, false); err != nil {
		t.Fatal(err)
	}
	if days() != 7 || !strings.Contains(out.String(), "keep trying for 7 days") {
		t.Fatalf("resume signed %d days\n%s", days(), out.String())
	}
	d.Validity = durationpb.New(365 * 24 * time.Hour)
	if err := op.rotate(ctx, depID, "sh.wendy.example", []string{vectorDevice}); err != nil {
		t.Fatal(err)
	}
	if days() != 364 {
		t.Fatalf("rotate signed %d days, past the ceiling", days())
	}
}

// Deploy and update run the device's own configuration check before the
// review, naming the setting and the rule; --remove-config drops a setting.
func TestConfigIsCheckedLikeTheDevice(t *testing.T) {
	op, fake, releaseID := stepsFixture(t)
	ctx := context.Background()
	a := allFlags(releaseID)
	a.config = []string{"my-var=1"}
	if err := runDeploy(ctx, op, a, io.Discard, false); err == nil || !strings.Contains(err.Error(), `"my-var"`) || !strings.Contains(err.Error(), "letters, digits") || len(fake.deploys) != 0 {
		t.Fatalf("an invalid name: %v", err)
	}
	op, fake, _, depID := deployedFixture(t)
	n := len(fake.assigned)
	if err := runUpdate(ctx, op, "sh.wendy.example", "line-3", changeFlags{config: []string{"OTEL_SERVICE_NAME=x"}, yes: true}, io.Discard, false); err == nil || !strings.Contains(err.Error(), `"OTEL_SERVICE_NAME" is set by the agent`) || len(fake.assigned) != n {
		t.Fatalf("an agent-set name: %v", err)
	}
	// A setting a device refuses, signed before this check: update removes it.
	fake.deploys[depID].ConfigJson = `{"LOG_LEVEL":"info","UPLOAD_INTERVAL_SECONDS":"30","my-var":"1"}`
	if err := runUpdate(ctx, op, "sh.wendy.example", "line-3", changeFlags{config: []string{"LOG_LEVEL=debug"}, yes: true}, io.Discard, false); err == nil || !strings.Contains(err.Error(), "--remove-config") {
		t.Fatalf("a refused setting kept: %v", err)
	}
	if err := runUpdate(ctx, op, "sh.wendy.example", "line-3", changeFlags{removeConfig: []string{"my-var"}, yes: true}, io.Discard, false); err != nil {
		t.Fatal(err)
	}
	if last := fake.assigned[len(fake.assigned)-1]; strings.Contains(last.GetConfigJson(), "my-var") || len(fake.assigned) != n+1 {
		t.Fatalf("signed %s", last.GetConfigJson())
	}
}

// M12: a device is found by id, then name, then a display name only one
// device carries; a shared display name is refused with each device's id.
func TestAssetIDPrefersIDThenNameAndRefusesASharedDisplayName(t *testing.T) {
	assets := []*cloudpb.Asset{
		{Id: "id-a", Name: "cam-a", DisplayName: proto.String("cam-b")},
		{Id: "id-b", Name: "cam-b", DisplayName: proto.String("Lobby")},
		{Id: "id-c", Name: "cam-c", DisplayName: proto.String("Lobby")},
		{Id: "id-d", Name: "cam-d", DisplayName: proto.String("Dock")},
	}
	for in, want := range map[string]string{"id-c": "id-c", "cam-b": "id-b", "Dock": "id-d"} {
		if got, err := assetID(assets, in); err != nil || got != want {
			t.Errorf("assetID(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	_, err := assetID(assets, "Lobby")
	if err == nil || !strings.Contains(err.Error(), "cam-b (id-b)") || !strings.Contains(err.Error(), "cam-c (id-c)") || !strings.Contains(err.Error(), "Use the device id") {
		t.Fatalf("shared display name: err %v, want both devices listed with their ids", err)
	}
	if _, err := assetID(assets, "nope"); err == nil {
		t.Fatal("unknown device: want an error")
	}
}
