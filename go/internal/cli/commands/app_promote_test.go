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
	prevAsk, prevConfirm := deployAsk, deployConfirm
	t.Cleanup(func() { deployAsk, deployConfirm = prevAsk, prevConfirm })
	deployAsk = func(q, _, _ string) (string, error) { t.Fatalf("asked %q", q); return "", nil }
	deployConfirm = func(q string) (bool, error) { t.Fatalf("asked %q", q); return false, nil }
	return op, fake
}

// keepRunImage writes an image where wendy run keeps its last build.
func keepRunImage(t *testing.T) v1.Image {
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
	p, err := layout.Write(chunkLayoutDir(cache, "sh.wendy.example", "linux/arm64"), empty.Index)
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
