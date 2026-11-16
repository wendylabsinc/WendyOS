package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type GalleryMetadata struct {
	Types          []string `json:"types"`
	Labels         []string `json:"labels"`
	Complete       bool     `json:"complete"`
	ContentKinds   []string `json:"contentKinds"`
	HasVisualMedia bool     `json:"hasVisualMedia"`
}

func (b *Backend) GalleryMetadataAt(selector, episode string) (GalleryMetadata, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, release, err := b.galleryAcquire(ctx, selector)
	if err != nil {
		return GalleryMetadata{}, err
	}
	// A malformed recording is not evidence of a broken transport.
	defer release(nil)
	result, err := conn.DataService.Inspect(ctx, &pb.DataInspectRequest{Episode: episode})
	if err != nil {
		return GalleryMetadata{}, err
	}
	if len(result.GetManifestJson()) > visualFileLimit {
		return GalleryMetadata{}, errors.New("recording metadata exceeds limit")
	}
	var manifest data.Manifest
	if err := json.Unmarshal(result.GetManifestJson(), &manifest); err != nil {
		return GalleryMetadata{}, err
	}
	if manifest.ID != episode {
		return GalleryMetadata{}, errors.New("recording identity mismatch")
	}
	var events []byte
	complete := true
	for _, file := range manifest.Files {
		if file.Path != "events.jsonl" {
			continue
		}
		if file.Size > 2<<20 {
			complete = false
			break
		}
		events, err = visualDownload(ctx, conn.DataService, episode, file)
		if err != nil {
			complete = false
		}
		break
	}
	reason := manifest.Trigger.Reason
	if manifest.Trigger.Expression == "manual" {
		reason = "manual"
	}
	metadata := galleryMetadata(reason, events, complete)
	metadata.HasVisualMedia = galleryHasVisualMedia(manifest)
	metadata.ContentKinds = galleryContentKinds(manifest)
	return metadata, nil
}

func galleryMetadata(reason string, events []byte, complete bool) GalleryMetadata {
	types, labels := map[string]bool{}, map[string]bool{}
	addLabel := func(label string) {
		if label != "" && len(label) <= 256 && len(labels) < 128 {
			labels[label] = true
		}
	}
	if reason == "manual" || strings.HasPrefix(reason, "manual:") {
		types["Manual recording"] = true
	}
	if strings.HasPrefix(reason, "event:") {
		types["Event"] = true
		addLabel(strings.TrimPrefix(reason, "event:"))
	}
	scan := bufio.NewScanner(bytes.NewReader(events))
	scan.Buffer(make([]byte, 4096), 2<<20)
	for scan.Scan() {
		var row struct {
			Type       string `json:"type"`
			Name       string `json:"name"`
			Attributes struct {
				Task       string   `json:"task"`
				Maximum    *float64 `json:"max_celsius"`
				Detections []struct {
					Label string `json:"label"`
				} `json:"detections"`
				Label  string   `json:"label"`
				Labels []string `json:"labels"`
			} `json:"attributes"`
		}
		if json.Unmarshal(scan.Bytes(), &row) != nil {
			complete = false
			continue
		}
		if row.Type == "event" {
			types["Event"] = true
			addLabel(row.Name)
		}
		if row.Attributes.Maximum != nil {
			types["Thermal"] = true
		}
		if len(row.Attributes.Detections) > 0 {
			if row.Attributes.Task == "image-classification" {
				types["Image classification"] = true
			} else {
				types["Object detection"] = true
			}
		}
		for _, detection := range row.Attributes.Detections {
			addLabel(detection.Label)
		}
		addLabel(row.Attributes.Label)
		for _, label := range row.Attributes.Labels {
			addLabel(label)
		}
	}
	if scan.Err() != nil {
		complete = false
	}
	if len(types) == 0 && complete {
		types["Other recording"] = true
	}
	out := GalleryMetadata{Types: []string{}, Labels: []string{}, Complete: complete}
	for kind := range types {
		out.Types = append(out.Types, kind)
	}
	sort.Strings(out.Types)
	for label := range labels {
		out.Labels = append(out.Labels, label)
	}
	sort.Strings(out.Labels)
	return out
}

// Inspect stored payloads rather than inferring media from the trigger or name.
func galleryHasVisualMedia(manifest data.Manifest) bool {
	for _, file := range manifest.Files {
		p := strings.ToLower(file.Path)
		media := strings.ToLower(file.MediaType)
		if strings.HasPrefix(media, "video/") || strings.HasPrefix(media, "image/") {
			return true
		}
		for _, ext := range []string{".mp4", ".mov", ".h264", ".h265", ".hevc", ".mjpeg", ".jpg", ".jpeg", ".png"} {
			if strings.HasSuffix(p, ext) {
				return true
			}
		}
	}
	return false
}

func galleryContentKinds(manifest data.Manifest) []string {
	found := map[string]bool{}
	for _, f := range manifest.Files {
		p, media := strings.ToLower(f.Path), strings.ToLower(f.MediaType)
		switch {
		case strings.HasPrefix(media, "video/") || strings.HasSuffix(p, ".mp4") || strings.HasSuffix(p, ".h264") || strings.HasSuffix(p, ".h265") || strings.HasSuffix(p, ".hevc") || strings.HasSuffix(p, ".mov") || strings.HasSuffix(p, ".mjpeg"):
			found["Video"] = true
		case strings.HasPrefix(media, "image/") || strings.HasSuffix(p, ".jpg") || strings.HasSuffix(p, ".jpeg") || strings.HasSuffix(p, ".png"):
			found["Images"] = true
		case strings.HasPrefix(media, "audio/") || strings.HasSuffix(p, ".wav"):
			found["Audio"] = true
		case p == "telemetry.jsonl":
			found["Device stats"] = true
		case p == "events.jsonl":
			found["Events"] = true
		case strings.HasPrefix(p, "ros2/") && (strings.HasSuffix(p, ".db3") || strings.HasSuffix(p, ".mcap")):
			found["Robot data"] = true
		}
	}
	out := []string{}
	for _, kind := range []string{"Video", "Images", "Audio", "Device stats", "Events", "Robot data"} {
		if found[kind] {
			out = append(out, kind)
		}
	}
	return out
}
