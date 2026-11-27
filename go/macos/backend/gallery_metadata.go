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
	MaximumCelsius  *float64 `json:"maximumCelsius,omitempty"`
	NotificationIDs []string `json:"notificationIDs,omitempty"`
	Campaign        string   `json:"campaign,omitempty"`
	Types           []string `json:"types"`
	Labels          []string `json:"labels"`
	Complete        bool     `json:"complete"`
	ContentKinds    []string `json:"contentKinds"`
	HasVisualMedia  bool     `json:"hasVisualMedia"`
}

func (b *Backend) GalleryMetadataAt(selector, episode string) (GalleryMetadata, error) {
	return b.galleryMetadataAt(selector, episode, false)
}

// GalleryMediaAt reads only the manifest; content filtering never needs event logs.
func (b *Backend) GalleryMediaAt(selector, episode string) (GalleryMetadata, error) {
	return b.galleryMetadataAt(selector, episode, true)
}

func (b *Backend) galleryMetadataAt(selector, episode string, mediaOnly bool) (GalleryMetadata, error) {
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
	if mediaOnly {
		return GalleryMetadata{Types: []string{}, Labels: []string{}, Complete: true,
			ContentKinds: galleryContentKinds(manifest), HasVisualMedia: galleryHasVisualMedia(manifest)}, nil
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
	metadata.Campaign = manifest.Trigger.CampaignName
	metadata.HasVisualMedia = galleryHasVisualMedia(manifest)
	metadata.ContentKinds = galleryContentKinds(manifest)
	return metadata, nil
}

func galleryMetadata(reason string, events []byte, complete bool) GalleryMetadata {
	types, labels := map[string]bool{}, map[string]bool{}
	notificationIDs := map[string]bool{}
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
				NotificationID string   `json:"notification_id"`
				Task           string   `json:"task"`
				Maximum        *float64 `json:"max_celsius"`
				Detections     []struct {
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
			if id := row.Attributes.NotificationID; len(id) == 36 && len(notificationIDs) < 256 {
				notificationIDs[id] = true
			}
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
	out := GalleryMetadata{Types: []string{}, Labels: []string{}, Complete: complete, MaximumCelsius: recordedThermalMaximum(events)}
	for kind := range types {
		out.Types = append(out.Types, kind)
	}
	sort.Strings(out.Types)
	for label := range labels {
		out.Labels = append(out.Labels, label)
	}
	sort.Strings(out.Labels)
	for id := range notificationIDs {
		out.NotificationIDs = append(out.NotificationIDs, id)
	}
	sort.Strings(out.NotificationIDs)
	return out
}

// Inspect stored payloads rather than inferring media from the trigger or name.
func galleryHasVisualMedia(manifest data.Manifest) bool {
	for _, kind := range galleryContentKinds(manifest) {
		if kind == "Video" || kind == "Images" {
			return true
		}
	}
	return false
}

func galleryContentKinds(manifest data.Manifest) []string {
	found := map[string]bool{}
	for _, f := range manifest.Files {
		media := strings.ToLower(strings.TrimSpace(strings.SplitN(f.MediaType, ";", 2)[0]))
		if strings.HasPrefix(media, "video/") {
			found["Video"] = true
		}
		if strings.HasPrefix(media, "image/") {
			found["Images"] = true
		}
		if strings.HasPrefix(media, "audio/") {
			found["Audio"] = true
		}
		if f.Path == "telemetry.jsonl" {
			found["Device stats"] = true
		}
		if f.Path == "events.jsonl" {
			found["Events"] = true
		}
		if strings.HasPrefix(f.Path, "ros2/") {
			found["Robot data"] = true
		}
		if media == "" || media == "application/octet-stream" {
			p := strings.ToLower(f.Path)
			switch {
			case strings.HasSuffix(p, ".mp4"), strings.HasSuffix(p, ".h264"), strings.HasSuffix(p, ".h265"), strings.HasSuffix(p, ".hevc"), strings.HasSuffix(p, ".mov"), strings.HasSuffix(p, ".mjpeg"):
				found["Video"] = true
			case strings.HasSuffix(p, ".jpg"), strings.HasSuffix(p, ".jpeg"), strings.HasSuffix(p, ".png"):
				found["Images"] = true
			case strings.HasSuffix(p, ".wav"):
				found["Audio"] = true
			}
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
