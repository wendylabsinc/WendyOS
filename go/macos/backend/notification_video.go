package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type NotificationVideoTimings struct {
	LookupSeconds     float64 `json:"lookupSeconds"`
	ConnectionSeconds float64 `json:"connectionSeconds"`
	IndexSeconds      float64 `json:"indexSeconds"`
	InspectSeconds    float64 `json:"inspectSeconds"`
	EventsSeconds     float64 `json:"eventsSeconds"`
	VideoSeconds      float64 `json:"videoSeconds"`
	Candidates        int     `json:"candidates"`
	VideoBytes        int64   `json:"videoBytes"`
}

type NotificationVideo struct {
	Timings NotificationVideoTimings
	Episode *pb.DataEpisode
	Preview VisualPreview
}

// Resolve and fetch together: one connection, one manifest and one event-log
// read per candidate, then only the matching MP4. No Gallery poster work.
func (b *Backend) NotificationVideoAt(selector, campaign, eventID string) (NotificationVideo, error) {
	return b.NotificationVideoNear(selector, campaign, eventID, 0, "")
}

func (b *Backend) NotificationVideoNear(selector, campaign, eventID string, notificationUnix float64, hint string) (NotificationVideo, error) {
	if id, err := uuid.Parse(eventID); err != nil || id.String() != eventID || campaign == "" || len(campaign) > 128 {
		return NotificationVideo{}, errors.New("invalid notification recording reference")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	connectionStart := time.Now()
	conn, release, err := b.galleryAcquire(ctx, selector)
	if err != nil {
		return NotificationVideo{}, err
	}
	defer release(nil)
	connectionSeconds := time.Since(connectionStart).Seconds()
	result, err := readNotificationVideoNear(ctx, conn.DataService, campaign, eventID, notificationUnix, hint)
	result.Timings.ConnectionSeconds = connectionSeconds
	return result, err
}

func readNotificationVideo(ctx context.Context, client pb.DataServiceClient, campaign, eventID string) (NotificationVideo, error) {
	return readNotificationVideoNear(ctx, client, campaign, eventID, 0, "")
}

type notificationCandidate struct {
	episode  *pb.DataEpisode
	manifest data.Manifest
	events   []byte
	timings  NotificationVideoTimings
}

func readNotificationVideoNear(ctx context.Context, client pb.DataServiceClient, campaign, eventID string, notificationUnix float64, hint string) (NotificationVideo, error) {
	lookupStart := time.Now()
	timings := NotificationVideoTimings{}
	var match notificationCandidate
	// A saved ID is only a hint: authenticate normally and verify the event again.
	if hint != "" {
		candidate, err := inspectNotificationCandidate(ctx, client, &pb.DataEpisode{Id: hint, Name: campaign}, campaign, eventID)
		if ctx.Err() != nil {
			return NotificationVideo{}, ctx.Err()
		}
		timings.Candidates++
		timings.InspectSeconds += candidate.timings.InspectSeconds
		timings.EventsSeconds += candidate.timings.EventsSeconds
		if err == nil && candidate.episode != nil {
			match = candidate
		}
	}
	if match.episode == nil {
		stageStart := time.Now()
		index, err := recordingEpisodeIndex(ctx, client)
		timings.IndexSeconds = time.Since(stageStart).Seconds()
		if err != nil {
			return NotificationVideo{}, err
		}
		var rows []*pb.DataEpisode
		for _, row := range index.GetEpisodes() {
			if row.GetName() == campaign {
				rows = append(rows, row)
			}
		}
		sort.SliceStable(rows, func(i, j int) bool {
			if notificationUnix > 0 && !math.IsInf(notificationUnix, 0) && !math.IsNaN(notificationUnix) {
				a := math.Abs(float64(rows[i].GetStartedUnixNanos())/1e9 - notificationUnix)
				b := math.Abs(float64(rows[j].GetStartedUnixNanos())/1e9 - notificationUnix)
				if a != b {
					return a < b
				}
			}
			return rows[i].GetStartedUnixNanos() > rows[j].GetStartedUnixNanos()
		})
		var firstError error
		failed := 0
		// Bound device load and preserve priority order, even if responses finish out of order.
		for offset := 0; offset < len(rows); offset += 4 {
			batch := rows[offset:min(offset+4, len(rows))]
			candidates := make([]notificationCandidate, len(batch))
			failures := make([]error, len(batch))
			var workers sync.WaitGroup
			for i, episode := range batch {
				workers.Add(1)
				go func() {
					defer workers.Done()
					candidates[i], failures[i] = inspectNotificationCandidate(ctx, client, episode, campaign, eventID)
				}()
			}
			workers.Wait()
			if err := ctx.Err(); err != nil {
				return NotificationVideo{}, err
			}
			for i, candidate := range candidates {
				timings.Candidates++
				timings.InspectSeconds += candidate.timings.InspectSeconds
				timings.EventsSeconds += candidate.timings.EventsSeconds
				if failures[i] != nil {
					failed++
					if firstError == nil {
						firstError = failures[i]
					}
				}
				if match.episode == nil && candidate.episode != nil {
					match = candidate
				}
			}
			if match.episode != nil {
				break
			}
		}
		if match.episode == nil {
			// An empty result means "not found". Another recording's failure does
			// not explain a missing event unless no recording could be read.
			if failed < len(rows) {
				firstError = nil
			}
			return NotificationVideo{}, firstError
		}
	}
	timings.LookupSeconds = time.Since(lookupStart).Seconds()
	episode, manifest, events := match.episode, match.manifest, match.events
	var err error
	var stageStart time.Time
	start, end := manifest.StartedEpisodeNS, manifest.StoppedEpisodeNS
	for _, source := range manifest.Sources {
		if source.Count > 0 {
			start = min(start, source.ActualOffset)
		}
	}
	if manifest.CaptureStoppedEpisodeNS != 0 {
		end = manifest.CaptureStoppedEpisodeNS
	}
	preview := VisualPreview{Reason: manifest.Trigger.Reason, Duration: (float64(end) - float64(start)) / 1e9, MaximumCelsius: recordedThermalMaximum(events)}
	for _, file := range manifest.Files {
		if file.Role == data.FileRoleDerived && path.Base(file.Path) == "playable.mp4" && strings.HasPrefix(file.Path, "cameras/") && file.Size > 0 && file.Size <= 64<<20 {
			stageStart = time.Now()
			preview.Path, err = visualDownloadVideo(ctx, client, manifest.ID, file)
			timings.VideoSeconds = time.Since(stageStart).Seconds()
			timings.VideoBytes = file.Size
			if err != nil {
				return NotificationVideo{}, err
			}
			return NotificationVideo{Episode: episode, Preview: preview, Timings: timings}, nil
		}
	}
	return NotificationVideo{}, errors.New("the event was recorded, but its playable camera video is unavailable")
}

func inspectNotificationCandidate(ctx context.Context, client pb.DataServiceClient, episode *pb.DataEpisode, campaign, eventID string) (notificationCandidate, error) {
	timings := NotificationVideoTimings{}

	stageStart := time.Now()
	response, err := client.Inspect(ctx, &pb.DataInspectRequest{Episode: episode.GetId()})
	if err != nil {
		return notificationCandidate{}, err
	}
	timings.InspectSeconds += time.Since(stageStart).Seconds()
	if len(response.GetManifestJson()) > visualFileLimit {
		return notificationCandidate{}, errors.New("recording metadata exceeds limit")
	}
	var manifest data.Manifest
	if err := json.Unmarshal(response.GetManifestJson(), &manifest); err != nil {
		return notificationCandidate{}, err
	}
	if manifest.ID != episode.GetId() || manifest.Name != campaign {
		return notificationCandidate{}, errors.New("recording identity mismatch")
	}
	if len(response.GetVerificationErrors()) > 0 {
		return notificationCandidate{}, errors.New("recording verification failed")
	}
	var events []byte
	for _, file := range manifest.Files {
		if file.Path != "events.jsonl" {
			continue
		}
		if file.Size > 2<<20 {
			return notificationCandidate{}, errors.New("recording event log exceeds limit")
		}
		stageStart = time.Now()
		events, err = visualDownload(ctx, client, manifest.ID, file)
		timings.EventsSeconds += time.Since(stageStart).Seconds()
		if err != nil {
			return notificationCandidate{}, err
		}
		break
	}
	if !notificationEventRecorded(events, eventID) {
		return notificationCandidate{timings: timings}, nil
	}
	if episode.GetState() == "" {
		var size int64
		for _, file := range manifest.Files {
			size += file.Size
		}
		episode = &pb.DataEpisode{Id: manifest.ID, Name: manifest.Name, State: manifest.State, StartedUnixNanos: manifest.StartedUnixNanos, SizeBytes: size}
	}
	return notificationCandidate{episode: episode, manifest: manifest, events: events, timings: timings}, nil
}

// Only the two fields that identify the event are decoded, so other attributes
// of any JSON type, and any number of alerts, cannot hide it.
func notificationEventRecorded(events []byte, eventID string) bool {
	scan := bufio.NewScanner(bytes.NewReader(events))
	scan.Buffer(make([]byte, 4096), 2<<20)
	for scan.Scan() {
		var row struct {
			Type       string `json:"type"`
			Attributes struct {
				NotificationID string `json:"notification_id"`
			} `json:"attributes"`
		}
		if json.Unmarshal(scan.Bytes(), &row) == nil && row.Type == "event" && row.Attributes.NotificationID == eventID {
			return true
		}
	}
	return false
}
