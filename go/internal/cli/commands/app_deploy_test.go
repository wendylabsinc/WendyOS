package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

func (f fakeApps) ListApps(context.Context, *cloudpb.ListAppsRequest) (*cloudpb.ListAppsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	apps := slices.SortedFunc(maps.Values(f.apps), func(a, b *cloudpb.App) int { return strings.Compare(a.GetId(), b.GetId()) })
	return &cloudpb.ListAppsResponse{Apps: apps, Total: int32(len(apps))}, nil
}

func (f *fakeRegistry) ListAssets(in *cloudpb.ListAssetsRequest, stream grpc.ServerStreamingServer[cloudpb.ListAssetsResponse]) error {
	f.mu.Lock()
	var out []*cloudpb.Asset
	for _, a := range f.assets {
		if !in.GetOnlineOnly() || f.online[a.GetId()] {
			out = append(out, a)
		}
	}
	f.mu.Unlock()
	for _, a := range out {
		if err := stream.Send(&cloudpb.ListAssetsResponse{Asset: a, Total: int32(len(out))}); err != nil {
			return err
		}
	}
	return nil
}

// ListAppDeviceStates answers a deployment's rows from what was signed:
// a device that got its statement reports it applied and running.
func (f *fakeRegistry) ListAppDeviceStates(_ context.Context, in *cloudpb.ListAppDeviceStatesRequest) (*cloudpb.ListAppDeviceStatesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if in.GetDeploymentId() == "" {
		return &cloudpb.ListAppDeviceStatesResponse{Rows: f.states}, nil
	}
	var rows []*cloudpb.AppDeviceState
	for i, a := range f.assigned {
		if a.GetDeploymentId() == in.GetDeploymentId() {
			id := strings.TrimPrefix(a.GetDevice(), "spiffe://wendy.sh/tenant/"+testOperatorTenant+"/device/")
			s := &cloudpb.AssetApp{
				AppId: a.GetAppId(), DeploymentId: &a.DeploymentId, DesiredState: a.GetDesiredState(), Outcome: cloudpb.ApplyOutcome_APPLY_OUTCOME_APPLIED,
				AppReleaseId: &a.AppReleaseId, ReportedReleaseId: &a.AppReleaseId, PostboxEntryId: int64(i + 1),
			}
			if f.report != nil {
				f.report(s)
			}
			rows = append(rows, &cloudpb.AppDeviceState{AssetId: id, AssetName: "dev-" + id[:4], State: s})
		}
	}
	return &cloudpb.ListAppDeviceStatesResponse{Rows: rows}, nil
}

// stepsFixture is a published release of the example app, two devices
// (packer-01 tagged line-3 and online; dock-cam-07 offline) and prompts that
// fail the test if a question is asked.
func stepsFixture(t *testing.T) (*registryOp, *fakeRegistry, string) {
	t.Helper()
	v := loadRecipientVectors(t)
	k1, _ := vectorRecipientKey(t, v, "recipient-key-valid")
	op, fake := registryFixture(t, k1)
	releaseID, _ := publishFixture(t, op)
	fake.defaults[releaseID] = `{"LOG_LEVEL":"info","UPLOAD_INTERVAL_SECONDS":"30"}`
	fake.apps["sh.wendy.example"] = &cloudpb.App{Id: "sh.wendy.example"}
	fake.assets = []*cloudpb.Asset{{Id: vectorDevice, Name: "packer-01", Tags: []string{"line-3"}}, {Id: vectorOtherDevice, Name: "dock-cam-07"}}
	fake.online = map[string]bool{vectorDevice: true}
	prevAsk, prevConfirm := askText, askConfirm
	t.Cleanup(func() { askText, askConfirm = prevAsk, prevConfirm })
	askText = func(q, _, _ string, _ tui.ValidateFunc) (string, error) { t.Fatalf("asked %q", q); return "", nil }
	askConfirm = func(q string) (bool, error) { t.Fatalf("asked %q", q); return false, nil }
	return op, fake, releaseID
}

func allFlags(releaseID string) deployAnswers {
	return deployAnswers{app: "sh.wendy.example", release: releaseID, tags: []string{"line-3"}, name: "line-3",
		config: []string{"UPLOAD_INTERVAL_SECONDS=10"}, installOnly: true, keepTrying: "30d", note: "first", yes: true}
}

// L2: with every flag given nothing is asked, and each answer reaches the
// signed request. A tag is resolved to the devices shown in the review.
func TestDeployWithAllFlagsAsksNothing(t *testing.T) {
	op, fake, releaseID := stepsFixture(t)
	var out bytes.Buffer
	if err := runDeploy(context.Background(), op, allFlags(releaseID), &out, false); err != nil {
		t.Fatal(err)
	}
	req := fake.signed(cloudpbDeployV2)[0].payload.(*cloudpb.CreateDeploymentRequest)
	var cfg map[string]string
	_ = json.Unmarshal([]byte(req.GetConfigJson()), &cfg)
	if req.GetName() != "line-3" || req.GetTargetTags()[0] != "line-3" || len(req.GetTargetDeviceIds()) != 0 || req.GetRunning() != cloudpb.Running_STOPPED ||
		req.GetNote() != "first" || req.GetValidity().AsDuration().Hours() != 30*24 || cfg["UPLOAD_INTERVAL_SECONDS"] != "10" || cfg["LOG_LEVEL"] != "info" {
		t.Fatalf("request %v", req)
	}
	if len(fake.assigned) != 1 || fake.assigned[0].GetDesiredState() != cloudpb.Running_STOPPED {
		t.Fatalf("signed %v", fake.assigned)
	}
	// The statement carries the operator's 30 days, not Cloud's echo.
	if exp := time.Until(fake.assigned[0].GetExpiresAt().AsTime()); exp < 29*24*time.Hour || exp > 31*24*time.Hour {
		t.Fatalf("expires in %v", exp)
	}
	for _, want := range []string{"line-3: sh.wendy.example ", "to 1 device, installed only, 1 value changed.", "Devices: packer-01",
		"Offline devices keep trying for 30 days.", "1 installed", "Follow it later with: wendy app deployment show sh.wendy.example/line-3"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// A site selects every device in it, by name or id; the review names each
// resolved device before signing, and each gets its own statement. An
// unknown site is refused.
func TestDeployBySite(t *testing.T) {
	op, fake, releaseID := stepsFixture(t)
	fake.sites = []*cloudpb.Site{{Id: "site-1", Name: "plant-a"}}
	fake.assets[0].SiteId = proto.String("site-1")
	a := allFlags(releaseID)
	a.tags, a.sites = nil, []string{"nowhere"}
	if err := runDeploy(context.Background(), op, a, io.Discard, false); err == nil || !strings.Contains(err.Error(), `No site "nowhere"`) {
		t.Fatalf("unknown site: %v", err)
	}
	a.sites = []string{"plant-a"}
	var out bytes.Buffer
	if err := runDeploy(context.Background(), op, a, &out, false); err != nil {
		t.Fatal(err)
	}
	req := fake.signed(cloudpbDeployV2)[0].payload.(*cloudpb.CreateDeploymentRequest)
	if !slices.Equal(req.GetTargetSiteIds(), []string{"site-1"}) || len(req.GetTargetTags())+len(req.GetTargetDeviceIds()) != 0 {
		t.Fatalf("request %v", req)
	}
	if !strings.Contains(out.String(), "Devices: packer-01\n") {
		t.Fatalf("review:\n%s", out.String())
	}
	if len(fake.assigned) != 1 || fake.assigned[0].GetDevice() != certs.DeviceSPIFFEURI(testOperatorTenant, vectorDevice) {
		t.Fatalf("statements %v", fake.assigned)
	}
}

// L3: without a terminal a missing answer is an error naming its flag, and
// nothing is created.
func TestDeployMissingAnswerNamesTheFlag(t *testing.T) {
	op, fake, releaseID := stepsFixture(t)
	for flag, drop := range map[string]func(*deployAnswers){
		"--app":            func(a *deployAnswers) { a.app = "" },
		"--release":        func(a *deployAnswers) { a.release = "" },
		"--tag <tag>":      func(a *deployAnswers) { a.tags = nil },
		"--name":           func(a *deployAnswers) { a.name = "" },
		"--install-only":   func(a *deployAnswers) { a.installOnly = false },
		"Add --yes to dep": func(a *deployAnswers) { a.yes = false },
	} {
		a := allFlags(releaseID)
		drop(&a)
		if err := runDeploy(context.Background(), op, a, io.Discard, false); err == nil || !strings.Contains(err.Error(), flag) {
			t.Fatalf("%s: %v", flag, err)
		}
	}
	a := allFlags(releaseID)
	a.tags, a.devices = nil, []string{""} // dock-cam-07 has no display name
	if err := runDeploy(context.Background(), op, a, io.Discard, false); err == nil || !strings.Contains(err.Error(), `No device ""`) {
		t.Fatalf("an empty device name: %v", err)
	}
	if len(fake.deploys) != 0 {
		t.Fatal("a deploy with a missing answer created a deployment")
	}
}

// L1: interactively, the questions come in the dashboard's order and words.
func TestDeployAsksTheDashboardQuestionsInOrder(t *testing.T) {
	op, _, releaseID := stepsFixture(t)
	answers := map[string]string{"App": "sh.wendy.example", "Release": releaseID, "Devices": "packer-01", "Name this deployment": "line-3",
		"LOG_LEVEL": "info", "UPLOAD_INTERVAL_SECONDS": "30", "Run it now, or install only?": "run", "If a device is offline, keep trying for": "1y", "Note for the history": ""}
	var asked []string
	askText = func(q, _, _ string, _ tui.ValidateFunc) (string, error) {
		asked = append(asked, q)
		return answers[q], nil
	}
	askConfirm = func(q string) (bool, error) { asked = append(asked, q); return true, nil }
	var out bytes.Buffer
	if err := runDeploy(context.Background(), op, deployAnswers{}, &out, true); err != nil {
		t.Fatal(err)
	}
	want := "App,Release,Devices,Name this deployment,LOG_LEVEL,UPLOAD_INTERVAL_SECONDS,Run it now, or install only?,If a device is offline, keep trying for,Note for the history,Deploy to 1 device?"
	if got := strings.Join(asked, ","); got != want {
		t.Fatalf("asked %s", got)
	}
	last := -1
	for _, h := range []string{"1/7  Which app do you want to deploy?", "2/7  Which release of sh.wendy.example?", "3/7  Which devices should get it?",
		"4/7  What configuration should it use?", "5/7  Should it run now?", "6/7  Review", "7/7  Progress"} {
		i := strings.Index(out.String(), h)
		if i <= last {
			t.Fatalf("%q missing or out of order:\n%s", h, out.String())
		}
		last = i
	}
}

// A failed site listing does not stop an interactive deploy by device: it
// says so and goes on.
func TestDeployGoesOnWhenSitesFailToLoad(t *testing.T) {
	op, fake, releaseID := stepsFixture(t)
	fake.sitesErr = status.Error(codes.Unavailable, "down")
	answers := map[string]string{"App": "sh.wendy.example", "Release": releaseID, "Devices": "packer-01", "Name this deployment": "line-3",
		"Run it now, or install only?": "run", "If a device is offline, keep trying for": "60d"}
	askText = func(q, _, def string, _ tui.ValidateFunc) (string, error) {
		if a, ok := answers[q]; ok {
			return a, nil
		}
		return def, nil
	}
	askConfirm = func(string) (bool, error) { return true, nil }
	var out bytes.Buffer
	if err := runDeploy(context.Background(), op, deployAnswers{}, &out, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Sites could not be loaded; choose tags or devices.") || len(fake.assigned) != 1 {
		t.Fatalf("assigned %d:\n%s", len(fake.assigned), out.String())
	}
}

// A chosen device that runs the app in another deployment moves only when
// the operator says so, and only its confirmed move is signed.
func TestDeployMovesOnlyConfirmedDevices(t *testing.T) {
	op, fake, releaseID := stepsFixture(t)
	other := "11111111-2222-4333-8444-555555555555"
	fake.states = []*cloudpb.AppDeviceState{{AssetId: vectorDevice, State: &cloudpb.AssetApp{AppId: "sh.wendy.example", DeploymentId: &other}}}
	a := allFlags(releaseID)
	if err := runDeploy(context.Background(), op, a, io.Discard, false); err == nil || !strings.Contains(err.Error(), "Add --move") {
		t.Fatalf("unconfirmed move: %v", err)
	}
	a.move = true
	if err := runDeploy(context.Background(), op, a, io.Discard, false); err != nil {
		t.Fatal(err)
	}
	if len(fake.assigned) != 1 || fake.assigned[0].GetReplacesDeploymentId() != other || fake.assigned[0].GetReplacesRevision() != 2 {
		t.Fatalf("move signed as %v", fake.assigned)
	}
}

// L4: the per-device states are the dashboard's (ruling 11).
func TestDeviceStateMapping(t *testing.T) {
	o := func(x cloudpb.ApplyOutcome) *cloudpb.AssetApp {
		return &cloudpb.AssetApp{Outcome: x, DesiredState: cloudpb.Running_RUNNING}
	}
	for _, tc := range []struct {
		s      *cloudpb.AssetApp
		online bool
		want   string
	}{
		{o(cloudpb.ApplyOutcome_APPLY_OUTCOME_APPLIED), true, "running"},
		{&cloudpb.AssetApp{Outcome: cloudpb.ApplyOutcome_APPLY_OUTCOME_APPLIED, DesiredState: cloudpb.Running_STOPPED}, true, "installed"},
		{&cloudpb.AssetApp{Outcome: cloudpb.ApplyOutcome_APPLY_OUTCOME_APPLIED, StatementPending: true}, true, "waiting"},
		{o(cloudpb.ApplyOutcome_APPLY_OUTCOME_PULLING), true, "downloading"},
		{o(cloudpb.ApplyOutcome_APPLY_OUTCOME_NOT_STARTED), true, "downloading"},
		{o(cloudpb.ApplyOutcome_APPLY_OUTCOME_PULL_RETRYING), true, "waiting"},
		{o(cloudpb.ApplyOutcome_APPLY_OUTCOME_UNSPECIFIED), false, "waiting"},
		{o(cloudpb.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED), true, "failed"},
		{o(cloudpb.ApplyOutcome_APPLY_OUTCOME_PULL_REFUSED), true, "failed"},
		{o(cloudpb.ApplyOutcome_APPLY_OUTCOME_REJECTED_DIGEST), true, "failed"},
	} {
		if got, detail := deviceState(tc.s, tc.online, 0); got != tc.want || (got != "running" && got != "installed" && got != "downloading" && detail == "") {
			t.Fatalf("%v online=%v: %s %q", tc.s.GetOutcome(), tc.online, got, detail)
		}
	}
}

const cloudpbDeployV2 = cloudpb.DeploymentService_CreateDeploymentV2_FullMethodName

// A setting the release requires must get a value (sem 2026-11-13 11:01);
// an empty value is a value for any other setting.
func TestDeployRefusesAnEmptyRequiredSetting(t *testing.T) {
	op, fake, releaseID := stepsFixture(t)
	fake.required[releaseID] = []string{"LOG_LEVEL"} // read back from the release
	a := allFlags(releaseID)
	a.config = []string{"LOG_LEVEL=", "UPLOAD_INTERVAL_SECONDS="}
	if err := runDeploy(context.Background(), op, a, io.Discard, false); err == nil || err.Error() != "LOG_LEVEL has no value. Add --config LOG_LEVEL=<value>." {
		t.Fatalf("empty required: %v", err)
	}
	if len(fake.deploys) != 0 {
		t.Fatal("a deployment was created")
	}
	a.config = []string{"LOG_LEVEL=debug", "UPLOAD_INTERVAL_SECONDS="}
	if err := runDeploy(context.Background(), op, a, io.Discard, false); err != nil {
		t.Fatalf("an empty optional setting: %v", err)
	}
	if err := runUpdate(context.Background(), op, "sh.wendy.example", "line-3", changeFlags{config: []string{"LOG_LEVEL="}, yes: true}, io.Discard, false); err == nil || !strings.Contains(err.Error(), "LOG_LEVEL has no value") {
		t.Fatalf("update to an empty required: %v", err)
	}
}

// Interactively, a required setting left empty is asked again.
func TestDeployAsksARequiredSettingAgain(t *testing.T) {
	op, _, releaseID := stepsFixture(t)
	prev := releaseRequired
	t.Cleanup(func() { releaseRequired = prev })
	releaseRequired = func(*cloudpb.AppRelease) []string { return []string{"LOG_LEVEL"} }
	asks := 0
	askText = func(q, hint, def string, _ tui.ValidateFunc) (string, error) {
		if q != "LOG_LEVEL" {
			return def, nil
		}
		if asks++; asks == 1 {
			return "", nil
		}
		return "debug", nil
	}
	a := allFlags(releaseID)
	a.config = nil
	if err := runDeploy(context.Background(), op, a, io.Discard, true); err != nil || asks != 2 {
		t.Fatalf("asked %d times: %v", asks, err)
	}
}

// Progress and --wait count a device's outcome only for the statement sent
// (its entry, and "applied" only for the release sent). --wait running out
// of time exits non-zero naming the devices still waiting, and a failed device
// fails deploy; deployment show still exits 0, since it showed the state.
func TestProgressMatchesTheStatementSent(t *testing.T) {
	other := "00000000-0000-4000-8000-000000000001"
	if got, _ := deviceState(&cloudpb.AssetApp{Outcome: cloudpb.ApplyOutcome_APPLY_OUTCOME_APPLIED, AppReleaseId: proto.String("b"), ReportedReleaseId: &other}, true, 0); got != "waiting" {
		t.Fatalf("applied for an earlier release shown %s", got)
	}
	op, fake, releaseID := stepsFixture(t)
	prevPoll := deployPoll
	deployPoll = time.Millisecond
	t.Cleanup(func() { deployPoll = prevPoll })
	a := allFlags(releaseID)
	a.wait, a.waitTimeout = true, 20*time.Millisecond
	fake.report = func(s *cloudpb.AssetApp) { s.PostboxEntryId-- } // still reports the entry before the one sent
	var out bytes.Buffer
	if err := runDeploy(context.Background(), op, a, &out, false); err == nil || !strings.Contains(err.Error(), "stopped waiting after 20ms; still waiting: dev-"+vectorDevice[:4]+". The deployment continues") || !strings.Contains(out.String(), "1 waiting") {
		t.Fatalf("an earlier entry's outcome: %v\n%s", err, out.String())
	}
	fake.report = func(s *cloudpb.AssetApp) { s.Outcome = cloudpb.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED }
	a.name = "line-4"
	if err := runDeploy(context.Background(), op, a, io.Discard, false); err == nil || !strings.Contains(err.Error(), "1 device failed") {
		t.Fatalf("a failed device: %v", err)
	}
	out.Reset()
	if err := op.showDeployment(context.Background(), "sh.wendy.example", "line-4", &out); err != nil || !strings.Contains(out.String(), "1 failed") {
		t.Fatalf("show with a failed device: %v\n%s", err, out.String())
	}
}

// In a terminal without --wait, progress follows with no time limit (an
// offline device is a normal state), and Ctrl-C leaves it with exit 0 and
// says the deployment continues.
func TestDeployCtrlCLeavesProgressAndExitsZero(t *testing.T) {
	op, fake, releaseID := stepsFixture(t)
	prevPoll := deployPoll
	deployPoll = time.Millisecond
	t.Cleanup(func() { deployPoll = prevPoll })
	a := allFlags(releaseID)
	a.waitTimeout = time.Millisecond // --wait's limit; not given here
	polls := 0
	ctx, cancel := context.WithCancelCause(context.Background())
	fake.report = func(s *cloudpb.AssetApp) {
		s.PostboxEntryId-- // never reports the entry sent: stays waiting
		if polls++; polls == 50 {
			cancel(signalCause(os.Interrupt))
		}
	}
	var out bytes.Buffer
	if err := runDeploy(ctx, op, a, &out, true); err != nil || polls < 50 {
		t.Fatalf("after %d polls: %v\n%s", polls, err, out.String())
	}
	if !strings.Contains(out.String(), "The deployment continues; follow it with: wendy app deployment show sh.wendy.example/line-3") {
		t.Fatalf("output:\n%s", out.String())
	}
}

// A deploy where some devices were never sent their instruction exits
// non-zero, with and without --wait, as a made deployment, so promote does
// not report success and points to deployment show.
func TestDeployWithAnUnsentDeviceFails(t *testing.T) {
	unsent := []*cloudpb.PendingDeviceStatement{{Kind: cloudpb.PendingDeviceStatement_KIND_ASSIGN, Device: certs.DeviceSPIFFEURI(testOperatorTenant, vectorOtherDevice), Revision: 1}}
	t.Run("deploy", func(t *testing.T) {
		op, fake, releaseID := stepsFixture(t)
		prevPoll := deployPoll
		deployPoll = time.Millisecond
		t.Cleanup(func() { deployPoll = prevPoll })
		fake.extra = unsent // signing it fails: it was not asked for
		for _, wait := range []bool{false, true} {
			a := allFlags(releaseID)
			a.name, a.wait, a.waitTimeout = fmt.Sprintf("line-3-%v", wait), wait, time.Minute
			var out bytes.Buffer
			err := runDeploy(context.Background(), op, a, &out, false)
			if _, ok := errors.AsType[*deployedError](err); !ok || !strings.Contains(err.Error(), "1 devices not sent their instruction") ||
				!strings.Contains(out.String(), "Send them later with: wendy app deployment resume sh.wendy.example/"+a.name) {
				t.Fatalf("--wait=%v: %v\n%s", wait, err, out.String())
			}
		}
	})
	t.Run("promote", func(t *testing.T) {
		_, fake := promoteFixture(t)
		keepRunImage(t)
		fake.extra = unsent
		_, err := runApp(newAppPromoteCmd(), promoteArgs...)
		if err == nil || !strings.Contains(err.Error(), "deployment sh.wendy.example/bench was made, but:") {
			t.Fatalf("promote: %v", err)
		}
	})
}

// --wait-timeout is checked only with --wait, which it belongs to.
func TestWaitTimeoutIsCheckedOnlyWithWait(t *testing.T) {
	if err := (&deployAnswers{waitTimeout: 0}).checkSteps(); err != nil {
		t.Fatalf("without --wait: %v", err)
	}
	if err := (&deployAnswers{wait: true, waitTimeout: 0}).checkSteps(); err == nil || !strings.Contains(err.Error(), "--wait-timeout must be longer than 0") {
		t.Fatalf("with --wait: %v", err)
	}
}
