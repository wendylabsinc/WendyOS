package backend

import (
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"slices"
	"testing"
)

func TestGalleryMetadataUsesRecordedEvidence(t *testing.T) {
	m := galleryMetadata("event:person_detected", []byte(`{"type":"prediction","attributes":{"detections":[{"label":"person"}]}}`), true)
	if !slices.Contains(m.Types, "Object detection") || !slices.Contains(m.Labels, "person") || !m.Complete {
		t.Fatalf("%+v", m)
	}
	thermal := galleryMetadata("event:temperature_high", []byte(`{"type":"event","name":"temperature_high","attributes":{"max_celsius":31.9}}`), true)
	if !slices.Contains(thermal.Types, "Thermal") {
		t.Fatalf("%+v", thermal)
	}
	unknown := galleryMetadata("thermal-person-campaign-name", nil, false)
	if len(unknown.Types) != 0 || unknown.Complete {
		t.Fatalf("guessed metadata: %+v", unknown)
	}
	malformed := galleryMetadata("manual", []byte(`not-json`), true)
	if malformed.Complete || !slices.Contains(malformed.Types, "Manual recording") {
		t.Fatalf("%+v", malformed)
	}
}

func TestGalleryMediaEvidence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []data.File
		want  bool
	}{
		{"telemetry and audio", []data.File{{Path: "telemetry.jsonl"}, {Path: "audio/segment.wav", MediaType: "audio/wav"}}, false},
		{"camera index without frames", []data.File{{Path: "cameras/camera/index.jsonl"}}, false},
		{"raw camera", []data.File{{Path: "cameras/camera/segment.h264"}}, true},
		{"playable", []data.File{{Path: "cameras/camera/playable.mp4"}}, true},
		{"snapshot", []data.File{{Path: "frame.jpg"}}, true},
		{"typed payload", []data.File{{Path: "frame.bin", MediaType: "image/jpeg"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := galleryHasVisualMedia(data.Manifest{Files: tc.files}); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestGalleryContentBadgesUsePayloads(t *testing.T) {
	manifest := data.Manifest{Files: []data.File{
		{Path: "audio/mic/index.jsonl"}, {Path: "audio/mic/part.wav"},
		{Path: "telemetry.jsonl"}, {Path: "events.jsonl"}, {Path: "ros2/bag.db3"},
	}}
	got := galleryContentKinds(manifest)
	if !slices.Equal(got, []string{"Audio", "Device stats", "Events", "Robot data"}) {
		t.Fatalf("%v", got)
	}
	if got := galleryContentKinds(data.Manifest{Files: []data.File{{Path: "cameras/c/index.jsonl"}}}); len(got) != 0 {
		t.Fatalf("invented content: %v", got)
	}
}
