package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

var galleryMedia = map[string][]*pb.DataEpisodeFileMedia{
	"video":   {{Path: "cameras/c/playable.mp4", MediaType: "video/mp4"}},
	"audio":   {{Path: "audio/mic.wav", MediaType: "audio/wav"}},
	"ros":     {{Path: "ros2/0/data.mcap"}},
	"mixed":   {{Path: "cameras/c/playable.mp4", MediaType: "video/mp4"}, {Path: "audio/mic.wav", MediaType: "audio/wav"}},
	"images":  {{Path: "cameras/c/snapshot-1.jpg", MediaType: "image/jpeg"}},
	"dataset": {{Path: "telemetry.jsonl", MediaType: "application/x-ndjson"}},
}

func galleryEpisodes() *pb.DataEpisodesResponse {
	rows := []*pb.DataEpisode{}
	for _, kind := range []string{"video", "audio", "ros", "mixed", "images", "dataset"} {
		rows = append(rows, &pb.DataEpisode{Id: "media-" + kind, Name: "QA " + kind, SizeBytes: 100, StartedUnixNanos: 1793995200000000000, Media: &pb.DataEpisodeMedia{Files: galleryMedia[kind]}})
	}
	return &pb.DataEpisodesResponse{Episodes: rows}
}
func galleryInspect(ctx context.Context, req *pb.DataInspectRequest) (*pb.DataInspectResponse, error) {
	// Type filters must work from the index while richer metadata is still slow.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(5 * time.Second):
	}
	var m data.Manifest
	for _, r := range galleryEpisodes().Episodes {
		if r.Id == req.Episode {
			m.ID = r.Id
			m.Name = r.Name
			m.State = "complete"
			for _, f := range r.Media.Files {
				m.Files = append(m.Files, data.File{Path: f.Path, MediaType: f.MediaType, Size: 1})
			}
		}
	}
	raw, err := json.Marshal(m)
	return &pb.DataInspectResponse{ManifestJson: raw}, err
}
