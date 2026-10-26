package containerd

import (
	"context"
	"reflect"
	"strings"
	"testing"

	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// runningTasks reports the named containers as running and every other one as
// having no task.
type runningTasks struct {
	tasksapi.TasksClient
	running map[string]bool
}

func (r runningTasks) Get(_ context.Context, req *tasksapi.GetRequest, _ ...grpc.CallOption) (*tasksapi.GetResponse, error) {
	if !r.running[req.ContainerID] {
		return nil, errdefs.ErrNotFound
	}
	return &tasksapi.GetResponse{Process: &tasktypes.Process{ID: req.ContainerID, ContainerID: req.ContainerID, Pid: 1, Status: tasktypes.Status_RUNNING}}, nil
}

func TestReportAppsReadsReleaseIdentityLabels(t *testing.T) {
	const release = "0b6f2c1e-8a3d-4c5e-9f00-1234567890ab"
	labels := func(app, digest string, extra ...string) map[string]string {
		l := map[string]string{labelKeyAppVersion: "1", labelKeyAppID: app, labelKeyImageDigest: digest}
		for i := 0; i+1 < len(extra); i += 2 {
			l[extra[i]] = extra[i+1]
		}
		return l
	}
	remote, err := containerd.New("", containerd.WithServices(
		containerd.WithContainerStore(ros2TestContainerStore{records: []containers.Container{
			{ID: "cam", Labels: labels("cam", "sha256:a", labelKeyReleaseID, release)},
			{ID: "crashed", Labels: labels("crashed", "sha256:b", labelKeyExitCode, "137", labelKeyExitReason, "signal")},
			{ID: "exited", Labels: labels("exited", "sha256:g", labelKeyExitCode, "255", labelKeyExitReason, "crashed")},
			{ID: "multi_api", Labels: labels("multi", "sha256:c", labelKeyReleaseID, release)},
			{ID: "multi_web", Labels: labels("multi", "sha256:d", labelKeyReleaseID, release)},
			{ID: "legacy", Labels: labels("campaign:x", "sha256:e")},
			{ID: "tampered", Labels: labels("tampered", "sha256:f", labelKeyReleaseID, "not-a-uuid")},
		}}),
		containerd.WithTaskClient(runningTasks{running: map[string]bool{"cam": true, "multi_web": true}})))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = remote.Close() }()
	c := &Client{client: remote, namespace: "report-test", logger: zap.NewNop()}

	got, err := c.ReportApps(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []services.AppReport{
		{AppID: "cam", ReleaseID: release, ManifestDigest: "sha256:a", Running: true},
		{AppID: "crashed", ManifestDigest: "sha256:b", ExitSignal: 9},
		// exit(255) is past every signal number: an exit code, not a signal.
		{AppID: "exited", ManifestDigest: "sha256:g"},
		// Services disagree on the image, so no digest; they share a release.
		{AppID: "multi", ReleaseID: release, Running: true},
		// A release id cloud would refuse is dropped, not sent.
		{AppID: "tampered", ManifestDigest: "sha256:f"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reports = %+v\nwant      %+v", got, want)
	}
}

func TestCreateContainerRefusesNonUUIDReleaseID(t *testing.T) {
	c := &Client{logger: zap.NewNop()}
	for _, bad := range []string{"", "latest", "0B6F2C1E-8A3D-4C5E-9F00-1234567890AB", "{0b6f2c1e-8a3d-4c5e-9f00-1234567890ab}"} {
		req := &agentpb.CreateContainerRequest{AppName: "app", ImageName: "img", ReleaseId: proto.String(bad)}
		err := c.createContainer(context.Background(), req, &appconfig.AppConfig{AppID: "app"}, nil, true)
		if err == nil || !strings.Contains(err.Error(), "invalid release id") {
			t.Fatalf("release id %q: err = %v, want invalid release id", bad, err)
		}
	}
}

func TestAddReleaseIdentityLabels(t *testing.T) {
	const release = "0b6f2c1e-8a3d-4c5e-9f00-1234567890ab"
	recorded := map[string]string{}
	addReleaseIdentity(recorded, "sha256:a", proto.String(release))
	if recorded[labelKeyImageDigest] != "sha256:a" || recorded[labelKeyReleaseID] != release {
		t.Fatalf("recorded deploy labels = %v", recorded)
	}
	unrecorded := map[string]string{}
	addReleaseIdentity(unrecorded, "sha256:a", nil)
	if _, ok := unrecorded[labelKeyReleaseID]; ok || unrecorded[labelKeyImageDigest] != "sha256:a" {
		t.Fatalf("unrecorded deploy labels = %v", unrecorded)
	}
}

type oneImageStore struct {
	images.Store
	image images.Image
}

func (s oneImageStore) Get(_ context.Context, name string) (images.Image, error) {
	if name != s.image.Name {
		return images.Image{}, errdefs.ErrNotFound
	}
	return s.image, nil
}

// The digest lookup names the image as create stored it: a chunk-diff deploy
// sends "<app>:latest", which the store holds under its normalized name.
func TestImageDigestNormalizesTheRequestName(t *testing.T) {
	const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000001"
	remote, err := containerd.New("", containerd.WithServices(containerd.WithImageStore(oneImageStore{image: images.Image{
		Name:   "docker.io/library/com.example.cam:latest",
		Target: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest},
	}})))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = remote.Close() }()
	c := &Client{client: remote, namespace: "digest-test", logger: zap.NewNop()}
	if got, err := c.ImageDigest(context.Background(), "com.example.cam:latest"); err != nil || got != digest {
		t.Fatalf("ImageDigest = %q, %v; want %s", got, err, digest)
	}
}
