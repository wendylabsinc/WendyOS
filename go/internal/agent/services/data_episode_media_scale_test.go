package services

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

// Exercise the real manifest listing and protobuf transport, without creating or
// reading any media files. Timings are observations, not machine-dependent gates.
func TestEpisodeMediaLargeRecordingLists(t *testing.T) {
	for _, tc := range []struct{ episodes, files int }{{2500, 6}, {10000, 12}} {
		t.Run(fmt.Sprintf("%d_episodes_%d_files", tc.episodes, tc.files), func(t *testing.T) {
			root := t.TempDir()
			manager, err := data.NewManager(root)
			if err != nil {
				t.Fatal(err)
			}
			files := []data.File{
				{Path: "telemetry.jsonl", MediaType: "application/x-ndjson", Size: 1024},
				{Path: "events.jsonl", MediaType: "application/x-ndjson", Size: 1024},
				{Path: "audio/microphone.wav", MediaType: "audio/wav", Size: 1024},
			}
			for i := len(files); i < tc.files; i++ {
				files = append(files, data.File{Path: fmt.Sprintf("cameras/by-id_usb-Camera_Camera_EA9667892-video-index%d/recording.mp4", i), MediaType: "video/mp4", Size: 1024})
			}
			for i := 0; i < tc.episodes; i++ {
				id := fmt.Sprintf("20261117T222547Z-%016x", i)
				dir := filepath.Join(root, id)
				if err := os.Mkdir(dir, 0750); err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(data.Manifest{ID: id, Name: "thermal-campaign", State: "complete", StartedUnixNanos: int64(i + 1), Files: files})
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			listener := bufconn.Listen(1 << 20)
			server := grpc.NewServer()
			pb.RegisterDataServiceServer(server, NewDataService(manager))
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(func() { server.Stop(); _ = listener.Close() })
			conn, err := grpc.NewClient("passthrough:///episode-scale", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			client := pb.NewDataServiceClient(conn)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			start := time.Now()
			// Match the bundled client's existing recordingEpisodeIndex receive budget.
			got, err := client.Episodes(ctx, &pb.DataEpisodesRequest{}, grpc.MaxCallRecvMsgSize(64<<20))
			if err != nil {
				t.Fatal(err)
			}
			elapsed := time.Since(start)
			if len(got.Episodes) != tc.episodes {
				t.Fatalf("lost episodes: %d", len(got.Episodes))
			}
			for i, row := range got.Episodes {
				if row.StartedUnixNanos != int64(tc.episodes-i) {
					t.Fatalf("lost chronological ordering at %d", i)
				}
				if row.Media == nil || len(row.Media.Files) != tc.files {
					t.Fatalf("lost media at %d", i)
				}
				for j, f := range row.Media.Files {
					if f.Path != files[j].Path || f.MediaType != files[j].MediaType {
						t.Fatalf("metadata changed at %d/%d", i, j)
					}
				}
			}
			raw, err := proto.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			var compressed bytes.Buffer
			zw := gzip.NewWriter(&compressed)
			if _, err = zw.Write(raw); err != nil {
				t.Fatal(err)
			}
			if err = zw.Close(); err != nil {
				t.Fatal(err)
			}
			baseline := proto.Clone(got).(*pb.DataEpisodesResponse)
			for _, row := range baseline.Episodes {
				row.Media = nil
			}
			baselineBytes := proto.Size(baseline)
			// Characterize the unchanged default gRPC limit as well as EMMA's budget.
			_, defaultErr := client.Episodes(ctx, &pb.DataEpisodesRequest{})
			if len(raw) > 4<<20 {
				if status.Code(defaultErr) != codes.ResourceExhausted {
					t.Fatalf("expected default receive limit, got %v", defaultErr)
				}
			} else if defaultErr != nil {
				t.Fatal(defaultErr)
			}
			t.Logf("episodes=%d files/episode=%d baseline=%d bytes with_media=%d bytes gzip=%d bytes list_and_RPC=%s default_4MiB=%v", tc.episodes, tc.files, baselineBytes, len(raw), compressed.Len(), elapsed, status.Code(defaultErr))
		})
	}
}
