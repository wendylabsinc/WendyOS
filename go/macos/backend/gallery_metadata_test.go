package backend

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
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

func TestGalleryNotificationIDsComeFromRecordedEvents(t *testing.T) {
	const id = "a4c0899e-f2c0-4c60-a42d-5c89f756be55"
	events := []byte(`{"type":"event","name":"thermal_recording_test","attributes":{"notification_id":"` + id + `","max_celsius":27.5}}
{"type":"event","attributes":{"notification_id":"` + id + `"}}
{"type":"prediction","attributes":{"notification_id":"0cdfb0f4-fd00-43bb-a821-ee28ed0b6cc9"}}`)
	got := galleryMetadata("event:thermal_recording_test", events, true)
	if !slices.Equal(got.NotificationIDs, []string{id}) || !got.Complete {
		t.Fatalf("%+v", got)
	}
	if got := galleryMetadata(id, nil, true); len(got.NotificationIDs) != 0 {
		t.Fatal("inferred a recording association from its title")
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

func TestGalleryMediaReadsManifestWithoutDownloadingEvents(t *testing.T) {
	b := New()
	defer b.galleryReset(true)
	// This client supports Inspect only: any attempt to download events fails the test.
	client := &campaignClipClient{manifests: map[string]data.Manifest{
		"episode": {ID: "episode", Files: []data.File{{Path: "events.jsonl", Size: 100}, {Path: "camera/part.mp4"}}},
		"wrong":   {ID: "different"},
	}}
	b.connectTarget = func(context.Context, string) (*grpcclient.AgentConnection, liteConnection, error) {
		return &grpcclient.AgentConnection{DataService: client}, nil, nil
	}
	result, err := b.GalleryMediaAt("device", "episode")
	if err != nil || !result.Complete || !slices.Contains(result.ContentKinds, "Video") || len(result.Types) != 0 || len(result.Labels) != 0 {
		t.Fatalf("%+v %v", result, err)
	}
	if _, err := b.GalleryMediaAt("device", "wrong"); err == nil {
		t.Fatal("accepted mismatched manifest")
	}
}

func TestGalleryContentMimeIsAuthoritativeAndPathsIdentifyStatistics(t *testing.T) {
	for _, tc := range []struct {
		file data.File
		want []string
	}{
		{data.File{Path: "misleading.mp4", MediaType: "audio/wav"}, []string{"Audio"}},
		{data.File{Path: "capture.bin", MediaType: " Video/MP4 ; codecs=avc1"}, []string{"Video"}},
		{data.File{Path: "telemetry.jsonl", MediaType: "application/x-ndjson"}, []string{"Device stats"}},
		{data.File{Path: "other.jsonl", MediaType: "application/x-ndjson"}, []string{}},
		{data.File{Path: "capture.MP4", MediaType: "application/octet-stream"}, []string{"Video"}},
		{data.File{Path: "capture.mp4", MediaType: "application/json"}, []string{}},
	} {
		got := galleryContentKinds(data.Manifest{Files: []data.File{tc.file}})
		if !slices.Equal(got, tc.want) {
			t.Errorf("%+v: got %v want %v", tc.file, got, tc.want)
		}
	}
}
