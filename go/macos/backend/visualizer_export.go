package backend

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// The archive contains every manifest file, without viewer decimation or
// format conversion. Bag directories and metadata.yaml remain together.
func (b *Backend) ExportVisualEpisode(session uint64, episode string) (string, error) {
	b.mu.Lock()
	if session == 0 || episode == "" {
		b.mu.Unlock()
		return "", errors.New("choose an episode on the connected device")
	}
	conn, err := b.connection(session)
	if err != nil {
		b.mu.Unlock()
		return "", err
	}
	life := b.life
	if life == nil {
		life = context.Background()
	}
	ctx, cancel := context.WithTimeout(life, 30*time.Minute)
	client := conn.DataService
	b.mu.Unlock()
	defer cancel()
	return exportVisualEpisode(ctx, client, episode)
}
func exportVisualEpisode(ctx context.Context, client pb.DataServiceClient, episode string) (filename string, err error) {
	r, err := client.Inspect(ctx, &pb.DataInspectRequest{Episode: episode})
	if err != nil {
		return "", err
	}
	var m data.Manifest
	if len(r.GetManifestJson()) > visualFileLimit {
		return "", errors.New("manifest exceeds export metadata limit")
	}
	if err = json.Unmarshal(r.GetManifestJson(), &m); err != nil {
		return "", err
	}
	if m.ID != episode || (m.State != "complete" && m.State != "interrupted") {
		return "", errors.New("stop recording before exporting this episode")
	}
	seen := map[string]bool{"manifest.json": true, "OPEN_RECORDING.txt": true}
	for _, f := range m.Files {
		if f.Path == "." || f.Path == "" || path.Clean(f.Path) != f.Path || strings.HasPrefix(f.Path, "/") || strings.HasPrefix(f.Path, "../") || strings.ContainsAny(f.Path, "\\:\x00") || seen[f.Path] {
			return "", fmt.Errorf("invalid or duplicate export path %q", f.Path)
		}
		seen[f.Path] = true
	}
	f, err := os.CreateTemp("", "wendy-episode-*.zip")
	if err != nil {
		return "", err
	}
	defer func() {
		_ = f.Close()
		if err != nil {
			_ = os.Remove(f.Name())
		}
	}()
	z := zip.NewWriter(f)
	w, err := z.Create("manifest.json")
	if err != nil {
		return "", err
	}
	if _, err = w.Write(r.GetManifestJson()); err != nil {
		return "", err
	}
	w, err = z.Create("OPEN_RECORDING.txt")
	if err != nil {
		return "", err
	}
	_, err = w.Write([]byte("Wendy Data episode export\n\nAll files are original, verified against manifest.json SHA-256 checksums.\nExtract this ZIP before opening files.\n\nROS 2: open .mcap files in an MCAP-compatible viewer, or use ros2 bag info/play on a bag directory containing metadata.yaml. SQLite .db3 bags retain their metadata and original message definitions/type names.\nAudio: open .wav segments in an audio editor. audio/*/index.jsonl records segment offsets and clock uncertainty.\nVideo: open cameras/*/playable.mp4.\nTelemetry and events: JSONL, with episode-relative timestamps.\n\nROS bag receipt timestamps and sensor message header timestamps are distinct. ros2/*-clock_samples.jsonl preserves the measured mapping to the episode timeline. Original data is not downsampled by the viewer.\n"))
	if err != nil {
		return "", err
	}
	for _, item := range m.Files {
		w, err = z.CreateHeader(&zip.FileHeader{Name: item.Path, Method: zip.Store})
		if err != nil {
			return "", err
		}
		if err = visualDownloadTo(ctx, client, episode, item, item.Size, w); err != nil {
			return "", fmt.Errorf("%s: %w", item.Path, err)
		}
	}
	if err = z.Close(); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}
