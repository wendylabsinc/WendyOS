package backend

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

var recordingsTool = dataChatTool{Tool: chat.Tool{Name: "device_data_recordings", Description: "Find recordings on the original chat device, with episode IDs, campaign and trigger. Filters use recording start time, not overlap. Defaults to the last 24h. Offset pages over a newest-first index; inspect failures and unscanned entries are explicit. Index may change between calls. Use device_data_recording to inspect a clip. Read-only.", Parameters: json.RawMessage(`{"type":"object","properties":{"campaign":{"type":"string"},"start":{"type":"string"},"end":{"type":"string"},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1,"maximum":100}},"additionalProperties":false}`)}, run: readChatRecordings}
var recordingTool = dataChatTool{Tool: chat.Tool{Name: "device_data_recording", Description: "Inspect a specific recording manifest, trigger, available files and timeline. Optionally display an Open recording card. Does not play or interpret video for the model. Files stay on the device until the user opens playback. Read-only.", Parameters: json.RawMessage(`{"type":"object","properties":{"episode":{"type":"string","minLength":1},"show":{"type":"boolean"}},"required":["episode"],"additionalProperties":false}`)}, run: readChatRecording}

func (t *deviceDataTools) manifest(ctx context.Context, id string, verify bool) (*data.Manifest, []string, error) {
	if strings.TrimSpace(id) == "" || len(id) > 256 {
		return nil, nil, errors.New("choose an episode ID returned by the recording list")
	}
	r, err := t.conn.DataService.Inspect(ctx, &pb.DataInspectRequest{Episode: id, Verify: verify})
	if err != nil {
		return nil, nil, err
	}
	if len(r.GetManifestJson()) > visualFileLimit {
		return nil, nil, errors.New("manifest exceeds limit")
	}
	var m data.Manifest
	if err = json.Unmarshal(r.GetManifestJson(), &m); err != nil {
		return nil, nil, err
	}
	if m.ID != id {
		return nil, nil, errors.New("agent returned a different episode")
	}
	return &m, r.GetVerificationErrors(), nil
}
func readChatRecordings(ctx context.Context, t *deviceDataTools, raw json.RawMessage) (any, error) {
	var a struct {
		Campaign string `json:"campaign"`
		Start    string `json:"start"`
		End      string `json:"end"`
		Offset   int    `json:"offset"`
		Limit    int    `json:"limit"`
	}
	if err := decodeDataArgs(raw, &a); err != nil {
		return nil, err
	}
	limit, err := dataLimit(a.Limit)
	if err != nil {
		return nil, err
	}
	if a.Offset < 0 {
		return nil, errors.New("offset must not be negative")
	}
	w, err := parseDataWindow(a.Start, a.End)
	if err != nil {
		return nil, err
	}
	r, err := t.conn.DataService.Episodes(ctx, &pb.DataEpisodesRequest{})
	if err != nil {
		return nil, err
	}
	episodes := append([]*pb.DataEpisode{}, r.GetEpisodes()...)
	episodes = append(episodes, r.GetInProgress()...)
	sort.SliceStable(episodes, func(i, j int) bool {
		if episodes[i].GetStartedUnixNanos() == episodes[j].GetStartedUnixNanos() {
			return episodes[i].GetId() < episodes[j].GetId()
		}
		return episodes[i].GetStartedUnixNanos() > episodes[j].GetStartedUnixNanos()
	})
	start := min(a.Offset, len(episodes))
	end := start + min(limit, len(episodes)-start)
	rows := []any{}
	unavailable := []map[string]string{}
	for _, e := range episodes[start:end] {
		if e.GetStartedUnixNanos() == 0 {
			unavailable = append(unavailable, map[string]string{"episode": e.GetId(), "reason": "start timestamp unavailable"})
			continue
		}
		if !w.includes(time.Unix(0, e.GetStartedUnixNanos())) {
			continue
		}
		m, _, err := t.manifest(ctx, e.GetId(), false)
		if err != nil {
			unavailable = append(unavailable, map[string]string{"episode": e.GetId(), "reason": err.Error()})
			continue
		}
		if a.Campaign != "" && m.Trigger.CampaignName != a.Campaign {
			continue
		}
		rows = append(rows, map[string]any{"episode": m.ID, "name": m.Name, "state": m.State, "started_at": time.Unix(0, m.StartedUnixNanos).UTC().Format(time.RFC3339Nano), "trigger": chatTriggerEvidence(m.Trigger), "duration_seconds": savedDuration(m), "size_bytes": e.GetSizeBytes(), "upload": m.Upload})
	}
	var next *int
	if end < len(episodes) {
		next = &end
	}
	return map[string]any{"recordings": rows, "window": w.evidence(), "unavailable": unavailable, "next_offset": next, "index_entries": len(episodes), "reports_in_progress": r.GetReportsInProgress(), "coverage": "Local device index only; deleted or cloud-only episodes are not included. A recording may overlap a window even if it started earlier. Retry unavailable manifests before drawing conclusions."}, nil
}
func readChatRecording(ctx context.Context, t *deviceDataTools, raw json.RawMessage) (any, error) {
	var a struct {
		Episode string `json:"episode"`
		Show    bool   `json:"show"`
	}
	if err := decodeDataArgs(raw, &a); err != nil {
		return nil, err
	}
	m, _, err := t.manifest(ctx, a.Episode, false)
	if err != nil {
		return nil, err
	}
	shown := false
	if a.Show {
		if t.turn == nil {
			return nil, errors.New("recording cards require an active native chat turn")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		t.turn.mu.Lock()
		for _, c := range t.turn.Cards {
			if c.Kind == "recording" && c.Episode == m.ID {
				shown = true
			}
		}
		if !shown && len(t.turn.Cards) < 6 {
			title := m.Name
			if title == "" {
				title = "Recording"
			}
			t.turn.Cards = append(t.turn.Cards, ChatCard{Kind: "recording", Title: title, Episode: m.ID})
			shown = true
		}
		t.turn.mu.Unlock()
		if !shown {
			return nil, errors.New("this response already has six cards")
		}
	}
	files := m.Files
	if len(files) > 100 {
		files = files[:100]
	}
	return map[string]any{"episode": m.ID, "name": m.Name, "state": m.State, "trigger": chatTriggerEvidence(m.Trigger), "boot_id": m.BootID, "canonical_clock": m.CanonicalClock, "request_boottime_nanos": m.RequestBootNanos, "started_unix_nanos": m.StartedUnixNanos, "start_episode_nanos": m.StartedEpisodeNS, "capture_stop_episode_nanos": m.CaptureStoppedEpisodeNS, "stop_episode_nanos": m.StoppedEpisodeNS, "duration_seconds": savedDuration(m), "files": files, "files_truncated": len(m.Files) > len(files), "sources": m.Sources, "playable_notes": m.PlayableNotes, "card_shown": shown, "verification": "Manifest read only; file contents not verified by this call. A matching trigger and nearby time are not proof that a particular event caused this episode."}, nil
}

// Notification webhook addresses may contain credentials. Chat only needs the
// trigger and notification policy, never the delivery endpoint.
func chatTriggerEvidence(trigger data.EpisodeTrigger) map[string]any {
	out := map[string]any{"reason": trigger.Reason, "campaign_name": trigger.CampaignName, "campaign_revision": trigger.CampaignRevision, "expression": trigger.Expression}
	if trigger.Notify != nil {
		out["notification_policy"] = map[string]any{"on": trigger.Notify.On, "event": trigger.Notify.Event, "webhook_configured": trigger.Notify.Webhook != ""}
	}
	return out
}
