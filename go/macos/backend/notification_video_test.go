package backend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type notificationVideoFixture struct {
	*visualFixtureClient
	mu                       sync.Mutex
	indexCalls, inspectCalls int
	downloads                []string
}

func (c *notificationVideoFixture) Episodes(context.Context, *pb.DataEpisodesRequest, ...grpc.CallOption) (*pb.DataEpisodesResponse, error) {
	c.indexCalls++
	return &pb.DataEpisodesResponse{Episodes: []*pb.DataEpisode{{Id: "old", Name: "thermal", StartedUnixNanos: 1}, {Id: "new", Name: "thermal", StartedUnixNanos: 2}}, ReportsInProgress: true}, nil
}
func (c *notificationVideoFixture) Inspect(ctx context.Context, req *pb.DataInspectRequest, opts ...grpc.CallOption) (*pb.DataInspectResponse, error) {
	c.mu.Lock()
	c.inspectCalls++
	c.mu.Unlock()
	return c.visualFixtureClient.Inspect(ctx, req, opts...)
}
func (c *notificationVideoFixture) Download(ctx context.Context, req *pb.DataDownloadRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[pb.DataDownloadChunk], error) {
	c.mu.Lock()
	c.downloads = append(c.downloads, req.Path)
	c.mu.Unlock()
	return c.visualFixtureClient.Download(ctx, req, opts...)
}
func TestNotificationVideoReadsMatchingClipOnceAndVerifiesBytes(t *testing.T) {
	const id = "dd97fd1c-db21-476f-b26f-b399d1fe229c"
	for _, corrupt := range []bool{false, true} {
		files := map[string][]byte{"events.jsonl": []byte(`{"type":"event","attributes":{"notification_id":"` + id + `","max_celsius":27.5}}`), "cameras/c/playable.mp4": []byte("test-mp4-payload")}
		manifest := data.Manifest{ID: "new", Name: "thermal", State: "complete", StoppedEpisodeNS: 20e9}
		for _, path := range []string{"events.jsonl", "cameras/c/playable.mp4"} {
			raw := files[path]
			sum := sha256.Sum256(raw)
			manifest.Files = append(manifest.Files, data.File{Path: path, Size: int64(len(raw)), SHA256: hex.EncodeToString(sum[:]), Role: data.FileRoleDerived})
		}
		c := &notificationVideoFixture{visualFixtureClient: &visualFixtureClient{manifest: manifest, files: files}}
		if corrupt {
			c.mutate = func(chunk *pb.DataDownloadChunk) {
				if chunk.Path == "cameras/c/playable.mp4" {
					chunk.Data[0] ^= 1
				}
			}
		}
		got, err := readNotificationVideo(context.Background(), c, "thermal", id)
		if got.Preview.Path != "" {
			defer os.Remove(got.Preview.Path)
		}
		if corrupt {
			if err == nil {
				t.Fatal("accepted corrupted video")
			}
			continue
		}
		if err != nil || got.Episode == nil || got.Episode.GetId() != "new" {
			t.Fatalf("%+v %v", got, err)
		}
		if c.indexCalls != 1 || c.inspectCalls != 2 || !slices.Equal(c.downloads, []string{"events.jsonl", "cameras/c/playable.mp4"}) {
			t.Fatalf("redundant reads: %+v", c)
		}
		if raw, err := os.ReadFile(got.Preview.Path); err != nil || string(raw) != "test-mp4-payload" {
			t.Fatalf("%s %v", raw, err)
		}
	}
}
func TestNotificationVideoNeverUsesNearestUnrelatedClip(t *testing.T) {
	c := &notificationVideoFixture{visualFixtureClient: &visualFixtureClient{manifest: data.Manifest{ID: "new", Name: "thermal"}, files: map[string][]byte{}}}
	// A mismatch must not fall back to the most recent video's content.
	got, err := readNotificationVideo(context.Background(), c, "thermal", "dd97fd1c-db21-476f-b26f-b399d1fe229c")
	if got.Episode != nil || got.Preview.Path != "" {
		t.Fatal("opened unrelated clip")
	}
	// The fixture intentionally returns a wrong manifest for the older candidate.
	// The newer one was read and lacks the event, so the result is "not found".
	if err != nil {
		t.Fatalf("reported an unrelated recording's failure: %v", err)
	}
}

// Each recording has its own event log; only the exact event is a match.
type notificationLookupFixture struct {
	pb.DataServiceClient
	mu                                   sync.Mutex
	rows                                 []*pb.DataEpisode
	fixtures                             map[string]*visualFixtureClient
	inspected                            []string
	indexCalls, videoCalls, active, peak int
	gate                                 chan struct{}
}

func newNotificationLookupFixture() *notificationLookupFixture {
	c := &notificationLookupFixture{fixtures: make(map[string]*visualFixtureClient)}
	for i := 1; i <= 16; i++ {
		id := fmt.Sprintf("episode-%02d", i)
		event := "unrelated"
		if i == 2 {
			event = "dd97fd1c-db21-476f-b26f-b399d1fe229c"
		}
		files := map[string][]byte{"events.jsonl": []byte(`{"type":"event","attributes":{"notification_id":"` + event + `"}}`), "cameras/c/playable.mp4": []byte("verified-video")}
		m := data.Manifest{ID: id, Name: "thermal", State: "complete", StartedUnixNanos: int64(i) * 10e9}
		for name, raw := range files {
			sum := sha256.Sum256(raw)
			m.Files = append(m.Files, data.File{Path: name, Size: int64(len(raw)), SHA256: hex.EncodeToString(sum[:]), Role: data.FileRoleDerived})
		}
		c.rows = append(c.rows, &pb.DataEpisode{Id: id, Name: "thermal", State: "complete", StartedUnixNanos: m.StartedUnixNanos})
		c.fixtures[id] = &visualFixtureClient{manifest: m, files: files}
	}
	return c
}
func (c *notificationLookupFixture) Episodes(context.Context, *pb.DataEpisodesRequest, ...grpc.CallOption) (*pb.DataEpisodesResponse, error) {
	c.indexCalls++
	return &pb.DataEpisodesResponse{Episodes: c.rows, ReportsInProgress: true}, nil
}
func (c *notificationLookupFixture) Inspect(ctx context.Context, req *pb.DataInspectRequest, opts ...grpc.CallOption) (*pb.DataInspectResponse, error) {
	c.mu.Lock()
	c.inspected = append(c.inspected, req.Episode)
	c.active++
	c.peak = max(c.peak, c.active)
	if c.gate != nil && c.active == 4 {
		close(c.gate)
	}
	gate := c.gate
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.active--; c.mu.Unlock() }()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := c.fixtures[req.Episode]
	if f == nil {
		return nil, errors.New("missing recording")
	}
	return f.Inspect(ctx, req, opts...)
}
func (c *notificationLookupFixture) Download(ctx context.Context, req *pb.DataDownloadRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[pb.DataDownloadChunk], error) {
	if req.Path == "cameras/c/playable.mp4" {
		c.mu.Lock()
		c.videoCalls++
		c.mu.Unlock()
	}
	return c.fixtures[req.Episode].Download(ctx, req, opts...)
}
func TestNotificationVideoPrioritizesTimeWithFourConcurrentChecks(t *testing.T) {
	c := newNotificationLookupFixture()
	c.gate = make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := readNotificationVideoNear(ctx, c, "thermal", "dd97fd1c-db21-476f-b26f-b399d1fe229c", 21, "")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(got.Preview.Path)
	if got.Episode.GetId() != "episode-02" || len(c.inspected) != 4 || c.peak != 4 || c.videoCalls != 1 {
		t.Fatalf("wrong lookup: %+v", c)
	}
}
func TestNotificationVideoRevalidatesHintsAndFallsBackAcrossClockSkew(t *testing.T) {
	for _, hint := range []string{"episode-02", "episode-01", "deleted"} {
		t.Run(hint, func(t *testing.T) {
			c := newNotificationLookupFixture()
			got, err := readNotificationVideoNear(context.Background(), c, "thermal", "dd97fd1c-db21-476f-b26f-b399d1fe229c", 160, hint)
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(got.Preview.Path)
			if got.Episode.GetId() != "episode-02" || c.videoCalls != 1 {
				t.Fatalf("opened wrong clip: %+v", got)
			}
			if hint == "episode-02" && (c.indexCalls != 0 || len(c.inspected) != 1 || got.Episode.StartedUnixNanos != 20e9) {
				t.Fatal("hint did not bypass list or lost metadata")
			}
			if hint != "episode-02" && c.indexCalls != 1 {
				t.Fatal("stale hint did not fall back")
			}
		})
	}
}

func TestNotificationVideoReportsFailuresOnlyWhenNoRecordingCouldBeRead(t *testing.T) {
	const absent = "6f1c3f0e-8a52-4d7b-9a4e-2d6f3b1c9e07"
	c := newNotificationLookupFixture()
	delete(c.fixtures, "episode-05")
	got, err := readNotificationVideoNear(context.Background(), c, "thermal", absent, 0, "")
	if err != nil || got.Episode != nil {
		t.Fatalf("reported an unrelated recording's failure: %+v %v", got, err)
	}
	c = newNotificationLookupFixture()
	c.fixtures = map[string]*visualFixtureClient{}
	if _, err := readNotificationVideoNear(context.Background(), c, "thermal", absent, 0, ""); err == nil {
		t.Fatal("hid that no recording could be read")
	}
}

// notificationRecordingFixture serves one verified recording, "new", of campaign "thermal".
func notificationRecordingFixture(files map[string][]byte) *notificationVideoFixture {
	manifest := data.Manifest{ID: "new", Name: "thermal", State: "complete", StoppedEpisodeNS: 20e9}
	for path, raw := range files {
		sum := sha256.Sum256(raw)
		manifest.Files = append(manifest.Files, data.File{Path: path, Size: int64(len(raw)), SHA256: hex.EncodeToString(sum[:]), Role: data.FileRoleDerived})
	}
	return &notificationVideoFixture{visualFixtureClient: &visualFixtureClient{manifest: manifest, files: files}}
}

func TestNotificationVideoFindsEventBesideAnyAttributesAndManyAlerts(t *testing.T) {
	const id = "dd97fd1c-db21-476f-b26f-b399d1fe229c"
	var many strings.Builder
	for i := range 300 {
		fmt.Fprintf(&many, `{"type":"event","attributes":{"notification_id":"%08d-0000-4000-8000-000000000000"}}`+"\n", i)
	}
	for name, events := range map[string]string{
		"after 300 alerts": many.String() + `{"type":"event","attributes":{"notification_id":"` + id + `"}}`,
		"string labels":    `{"type":"event","attributes":{"notification_id":"` + id + `","labels":"person"}}`,
		"numeric label":    `{"type":"event","attributes":{"notification_id":"` + id + `","label":3}}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := notificationRecordingFixture(map[string][]byte{"events.jsonl": []byte(events), "cameras/c/playable.mp4": []byte("test-mp4-payload")})
			got, err := readNotificationVideoNear(context.Background(), c, "thermal", id, 0, "new")
			if got.Preview.Path != "" {
				defer os.Remove(got.Preview.Path)
			}
			if err != nil || got.Episode.GetId() != "new" {
				t.Fatalf("missed the recorded event: %+v %v", got, err)
			}
		})
	}
}
