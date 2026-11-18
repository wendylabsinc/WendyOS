package services

import (
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestEpisodeMediaPresence(t *testing.T) {
	for _, files := range [][]data.EpisodeFileMedia{nil, {{Path: "clip.mp4", MediaType: "video/mp4"}, {Path: "telemetry.jsonl", MediaType: "application/x-ndjson"}}} {
		row := infoEpisode(data.EpisodeInfo{MediaFiles: files})
		raw, err := proto.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		var decoded agentpbv2.DataEpisode
		if err = proto.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.GetMedia() == nil || len(decoded.GetMedia().GetFiles()) != len(files) {
			t.Fatalf("lost presence: %v", &decoded)
		}
		for i, f := range files {
			if got := decoded.GetMedia().GetFiles()[i]; got.GetPath() != f.Path || got.GetMediaType() != f.MediaType {
				t.Fatalf("changed metadata: %v", got)
			}
		}
	}
	if inProgressEpisode(data.InProgressEpisode{}).GetMedia() != nil {
		t.Fatal("unfinished episode cannot claim exhaustive media")
	}
}
