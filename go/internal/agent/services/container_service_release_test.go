package services

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type digestMock struct{ *mockContainerdClient }

func (digestMock) ImageDigest(context.Context, string) (string, error) { return "sha256:feed", nil }

// A CLI that predates release_id sends no field 9: the create goes through
// unchanged, as an unrecorded deploy, and the response carries the digest.
func TestCreateContainer_WithoutReleaseIDIsUnrecorded(t *testing.T) {
	mock := &mockContainerdClient{}
	client, cleanup := startContainerServer(t, digestMock{mock})
	defer cleanup()

	resp, err := client.CreateContainer(context.Background(), &agentpb.CreateContainerRequest{ImageName: "img", AppName: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if len(mock.createReqs) != 1 || mock.createReqs[0].ReleaseId != nil {
		t.Fatalf("create requests = %v, want one without a release id", mock.createReqs)
	}
	if resp.GetImageDigest() != "sha256:feed" {
		t.Fatalf("image digest = %q, want sha256:feed", resp.GetImageDigest())
	}
}

// RunContainer forwards a recorded deploy's release id to the create and puts
// the digest in its Started message.
func TestRunContainer_ForwardsReleaseIDAndReturnsDigest(t *testing.T) {
	outputCh := make(chan ContainerOutput, 1)
	outputCh <- ContainerOutput{Done: true}
	close(outputCh)
	mock := &mockContainerdClient{startOutputCh: outputCh}
	client, cleanup := startContainerServer(t, digestMock{mock})
	defer cleanup()

	const release = "0b6f2c1e-8a3d-4c5e-9f00-1234567890ab"
	stream, err := client.RunContainer(context.Background(), &agentpb.RunContainerLayersRequest{
		ImageName:   "img",
		AppName:     "app",
		Layers:      []*agentpb.RunContainerLayerHeader{{Digest: "sha256:layer", Size: 10, DiffId: "sha256:diff"}},
		ImageConfig: []byte(`{}`),
		ReleaseId:   proto.String(release),
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if got := first.GetStarted().GetImageDigest(); got != "sha256:feed" {
		t.Fatalf("Started image digest = %q, want sha256:feed", got)
	}
	if err := drainRunContainerStream(stream); err != nil {
		t.Fatal(err)
	}
	if len(mock.createReqs) != 1 || mock.createReqs[0].GetReleaseId() != release {
		t.Fatalf("create requests = %v, want release id %s", mock.createReqs, release)
	}
}
