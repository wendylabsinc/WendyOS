package commands

import (
	"os"
	"slices"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

const promoteWendyJSON = `{"appId": "sh.wendy.example"}`

// promoteFixture is a project folder without a terminal, a user cache of its
// own, and the steps fixture's app and devices; any question fails the test.
func promoteFixture(t *testing.T) (*registryOp, *fakeRegistry) {
	t.Helper()
	k1, _ := vectorRecipientKey(t, loadRecipientVectors(t), "recipient-key-valid")
	op, fake := appFixture(t, k1)
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
	if err := os.WriteFile("wendy.json", []byte(promoteWendyJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	fake.apps["sh.wendy.example"] = &cloudpb.App{Id: "sh.wendy.example"}
	fake.assets = []*cloudpb.Asset{{Id: vectorDevice, Name: "packer-01"}, {Id: vectorOtherDevice, Name: "dock-cam-07"}}
	fake.online = map[string]bool{vectorDevice: true}
	prevAsk, prevConfirm := askText, askConfirm
	t.Cleanup(func() { askText, askConfirm = prevAsk, prevConfirm })
	askText = func(q, _, _ string, _ tui.ValidateFunc) (string, error) { t.Fatalf("asked %q", q); return "", nil }
	askConfirm = func(q string) (bool, error) { t.Fatalf("asked %q", q); return false, nil }
	return op, fake
}

// keepRunImage writes an image where wendy run keeps its last build.
func keepRunImage(t *testing.T) v1.Image {
	t.Helper()
	return keepRunImageFor(t, "linux/arm64")
}

// keepRunImageFor writes an image where wendy run keeps its last build for
// platform.
func keepRunImageFor(t *testing.T, platform string) v1.Image {
	t.Helper()
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	layer, _ := random.Layer(1000, types.OCILayer)
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatal(err)
	}
	p, err := layout.Write(chunkLayoutDir(cache, "sh.wendy.example", platform), empty.Index)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.AppendImage(img); err != nil {
		t.Fatal(err)
	}
	return img
}

var promoteArgs = []string{"--device", "packer-01", "--tag", "1.0.0", "--name", "bench", "--run", "--yes"}

// Promote publishes the image the last run kept (labelled with wendy.json as
// every release is), then deploys exactly that release to exactly that device.
func TestPromotePublishesTheLastRunAndDeploysItToThatDevice(t *testing.T) {
	op, fake := promoteFixture(t)
	kept := keepRunImage(t)
	out, err := runApp(newAppPromoteCmd(), promoteArgs...)
	if err != nil {
		t.Fatal(err)
	}
	req, m, _ := published(t, op, fake)
	_, def, _ := appDefinition([]byte(promoteWendyJSON), "")
	want, _ := withAppDefinition(kept, def)
	if d, _ := want.Digest(); req.GetName() != "1.0.0" || m.GetManifestDigest() != d.String() {
		t.Fatalf("published %s %s, want the kept image %s", req.GetName(), m.GetManifestDigest(), d)
	}
	var releaseID string
	for releaseID = range fake.releases {
	}
	deploys := fake.signed(cloudpbDeployV2)
	if len(deploys) != 1 {
		t.Fatalf("deploys %v", deploys)
	}
	dep := deploys[0].payload.(*cloudpb.CreateDeploymentRequest)
	if dep.GetAppReleaseId() != releaseID || !slices.Equal(dep.GetTargetDeviceIds(), []string{vectorDevice}) || len(dep.GetTargetTags()) != 0 {
		t.Fatalf("deployed %v", dep)
	}
	for _, line := range []string{"Published 1.0.0 of sh.wendy.example. Deploying it to packer-01.",
		"packer-01 replaces the running app with the published release; it does not keep the running container yet."} {
		if !strings.Contains(out, line) {
			t.Fatalf("output lacks %q:\n%s", line, out)
		}
	}
}

// Without a last run there is nothing to promote, and nothing is published.
func TestPromoteWithoutALastRunPublishesNothing(t *testing.T) {
	_, fake := promoteFixture(t)
	_, err := runApp(newAppPromoteCmd(), promoteArgs...)
	if err == nil || err.Error() != "Nothing to promote: run the app with wendy run first, or publish it with wendy app publish." {
		t.Fatalf("no last run: %v", err)
	}
	if len(fake.calls) != 0 || len(fake.releases) != 0 {
		t.Fatalf("calls %+v", fake.calls)
	}
}

// Without a terminal a missing --device is named, before anything happens.
func TestPromoteWithoutDeviceNamesTheFlag(t *testing.T) {
	_, fake := promoteFixture(t)
	keepRunImage(t)
	_, err := runApp(newAppPromoteCmd(), promoteArgs[2:]...)
	if err == nil || !strings.Contains(err.Error(), "--device") {
		t.Fatalf("missing device: %v", err)
	}
	if len(fake.calls) != 0 || len(fake.releases) != 0 {
		t.Fatalf("calls %+v", fake.calls)
	}
}

// Promote checks the deploy answers (the device, an answer missing without a
// terminal, an empty required setting) before it publishes anything; a deploy
// that fails after the publish names the release and the command to deploy it.
func TestPromoteChecksDeployAnswersBeforePublishing(t *testing.T) {
	_, fake := promoteFixture(t)
	keepRunImage(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--device", "nope", "--tag", "1.0.0", "--name", "bench", "--run", "--yes"}, `No device "nope"`},
		{[]string{"--device", "packer-01", "--tag", "1.0.0", "--run", "--yes"}, "--name"},
		{[]string{"--device", "packer-01", "--tag", "1.0.0", "--name", "bench", "--yes"}, "--run or --install-only"},
		{[]string{"--device", "packer-01", "--tag", "1.0.0", "--name", "bench", "--run"}, "--yes"},
		{[]string{"--device", "packer-01", "--tag", "1.0.0", "--name", "bench", "--run", "--yes", "--config", "my-var=1"}, `"my-var"`},
	} {
		if _, err := runApp(newAppPromoteCmd(), tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) || len(fake.releases) != 0 {
			t.Fatalf("%v: %v, %d releases", tc.args, err, len(fake.releases))
		}
	}
	if err := os.WriteFile("wendy.json", []byte(`{"appId": "sh.wendy.example", "env": {"API_KEY": ""}, "requiredEnv": ["API_KEY"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runApp(newAppPromoteCmd(), promoteArgs...); err == nil || !strings.Contains(err.Error(), "API_KEY has no value") || len(fake.releases) != 0 {
		t.Fatalf("an empty required setting: %v, %d releases", err, len(fake.releases))
	}
	fake.keys = nil // the device has no registry key: the deploy fails after the publish
	_, err := runApp(newAppPromoteCmd(), append(promoteArgs, "--config", "API_KEY=k")...)
	if err == nil || !strings.Contains(err.Error(), "release 1.0.0 of sh.wendy.example is published") ||
		!strings.Contains(err.Error(), "wendy app deploy --app sh.wendy.example --release 1.0.0 --device packer-01") || len(fake.releases) != 1 {
		t.Fatalf("a deploy failed after the publish: %v", err)
	}
}

// M21: promote picks the last run's build for the device's own CPU (an
// x86_64 device here), not arm64, and names its digest and build time
// before publishing.
func TestPromoteUsesTheDevicesArchitectureAndNamesTheImage(t *testing.T) {
	_, fake := promoteFixture(t)
	fake.assets[0].Architecture = proto.String("x86_64")
	keepRunImage(t) // an arm64 build that must not be picked
	kept := keepRunImageFor(t, "linux/amd64")
	out, err := runApp(newAppPromoteCmd(), promoteArgs...)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := kept.Digest()
	if !strings.Contains(out, "Promoting the image wendy run last built for linux/amd64: "+d.String()+", built ") {
		t.Fatalf("output does not name the amd64 build %s:\n%s", d, out)
	}
	for in, want := range map[string]string{"": "arm64", "aarch64": "arm64", "AMD64": "amd64", "x86_64": "amd64", "riscv64": "riscv64"} {
		if got := deviceArch(in); got != want {
			t.Errorf("deviceArch(%q) = %q, want %q", in, got, want)
		}
	}
}

// M10/M22: after publishing, promote's advice follows what happened. A
// deployment that was made (here a device failed) points to deployment
// show, never to a second deploy. When nothing was made, the deploy command
// carries the user's answers, so it works without a terminal.
func TestPromoteAdviceAfterPublish(t *testing.T) {
	_, fake := promoteFixture(t)
	keepRunImage(t)
	fake.report = func(s *cloudpb.AssetApp) { s.Outcome = cloudpb.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED }
	_, err := runApp(newAppPromoteCmd(), promoteArgs...)
	if err == nil || !strings.Contains(err.Error(), "See it with: wendy app deployment show sh.wendy.example/bench") || strings.Contains(err.Error(), "wendy app deploy ") {
		t.Fatalf("a made deployment: %v", err)
	}

	fake.keys = nil // the device has no key: nothing is made
	_, err = runApp(newAppPromoteCmd(), "--device", "packer-01", "--tag", "1.0.1", "--name", "bench-2", "--run", "--yes", "--config", "MODE=fast mode")
	if err == nil || !strings.Contains(err.Error(), "nothing was deployed") ||
		!strings.Contains(err.Error(), "Deploy it with: wendy app deploy --app sh.wendy.example --release 1.0.1 --device packer-01 --name bench-2 --config 'MODE=fast mode' --digest sha256:") ||
		!strings.HasSuffix(err.Error(), " --run --yes") {
		t.Fatalf("nothing made: %v", err)
	}
}
