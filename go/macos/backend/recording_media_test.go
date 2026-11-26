package backend

import (
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"testing"
)

func TestRecordingContentKeepsKnownEmptySeparateFromUnknown(t *testing.T) {
	unknown := summarizeRecording(&pb.DataEpisode{Id: "old"}, nil)
	empty := summarizeRecording(&pb.DataEpisode{Id: "empty", Media: &pb.DataEpisodeMedia{}}, nil)
	video := summarizeRecording(&pb.DataEpisode{Id: "video", Media: &pb.DataEpisodeMedia{Files: []*pb.DataEpisodeFileMedia{{Path: "opaque.bin", MediaType: "video/mp4"}}}}, nil)
	if unknown.ContentKinds != nil {
		t.Fatal("old agent metadata should be unknown")
	}
	if empty.ContentKinds == nil || len(*empty.ContentKinds) != 0 {
		t.Fatal("empty metadata should be known empty")
	}
	if video.ContentKinds == nil || len(*video.ContentKinds) != 1 || (*video.ContentKinds)[0] != "Video" {
		t.Fatalf("video metadata: %+v", video)
	}
}
