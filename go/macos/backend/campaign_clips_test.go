package backend

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
)

type campaignClipClient struct {
	pb.DataServiceClient
	episodes  []*pb.DataEpisode
	manifests map[string]data.Manifest
}

func (c *campaignClipClient) Episodes(ctx context.Context, _ *pb.DataEpisodesRequest, _ ...grpc.CallOption) (*pb.DataEpisodesResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &pb.DataEpisodesResponse{Episodes: c.episodes}, nil
}
func (c *campaignClipClient) Inspect(_ context.Context, req *pb.DataInspectRequest, _ ...grpc.CallOption) (*pb.DataInspectResponse, error) {
	m, ok := c.manifests[req.GetEpisode()]
	if !ok {
		return nil, errors.New("recording removed")
	}
	raw, _ := json.Marshal(m)
	return &pb.DataInspectResponse{ManifestJson: raw}, nil
}
func TestCampaignClipsUseRecordedAssociationAndStableNewestOrder(t *testing.T) {
	c := &campaignClipClient{episodes: []*pb.DataEpisode{
		{Id: "a", Name: "misleading-name", SizeBytes: 123}, {Id: "b", Name: "people"}, {Id: "c", Name: "people"}, {Id: "manual", Name: "people"},
	}, manifests: map[string]data.Manifest{
		"a":      {ID: "a", State: "complete", StartedUnixNanos: 2, StartedEpisodeNS: -2e9, StoppedEpisodeNS: 3e9, Trigger: data.EpisodeTrigger{CampaignName: "people", Reason: "person"}},
		"b":      {ID: "b", State: "recording", StartedUnixNanos: 3, Trigger: data.EpisodeTrigger{CampaignName: "animals"}},
		"c":      {ID: "c", State: "interrupted", StartedUnixNanos: 2, Trigger: data.EpisodeTrigger{CampaignName: "people"}},
		"manual": {ID: "manual", Name: "people"},
	}}
	got, err := readCampaignClips(context.Background(), c)
	if err != nil || got.Warning != "" || len(got.Items) != 3 {
		t.Fatalf("%+v %v", got, err)
	}
	if got.Items[0].ID != "b" || got.Items[1].ID != "a" || got.Items[2].ID != "c" {
		t.Fatalf("bad order: %+v", got.Items)
	}
	if got.Items[0].Duration != -1 || got.Items[1].Duration != 5 || got.Items[1].Size != 123 || got.Items[1].Campaign != "people" {
		t.Fatalf("bad metadata: %+v", got.Items)
	}
}
func TestCampaignClipsReportIncompleteCountsAndRejectWrongManifest(t *testing.T) {
	c := &campaignClipClient{episodes: []*pb.DataEpisode{{Id: "missing"}, {Id: "wrong"}}, manifests: map[string]data.Manifest{
		"wrong": {ID: "some-other-episode", Trigger: data.EpisodeTrigger{CampaignName: "people"}},
	}}
	got, err := readCampaignClips(context.Background(), c)
	if err != nil || len(got.Items) != 0 || got.Warning == "" {
		t.Fatalf("must not report an exact zero: %+v %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = readCampaignClips(ctx, c); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestCampaignClipDurationIncludesRetainedPrerollAndExcludesSealDrain(t *testing.T) {
	c := &campaignClipClient{episodes: []*pb.DataEpisode{{Id: "clip"}}, manifests: map[string]data.Manifest{
		"clip": {ID: "clip", State: "complete", StoppedEpisodeNS: 25e9, CaptureStoppedEpisodeNS: 20e9,
			Sources: []data.SourceStats{{Count: 12, ActualOffset: -10e9}, {Count: 0, ActualOffset: -60e9}},
			Trigger: data.EpisodeTrigger{CampaignName: "people"}},
	}}
	got, err := readCampaignClips(context.Background(), c)
	if err != nil || len(got.Items) != 1 || got.Items[0].Duration != 30 {
		t.Fatalf("incorrect capture duration: %+v %v", got, err)
	}
}
