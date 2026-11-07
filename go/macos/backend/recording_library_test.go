package backend

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
)

type recordingLibraryAgent struct {
	pb.UnimplementedDataServiceServer
	block   chan struct{}
	arrived chan struct{}
}

func (f *recordingLibraryAgent) Episodes(ctx context.Context, _ *pb.DataEpisodesRequest) (*pb.DataEpisodesResponse, error) {
	if f.block != nil {
		close(f.arrived)
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &pb.DataEpisodesResponse{Episodes: []*pb.DataEpisode{
		{Id: "one", Name: "Door", SizeBytes: 800, StartedUnixNanos: 3}, {Id: "gone", Name: "Manual clip", SizeBytes: 300, StartedUnixNanos: 1},
	}, InProgress: []*pb.DataEpisode{{Id: "still-recording", State: "recording"}}}, nil
}
func (f *recordingLibraryAgent) Inspect(_ context.Context, req *pb.DataInspectRequest) (*pb.DataInspectResponse, error) {
	if req.GetEpisode() == "gone" {
		return nil, errors.New("removed")
	}
	m := data.Manifest{ID: req.GetEpisode(), State: "complete", CanonicalClock: "CLOCK_BOOTTIME", StoppedEpisodeNS: 8e9, CaptureStoppedEpisodeNS: 5e9, Trigger: data.EpisodeTrigger{Reason: "event:door_opened"}, Upload: data.WorkflowState{State: "uploaded"}, Sources: []data.SourceStats{{Source: data.Source{Kind: "ros2", ID: "ros2:fastdds:domain-0:/camera"}, Count: 1, ActualOffset: -1e9}}}
	body, err := json.Marshal(m)
	return &pb.DataInspectResponse{ManifestJson: body}, err
}
func isolatedRecordingAgent(t *testing.T, f *recordingLibraryAgent) {
	t.Helper()
	// Unix socket paths must fit sockaddr_un even when the OS temp path is long.
	dir, err := os.MkdirTemp("/tmp", "wc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "data")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	pb.RegisterDataServiceServer(server, f)
	go server.Serve(listener)
	t.Cleanup(func() { server.Stop(); listener.Close() })
	t.Setenv("WENDY_AGENT_SOCKET", socket)
}
func TestRecordingLibraryUsesIndependentConnectionForListPlayAndExport(t *testing.T) {
	isolatedRecordingAgent(t, &recordingLibraryAgent{})
	b := New()
	primary := &grpcclient.AgentConnection{Host: "primary"}
	b.conn, b.deviceSelector, b.session = primary, "primary", 17
	b.life, b.stop = context.WithCancel(context.Background())
	defer b.stop()
	b.recordings.setActive(true)
	id := b.BeginRecordingRead()
	defer b.EndRecordingRead(id)
	const secondary = "secondary.local:50051"
	library, err := b.RecordingLibrary(id, secondary, false)
	if err != nil || len(library.Episodes) != 2 || library.Warning != "" {
		t.Fatalf("list: %+v, %v", library, err)
	}
	newest := library.Episodes[0]
	if newest.ID != "one" || newest.Duration != -1 || newest.DetailsLoaded {
		t.Fatalf("metadata: %+v", newest)
	}
	if row := library.Episodes[1]; row.ID != "gone" || row.Bytes != 300 || row.Kind != "" || row.Duration != -1 {
		t.Fatalf("listed recording lost: %+v", row)
	}
	details, err := b.RecordingDetails(id, secondary, false, []string{"gone", "one"})
	if err != nil || len(details.Episodes) != 2 || details.Episodes[0].DetailsLoaded || details.Episodes[1].Trigger != "event:door_opened" || !details.Episodes[1].DetailsLoaded {
		t.Fatalf("detail continuation: %+v, %v", details, err)
	}
	snap, err := b.PlayRecording(id, secondary, false, "one")
	if err != nil || snap.Clock != "CLOCK_BOOTTIME" {
		t.Fatalf("play: %+v %v", snap, err)
	}
	t.Setenv("TMPDIR", t.TempDir())
	paths, err := b.ExportRecordings(id, secondary, false, []string{"one", "two"})
	if err != nil || len(paths) != 2 {
		t.Fatalf("exports: %v, %v", paths, err)
	}
	for i, file := range paths {
		archive, err := zip.OpenReader(file)
		if err != nil {
			t.Fatal(err)
		}
		content, err := archive.Open("manifest.json")
		if err != nil {
			t.Fatal(err)
		}
		var manifest data.Manifest
		err = json.NewDecoder(content).Decode(&manifest)
		content.Close()
		archive.Close()
		os.Remove(file)
		if err != nil || manifest.ID != []string{"one", "two"}[i] {
			t.Fatalf("wrong exported recording: %s, %v", manifest.ID, err)
		}
	}
	if _, err := b.ExportRecordings(id, secondary, false, []string{"one", "gone"}); err == nil {
		t.Fatal("partial export succeeded")
	}
	if files, _ := filepath.Glob(filepath.Join(os.TempDir(), "wendy-episode-*.zip")); len(files) > 0 {
		t.Fatalf("leaked partial export: %v", files)
	}
	if b.conn != primary || b.session != 17 || b.deviceSelector != "primary" || b.life.Err() != nil {
		t.Fatal("recording read disturbed the primary connection")
	}
}
func TestRecordingLibraryCancelsInFlightReadsAndRejectsStaleRequests(t *testing.T) {
	f := &recordingLibraryAgent{block: make(chan struct{}), arrived: make(chan struct{})}
	isolatedRecordingAgent(t, f)
	b := New()
	b.recordings.setActive(true)
	id := b.BeginRecordingRead()
	done := make(chan error, 1)
	go func() { _, err := b.RecordingLibrary(id, "secondary.local:50051", false); done <- err }()
	select {
	case <-f.arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("read did not start")
	}
	b.recordings.setActive(false)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled recording read succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active read was not cancelled")
	}
	if b.BeginRecordingRead() != 0 {
		t.Fatal("background read opened")
	}
	b.recordings.setActive(true)
	if _, err := b.PlayRecording(id, "secondary.local:50051", false, "one"); !errors.Is(err, context.Canceled) {
		t.Fatalf("old request revived: %v", err)
	}
}

type progressiveManifestClient struct {
	*campaignClipClient
	lists     int
	fast      bool
	mu        sync.Mutex
	inspected []string
}

func (c *progressiveManifestClient) Episodes(ctx context.Context, req *pb.DataEpisodesRequest, opts ...grpc.CallOption) (*pb.DataEpisodesResponse, error) {
	c.lists++
	return c.campaignClipClient.Episodes(ctx, req, opts...)
}
func (c *progressiveManifestClient) Inspect(ctx context.Context, req *pb.DataInspectRequest, opts ...grpc.CallOption) (*pb.DataInspectResponse, error) {
	c.mu.Lock()
	c.inspected = append(c.inspected, req.Episode)
	c.mu.Unlock()
	if !c.fast && req.Episode != "latest" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return c.campaignClipClient.Inspect(ctx, req, opts...)
}
func TestRecordingLibraryContinuesMissingDetailsWithoutRelisting(t *testing.T) {
	ids := []string{"old-1", "old-2", "old-3", "old-4", "old-5", "latest"}
	c := &progressiveManifestClient{campaignClipClient: &campaignClipClient{manifests: make(map[string]data.Manifest)}}
	for i, id := range ids {
		c.episodes = append(c.episodes, &pb.DataEpisode{Id: id, StartedUnixNanos: int64(i + 1), Name: "episode-" + id, SizeBytes: 100})
		c.manifests[id] = data.Manifest{ID: id, Trigger: data.EpisodeTrigger{Reason: "manual"}}
	}
	lib, err := readRecordingLibrary(context.Background(), c)
	if err != nil || len(lib.Episodes) != len(ids) || lib.Warning != "" {
		t.Fatalf("index: %+v %v", lib, err)
	}
	if row := lib.Episodes[0]; row.ID != "latest" || row.DetailsLoaded || row.Trigger != "" {
		t.Fatalf("latest episode was not prioritized: %+v", row)
	}
	var missing []string
	for _, row := range lib.Episodes {
		if row.DetailsLoaded || row.Title != "episode-"+row.ID || row.Bytes != 100 {
			t.Fatalf("lost index row: %+v", row)
		}
		missing = append(missing, row.ID)
	}

	if len(c.inspected) != 0 {
		t.Fatalf("index blocked on %d manifest reads", len(c.inspected))
	}
	c.fast = true
	c.inspected = nil
	for offset := 0; offset < len(missing); offset += 2 {
		batch := missing[offset:min(offset+2, len(missing))]
		details, err := readRecordingDetails(context.Background(), c, batch)
		if err != nil || len(details.Episodes) != len(batch) {
			t.Fatalf("batch %v: %+v, %v", batch, details, err)
		}
		for i, row := range details.Episodes {
			if row.ID != batch[i] || !row.DetailsLoaded || row.Trigger != "manual" {
				t.Fatalf("wrong detail for %v: %+v", batch[i], row)
			}
		}
	}
	slices.Sort(c.inspected)
	slices.Sort(missing)
	if c.lists != 1 || !slices.Equal(c.inspected, missing) {
		t.Fatalf("continuation did extra work: %d lists, inspected %v", c.lists, c.inspected)
	}
}

func TestRecordingLibrarySnapshotCampaignIsImagesDespiteDerivedMovie(t *testing.T) {
	m := &data.Manifest{Sources: []data.SourceStats{{Source: data.Source{Kind: "camera"}}}, Files: []data.File{{Path: "cameras/front/snapshot-000001.h264", Size: 100}, {Path: "cameras/front/playable.mp4"}}}
	if kind, _ := recordingKind(m); kind != "images" {
		t.Fatalf("snapshot-only campaign appeared as %s", kind)
	}
	m.Files = append(m.Files, data.File{Path: "cameras/rear/segment-000001.h264", Size: 100})
	if kind, _ := recordingKind(m); kind != "video" {
		t.Fatalf("mixed cameras appeared as %s", kind)
	}
}
