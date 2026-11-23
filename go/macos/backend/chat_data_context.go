package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path"
	"sort"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
)

var samplesTool = dataChatTool{Tool: chat.Tool{Name: "device_data_samples", Description: "Read recorded telemetry, application events/log streams, or supported ROS messages around an episode-relative time. Verify downloaded file hashes; never infer past readings from current metrics. No video or audio downloads. Bounded to 16 MiB total and 20,000 decoded samples; omissions explicit. Missing/unsupported data is unknown, not zero. Numeric summaries cover decoded matching samples only. Read-only.", Parameters: json.RawMessage(`{"type":"object","properties":{"episode":{"type":"string","minLength":1},"kind":{"type":"string","enum":["telemetry","events","streams","ros2"]},"track":{"type":"string","description":"Exact track ID from a previous response; omitted lists all"},"start_seconds":{"type":"number","description":"Inclusive episode-relative seconds; negative pre-roll allowed"},"end_seconds":{"type":"number","description":"Exclusive episode-relative seconds"},"limit":{"type":"integer","minimum":1,"maximum":100}},"required":["episode","kind"],"additionalProperties":false}`)}, run: readChatSamples}

type chatSample struct {
	Track           string   `json:"track"`
	EpisodeSeconds  float64  `json:"episode_seconds"`
	Label           string   `json:"label,omitempty"`
	Detail          string   `json:"detail,omitempty"`
	DetailTruncated bool     `json:"detail_truncated,omitempty"`
	Value           *float64 `json:"value,omitempty"`
	Unit            string   `json:"unit,omitempty"`
}
type chatNumericSummary struct {
	Count int     `json:"count"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Mean  float64 `json:"mean"`
	Unit  string  `json:"unit"`
}

func readChatSamples(ctx context.Context, t *deviceDataTools, raw json.RawMessage) (any, error) {
	var a struct {
		Episode string   `json:"episode"`
		Kind    string   `json:"kind"`
		Track   string   `json:"track"`
		Start   *float64 `json:"start_seconds"`
		End     *float64 `json:"end_seconds"`
		Limit   int      `json:"limit"`
	}
	if err := decodeDataArgs(raw, &a); err != nil {
		return nil, err
	}
	limit, err := dataLimit(a.Limit)
	if err != nil {
		return nil, err
	}
	if a.Kind != "telemetry" && a.Kind != "events" && a.Kind != "streams" && a.Kind != "ros2" {
		return nil, errors.New("choose telemetry, events, streams, or ros2")
	}
	for _, bound := range []*float64{a.Start, a.End} {
		if bound != nil && (!visualFinite(*bound) || math.Abs(*bound) > 9e9) {
			return nil, errors.New("invalid episode-relative time")
		}
	}
	if a.Start != nil && a.End != nil && *a.Start >= *a.End {
		return nil, errors.New("start_seconds must precede end_seconds")
	}
	m, _, err := t.manifest(ctx, a.Episode, false)
	if err != nil {
		return nil, err
	}
	if m.CanonicalClock != "CLOCK_BOOTTIME" {
		return nil, errors.New("recording has no supported canonical clock")
	}
	snapshot := &VisualSnapshot{}
	warnings := []string{}
	filesRead := []string{}
	selected := *m
	selected.Files = nil
	budget := int64(16 << 20)
	for _, f := range m.Files {
		wanted := a.Kind == "telemetry" && f.Path == "telemetry.jsonl" || a.Kind == "events" && f.Path == "events.jsonl" || a.Kind == "streams" && f.Path == data.RecordingLogFile || a.Kind == "ros2" && strings.HasPrefix(f.Path, "ros2/") && (path.Ext(f.Path) == ".db3" || path.Ext(f.Path) == ".mcap" || strings.HasSuffix(f.Path, "-clock_samples.jsonl"))
		if !wanted {
			continue
		}
		if f.Size < 0 || f.Size > 8<<20 || f.Size > budget {
			warnings = append(warnings, f.Path+": omitted; exceeds chat download budget")
			continue
		}
		budget -= f.Size
		if a.Kind == "ros2" {
			selected.Files = append(selected.Files, f)
			continue
		}
		payload, err := visualDownload(ctx, t.conn.DataService, a.Episode, f)
		if err != nil {
			warnings = append(warnings, f.Path+": "+err.Error())
			continue
		}
		if a.Kind == "streams" {
			err = snapshot.readRecords(payload, *m)
		} else {
			err = snapshot.readJSONLines(payload, f)
		}
		filesRead = append(filesRead, f.Path)
		if err != nil {
			warnings = append(warnings, f.Path+": "+err.Error())
		}
	}
	if a.Kind == "ros2" {
		snapshot.readSensors(ctx, t.conn.DataService, a.Episode, selected)
	}
	warnings = append(warnings, snapshot.Warnings...)
	if len(snapshot.Samples) >= visualSampleLimit {
		warnings = append(warnings, "Decoder sample limit reached; summaries cover only decoded samples.")
	}
	rows := []chatSample{}
	stats := map[string]*chatNumericSummary{}
	for _, s := range snapshot.Samples {
		seconds := float64(s.Time) / 1e9
		if (a.Track != "" && s.Track != a.Track) || (a.Start != nil && seconds < *a.Start) || (a.End != nil && seconds >= *a.End) {
			continue
		}
		detail := s.Detail
		truncated := len(detail) > 2048
		if truncated {
			detail = detail[:2048]
		}
		row := chatSample{Track: s.Track, EpisodeSeconds: seconds, Label: s.Label, Detail: detail, DetailTruncated: truncated, Unit: s.Unit}
		if s.HasValue && visualFinite(s.Value) {
			value := s.Value
			row.Value = &value
			summary := stats[s.Track]
			if summary == nil {
				summary = &chatNumericSummary{Min: value, Max: value, Unit: s.Unit}
				stats[s.Track] = summary
			}
			summary.Count++
			summary.Min = min(summary.Min, value)
			summary.Max = max(summary.Max, value)
			n := float64(summary.Count)
			summary.Mean = summary.Mean*((n-1)/n) + value/n
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].EpisodeSeconds < rows[j].EpisodeSeconds })
	matched := len(rows)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	if len(snapshot.Samples) == 0 {
		warnings = append(warnings, "No decodable samples for this kind; it may not have been captured, may be unavailable, or may use an unsupported encoding.")
	}
	if m.State != "complete" && m.State != "interrupted" {
		warnings = append(warnings, "Episode is not sealed; data may still be arriving.")
	}
	tracks := snapshot.Tracks
	if a.Track != "" {
		tracks = nil
		for _, track := range snapshot.Tracks {
			if track.ID == a.Track {
				tracks = append(tracks, track)
			}
		}
	}
	trackCount := len(tracks)
	if len(tracks) > 100 {
		tracks = tracks[:100]
	}
	summaryCount := len(stats)
	if len(stats) > 100 {
		keys := make([]string, 0, len(stats))
		for key := range stats {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys[100:] {
			delete(stats, key)
		}
	}
	return map[string]any{"episode": m.ID, "boot_id": m.BootID, "canonical_clock": m.CanonicalClock, "time_basis": "Signed episode-relative seconds. Do not align other boots or UTC without clock evidence from the manifest.", "tracks": tracks, "tracks_truncated": trackCount > len(tracks), "numeric_summaries_truncated": summaryCount > len(stats), "samples": rows, "numeric_summaries": stats, "matching_decoded_samples": matched, "truncated": matched > limit, "verified_files": filesRead, "warnings": warnings, "coverage": fmt.Sprintf("%s only; at most 8 MiB per file, 16 MiB total, 20,000 decoded samples. Summaries do not establish causes or unrecorded history. ROS payload decoding may be unsupported; no image, audio, or point-cloud perception.", a.Kind)}, nil
}
