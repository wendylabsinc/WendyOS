package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"google.golang.org/grpc"

	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

func (f *fakeRegistry) ListApps(context.Context, *cloudpb.ListAppsRequest) (*cloudpb.ListAppsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &cloudpb.ListAppsResponse{Apps: f.apps, Total: int32(len(f.apps))}, nil
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
	for _, a := range f.assigned {
		if a.GetDeploymentId() == in.GetDeploymentId() {
			id := strings.TrimPrefix(a.GetDevice(), "spiffe://wendy.sh/tenant/"+testOperatorTenant+"/device/")
			rows = append(rows, &cloudpb.AppDeviceState{AssetId: id, AssetName: "dev-" + id[:4], State: &cloudpb.AssetApp{
				AppId: a.GetAppId(), DeploymentId: &a.DeploymentId, DesiredState: a.GetDesiredState(), Outcome: cloudpb.ApplyOutcome_APPLY_OUTCOME_APPLIED,
			}})
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
	fake.apps = []*cloudpb.App{{Id: "sh.wendy.example"}}
	fake.assets = []*cloudpb.Asset{{Id: vectorDevice, Name: "packer-01", Tags: []string{"line-3"}}, {Id: vectorOtherDevice, Name: "dock-cam-07"}}
	fake.online = map[string]bool{vectorDevice: true}
	prevAsk, prevConfirm := deployAsk, deployConfirm
	t.Cleanup(func() { deployAsk, deployConfirm = prevAsk, prevConfirm })
	deployAsk = func(q, _, _ string) (string, error) { t.Fatalf("asked %q", q); return "", nil }
	deployConfirm = func(q string) (bool, error) { t.Fatalf("asked %q", q); return false, nil }
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
	for _, want := range []string{"line-3: sh.wendy.example ", "to 1 device, installed only, 1 value changed.", "Devices: packer-01",
		"Offline devices keep trying for 30 days.", "1 installed", "Follow it later with: wendy app deployment show sh.wendy.example/line-3"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
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
	if len(fake.deploys) != 0 {
		t.Fatal("a deploy with a missing answer created a deployment")
	}
}

// L1: interactively, the questions come in the dashboard's order and words.
func TestDeployAsksTheDashboardQuestionsInOrder(t *testing.T) {
	op, _, releaseID := stepsFixture(t)
	answers := map[string]string{"App": "sh.wendy.example", "Release": releaseID, "Devices": "packer-01", "Name this deployment": "line-3",
		"LOG_LEVEL": "info", "UPLOAD_INTERVAL_SECONDS": "30", "Run it now, or install only?": "run", "Note for the history": ""}
	var asked []string
	deployAsk = func(q, _, _ string) (string, error) { asked = append(asked, q); return answers[q], nil }
	deployConfirm = func(q string) (bool, error) { asked = append(asked, q); return true, nil }
	var out bytes.Buffer
	if err := runDeploy(context.Background(), op, deployAnswers{keepTrying: "60d"}, &out, true); err != nil {
		t.Fatal(err)
	}
	want := "App,Release,Devices,Name this deployment,LOG_LEVEL,UPLOAD_INTERVAL_SECONDS,Run it now, or install only?,Note for the history,Deploy to 1 device?"
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
		if got, detail := deviceState(tc.s, tc.online); got != tc.want || (got != "running" && got != "installed" && got != "downloading" && detail == "") {
			t.Fatalf("%v online=%v: %s %q", tc.s.GetOutcome(), tc.online, got, detail)
		}
	}
}

const cloudpbDeployV2 = cloudpb.DeploymentService_CreateDeploymentV2_FullMethodName
