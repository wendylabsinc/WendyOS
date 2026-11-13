package commands

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// Create step 4 by flags: the given values are the app's override, checked
// against the organization's per-app limit under the plan limit before
// anything is created.
func TestAppCreateLifecycleByFlags(t *testing.T) {
	_, fake := appFixture(t)
	if _, err := runApp(newAppCreateCmd(), "--name", "Cam", "--app", "cam", "--keep-tagged", "30", "--yes"); err == nil || !strings.Contains(err.Error(), "enter 20 or less") {
		t.Fatalf("above the per-app limit: %v", err)
	}
	if n := len(fake.signed(cloudpb.AppService_UpsertApp_FullMethodName)); n != 0 {
		t.Fatalf("created %d apps", n)
	}
	out, err := runApp(newAppCreateCmd(), "--name", "Cam", "--app", "cam", "--keep-tagged", "10", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	p := fake.signed(cloudpb.AppService_UpsertApp_FullMethodName)[0].payload.(*cloudpb.UpsertAppRequest).GetLifecyclePolicy()
	if p.GetKeepTagged() != 10 || p.KeepUntagged != nil || p.UntaggedMaxAgeDays != nil {
		t.Fatalf("policy %v", p)
	}
	if !strings.Contains(out, "Lifecycle: Own policy · keep 10 tagged") {
		t.Fatalf("review:\n%s", out)
	}
}

// Create step 4 in a terminal: the organization default is shown, and a
// policy for this app is asked value by value within its limit.
func TestAppCreateAsksTheLifecycle(t *testing.T) {
	_, fake := appFixture(t)
	isInteractiveTerminalFn = func() bool { return true }
	answers := map[string]string{"Use the organization default, or set a policy for this app?": "own",
		"Tagged releases to keep": "8", "Untagged releases to keep": "12", "Delete untagged releases older than (days)": "90"}
	askText = func(prompt, _, _ string, check tui.ValidateFunc) (string, error) {
		if prompt == "Tagged releases to keep" && (check("21") == nil || check("0") == nil) {
			t.Error("tagged accepts 21 or 0 under a limit of 20")
		}
		return answers[prompt], nil
	}
	if _, err := runApp(newAppCreateCmd(), "--name", "Cam", "--app", "cam", "--no-notifications", "--access", "", "--yes"); err != nil {
		t.Fatal(err)
	}
	p := fake.signed(cloudpb.AppService_UpsertApp_FullMethodName)[0].payload.(*cloudpb.UpsertAppRequest).GetLifecyclePolicy()
	if p.GetKeepTagged() != 8 || p.GetKeepUntagged() != 12 || p.GetUntaggedMaxAgeDays() != 90 {
		t.Fatalf("policy %v", p)
	}
}

// `wendy app lifecycle` shows the policy, its source and limits; a value
// flag changes only that field of the override; --org-default clears it.
func TestAppLifecycleShowAndSet(t *testing.T) {
	_, fake := appFixture(t)
	fake.apps["cam"] = &cloudpb.App{Id: "cam", LifecyclePolicy: &cloudpb.LifecyclePolicy{KeepTagged: proto.Int32(3)}}
	out, err := runApp(newAppLifecycleCmd(), "cam")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cam: Own policy", "Tagged releases: keep the last 3. It may keep up to 20.",
		"Untagged releases: keep the last 10, and none older than 180 days. It may keep up to 100, for up to 365 days."} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if _, err := runApp(newAppLifecycleCmd(), "cam", "--keep-untagged", "7"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("unconfirmed: %v", err)
	}
	if _, err := runApp(newAppLifecycleCmd(), "cam", "--keep-untagged", "7", "--yes"); err != nil {
		t.Fatal(err)
	}
	if _, err := runApp(newAppLifecycleCmd(), "cam", "--org-default", "--yes"); err != nil {
		t.Fatal(err)
	}
	upd := fake.signed(cloudpb.AppService_UpdateApp_FullMethodName)
	if p := upd[0].payload.(*cloudpb.UpdateAppRequest).GetLifecyclePolicy(); len(upd) != 2 || p.GetKeepTagged() != 3 || p.GetKeepUntagged() != 7 {
		t.Fatalf("set %v", upd)
	}
	if p := upd[1].payload.(*cloudpb.UpdateAppRequest).LifecyclePolicy; p == nil || p.KeepTagged != nil || p.KeepUntagged != nil {
		t.Fatalf("--org-default sent %v", p)
	}
}
