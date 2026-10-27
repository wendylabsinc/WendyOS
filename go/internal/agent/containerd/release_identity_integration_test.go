//go:build linux

package containerd

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// A created container carries the digest of the image it was created from
// and, for a recorded deploy, the release id; an unrecorded deploy (a CLI
// that predates release_id) gets no release label. Needs a disposable
// containerd, like the other integration tests.
func TestIntegrationCreateLabelsReleaseIdentity(t *testing.T) {
	c, ctx := newIntegrationClient(t)
	img := pullReplaceTestImage(t, c, ctx)
	const release = "0b6f2c1e-8a3d-4c5e-9f00-1234567890ab"

	for app, releaseID := range map[string]*string{"recorded-app": proto.String(release), "unrecorded-app": nil} {
		req := &agentpb.CreateContainerRequest{AppName: app, ImageName: replaceTestImage, ReleaseId: releaseID}
		if err := c.CreateContainerWithProgress(ctx, req, &appconfig.AppConfig{AppID: app, Version: "1"}, nil); err != nil {
			t.Fatalf("%s: CreateContainerWithProgress: %v", app, err)
		}
		ctr, err := c.client.LoadContainer(ctx, app)
		if err != nil {
			t.Fatalf("%s: loading container: %v", app, err)
		}
		labels, err := ctr.Labels(ctx)
		if err != nil {
			t.Fatalf("%s: reading labels: %v", app, err)
		}
		if got, want := labels[labelKeyImageDigest], img.Target().Digest.String(); got != want {
			t.Fatalf("%s: digest label = %q, want %q", app, got, want)
		}
		got, ok := labels[labelKeyReleaseID]
		if releaseID == nil && ok || releaseID != nil && got != release {
			t.Fatalf("%s: release label = %q (set %v), want %v", app, got, ok, releaseID)
		}
	}
}
