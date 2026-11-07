package backend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/episodeexport"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// The catalog passes a single sealed file descriptor to the visible row. Never
// fetch a whole episode or scan other files while someone scrolls their library.
const thumbnailFileLimit = 4 << 20

func RecordingPreviewFormat(f data.File) string {
	switch strings.ToLower(path.Ext(f.Path)) {
	case ".jpeg", ".jpg", ".png":
		return "image"
	case ".h264":
		if strings.HasPrefix(f.Path, "cameras/") && strings.HasPrefix(path.Base(f.Path), "snapshot-") {
			return "video"
		}
	case ".mp4":
		return "video"
	case ".wav":
		return "audio"
	}
	return ""
}
func thumbnailEligible(f data.File) bool {
	digest, err := hex.DecodeString(f.SHA256)
	return f.Size > 0 && f.Size <= thumbnailFileLimit && len(digest) == sha256.Size && err == nil &&
		RecordingPreviewFormat(f) != "" && !path.IsAbs(f.Path) && path.Clean(f.Path) == f.Path && !strings.HasPrefix(f.Path, "../")
}

func recordingPreview(m *data.Manifest, kind string) ([]uint64, *data.File) {
	var counts []uint64
	switch kind {
	case "dataset":
		for _, s := range m.Sources[:min(12, len(m.Sources))] {
			counts = append(counts, s.Count)
		}
		return counts, nil
	case "rosbag":
		var domains []uint64
		for _, s := range m.Sources {
			if s.Count == 0 || s.Source.Kind != "ros2" {
				continue
			}
			if strings.Contains(s.Source.ID, ":/") {
				counts = append(counts, s.Count)
			} else {
				domains = append(domains, s.Count)
			}
		}
		if len(counts) == 0 {
			counts = domains
		} // older Agents only counted the whole domain
		sort.Slice(counts, func(i, j int) bool { return counts[i] > counts[j] })
		return counts[:min(3, len(counts))], nil
	}
	// Prefer a still over a movie; for audio use the first recorded microphone
	// in manifest order, not whichever directory sorts first (often loopback).
	sourceRank := map[string]int{}
	for i, s := range m.Sources {
		if s.Count > 0 {
			sourceRank[s.Source.ID] = i
		}
	}
	var best *data.File
	bestRank := math.MaxInt
	for i := range m.Files {
		f := &m.Files[i]
		format := RecordingPreviewFormat(*f)
		if !thumbnailEligible(*f) {
			continue
		}
		rank := 0
		switch kind {
		case "video", "images", "mixed":
			if format == "video" {
				rank = 2
				if strings.HasSuffix(strings.ToLower(f.Path), ".h264") {
					rank = 1
				}
			} else if format != "image" {
				continue
			}
		case "audio":
			if format != "audio" {
				continue
			}
			if n, ok := sourceRank[f.SourceID]; ok {
				rank = n
			} else {
				rank = len(m.Sources)
			}
		default:
			continue
		}
		if best == nil || rank < bestRank || (rank == bestRank && f.Path < best.Path) {
			best = f
			bestRank = rank
		}
	}
	return nil, best
}

func (b *Backend) RecordingThumbnail(id uint64, selector string, secure bool, episode string, file data.File) ([]byte, error) {
	if episode == "" || !thumbnailEligible(file) {
		return nil, errors.New("no small preview available")
	}
	ctx, conn, done, err := b.recordingConnection(id, selector, secure, 25*time.Second)
	if err != nil {
		return nil, err
	}
	defer done()
	return readRecordingThumbnail(ctx, conn.DataService, episode, file)
}

func readRecordingThumbnail(ctx context.Context, client pb.DataServiceClient, episode string, file data.File) ([]byte, error) {
	var buf bytes.Buffer
	if err := visualDownloadTo(ctx, client, episode, file, thumbnailFileLimit, &buf); err != nil {
		return nil, err
	}
	raw := buf.Bytes()
	if strings.ToLower(path.Ext(file.Path)) == ".h264" {
		return episodeexport.SnapshotMovie(raw)
	}
	if RecordingPreviewFormat(file) != "audio" {
		return raw, nil
	}
	peaks, err := readWAVPeaks(bytes.NewReader(raw), int64(len(raw)), nil)
	if err != nil {
		return nil, err
	}
	if len(peaks) == 0 {
		return nil, errors.New("empty audio segment")
	}
	// Nine chronological peak bins match the handoff's compact waveform.
	bars := make([]byte, min(9, len(peaks)))
	for i, p := range peaks {
		index := i * len(bars) / len(peaks)
		// Map -60...0 dBFS onto the available height. Digital silence stays flat.
		if p > 0 {
			height := max(0, min(1, (20*math.Log10(p)+60)/60))
			bars[index] = max(bars[index], byte(math.Round(255*height)))
		}
	}
	return bars, nil
}
