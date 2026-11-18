package backend

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"testing"
)

type notificationRecordingFixture struct {
	*visualFixtureClient
	rows []*pb.DataEpisode
}

func (c *notificationRecordingFixture) Episodes(context.Context, *pb.DataEpisodesRequest, ...grpc.CallOption) (*pb.DataEpisodesResponse, error) {
	return &pb.DataEpisodesResponse{Episodes: c.rows}, nil
}
func TestNotificationRecordingMatchesEventAndRejectsCorruption(t *testing.T) {
	const event = "dd97fd1c-db21-476f-b26f-b399d1fe229c"
	raw := []byte(`{"type":"event","attributes":{"notification_id":"` + event + `"}}`)
	manifest := data.Manifest{ID: "clip", Name: "person", Files: []data.File{{Path: "events.jsonl", Size: int64(len(raw)), SHA256: fmt.Sprintf("%x", sha256.Sum256(raw))}}}
	fixture := &notificationRecordingFixture{visualFixtureClient: &visualFixtureClient{manifest: manifest, files: map[string][]byte{"events.jsonl": raw}}, rows: []*pb.DataEpisode{{Id: "clip", Name: "person"}}}
	row, err := readNotificationRecording(context.Background(), fixture, "person", event)
	if err != nil || row == nil || row.ID != "clip" {
		t.Fatalf("matched clip: %+v %v", row, err)
	}
	row, err = readNotificationRecording(context.Background(), fixture, "person", "00000000-0000-0000-0000-000000000000")
	if err != nil || row != nil {
		t.Fatalf("must not open an unrelated latest clip: %+v %v", row, err)
	}
	fixture.mutate = func(chunk *pb.DataDownloadChunk) { chunk.Data[0] ^= 1 }
	if _, err = readNotificationRecording(context.Background(), fixture, "person", event); err == nil {
		t.Fatal("accepted corrupted event log")
	}
	fixture.mutate = nil
	fixture.manifest.ID = "other"
	if _, err = readNotificationRecording(context.Background(), fixture, "person", event); err == nil {
		t.Fatal("accepted another episode manifest")
	}
	fixture.rows = nil
	if row, err = readNotificationRecording(context.Background(), fixture, "person", event); err != nil || row != nil {
		t.Fatalf("pending clip must return no match: %v %v", row, err)
	}
}
func TestRecordingListMediaClassifiesWithoutMarkingLabelsComplete(t *testing.T) {
	for _, tt := range []struct {
		name, kind string
		files      []*pb.DataEpisodeFileMedia
	}{
		{"video", "video", []*pb.DataEpisodeFileMedia{{Path: "cameras/c/segment-0.h264", MediaType: "video/h264"}}},
		{"audio", "audio", []*pb.DataEpisodeFileMedia{{Path: "audio/sample.bin", MediaType: "audio/flac"}}},
		{"ros", "rosbag", []*pb.DataEpisodeFileMedia{{Path: "ros2/domain/topics.mcap"}}},
		{"mixed", "mixed", []*pb.DataEpisodeFileMedia{{Path: "cameras/c/playable.mp4"}, {Path: "audio/a.wav"}}},
		{"snapshots", "images", []*pb.DataEpisodeFileMedia{{Path: "cameras/c/snapshot-1.h264", MediaType: "video/h264"}, {Path: "cameras/c/playable.mp4", MediaType: "video/mp4"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			row := summarizeRecording(&pb.DataEpisode{Id: "clip", Media: &pb.DataEpisodeMedia{Files: tt.files}}, nil)
			if row.Kind != tt.kind || row.DetailsLoaded {
				t.Fatalf("%+v", row)
			}
		})
	}
	if row := summarizeRecording(&pb.DataEpisode{Id: "legacy"}, nil); row.Kind != "" || row.DetailsLoaded {
		t.Fatal("legacy metadata must stay unknown")
	}
}
