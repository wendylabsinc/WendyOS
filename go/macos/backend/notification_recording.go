package backend

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// Resolve the exact event, never the latest clip with a similar name. A missing
// finalized episode is a pending result; the UI can retry while it is sealing.
func (b *Backend) NotificationRecording(id uint64, selector string, secure bool, campaign, eventID string) (result RecordingLibrary, err error) {
	if parsed, e := uuid.Parse(eventID); e != nil || parsed.String() != eventID || campaign == "" || len(campaign) > 128 {
		return result, errors.New("invalid notification recording reference")
	}
	ctx, conn, done, err := b.recordingConnection(id, selector, secure, 10*time.Second)
	if err != nil {
		return result, err
	}
	defer func() { done(err) }()
	row, err := readNotificationRecording(ctx, conn.DataService, campaign, eventID)
	if row != nil {
		result.Episodes = []RecordingEpisode{*row}
	}
	return result, err
}
func readNotificationRecording(ctx context.Context, client pb.DataServiceClient, campaign, eventID string) (*RecordingEpisode, error) {
	index, err := recordingEpisodeIndex(ctx, client)
	if err != nil {
		return nil, err
	}
	rows := append([]*pb.DataEpisode(nil), index.GetEpisodes()...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].GetStartedUnixNanos() > rows[j].GetStartedUnixNanos() })
	for _, episode := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if episode.GetName() != campaign {
			continue
		}
		response, err := client.Inspect(ctx, &pb.DataInspectRequest{Episode: episode.GetId()})
		if err != nil {
			return nil, err
		}
		if len(response.GetManifestJson()) > visualFileLimit {
			return nil, errors.New("recording metadata exceeds limit")
		}
		var manifest data.Manifest
		if err = json.Unmarshal(response.GetManifestJson(), &manifest); err != nil {
			return nil, err
		}
		if manifest.ID != episode.GetId() || manifest.Name != campaign {
			return nil, errors.New("recording identity mismatch")
		}
		if len(response.GetVerificationErrors()) > 0 {
			return nil, errors.New("recording verification failed")
		}
		for _, file := range manifest.Files {
			if file.Path != "events.jsonl" {
				continue
			}
			if file.Size > 2<<20 {
				return nil, errors.New("recording event log exceeds limit")
			}
			events, err := visualDownload(ctx, client, manifest.ID, file)
			if err != nil {
				return nil, err
			}
			if slices.Contains(galleryMetadata("", events, true).NotificationIDs, eventID) {
				row := summarizeRecording(episode, &manifest)
				return &row, nil
			}
		}
	}
	return nil, nil
}
