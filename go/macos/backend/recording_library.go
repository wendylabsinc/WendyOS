package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// Each browse/preview/export gets its own cancellation handle and connection.
// It never reserves or replaces the app's selected device or its live streams.
type recordingReads struct {
	mu     sync.Mutex
	active bool
	next   uint64
	reads  map[uint64]recordingRead
}
type recordingRead struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func (r *recordingReads) setActive(active bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active = active
	if !active {
		for _, read := range r.reads {
			read.cancel()
		}
		clear(r.reads)
	}
}
func (b *Backend) BeginRecordingRead() uint64 {
	r := &b.recordings
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active {
		return 0
	}
	if r.reads == nil {
		r.reads = make(map[uint64]recordingRead)
	}
	r.next++
	ctx, cancel := context.WithCancel(context.Background())
	r.reads[r.next] = recordingRead{ctx, cancel}
	return r.next
}
func (b *Backend) EndRecordingRead(id uint64) {
	r := &b.recordings
	r.mu.Lock()
	defer r.mu.Unlock()
	if read, ok := r.reads[id]; ok {
		read.cancel()
		delete(r.reads, id)
	}
}
func (b *Backend) recordingConnection(id uint64, selector string, secure bool, limit time.Duration) (context.Context, *grpcclient.AgentConnection, func(), error) {
	b.recordings.mu.Lock()
	read, ok := b.recordings.reads[id]
	b.recordings.mu.Unlock()
	if !ok {
		return nil, nil, nil, context.Canceled
	}
	ctx, cancel := context.WithTimeout(read.ctx, limit)
	// Separate the dial deadline from the lifetime of a successfully opened
	// transport. Some relays retain the context supplied to their connector.
	dialTimer := time.AfterFunc(12*time.Second, cancel)
	conn, err := commands.DesktopRecordingConnect(ctx, selector, secure)
	dialTimer.Stop()
	if err != nil || ctx.Err() != nil {
		if conn != nil {
			conn.Close()
		}
		if err == nil {
			err = ctx.Err()
		}
		cancel()
		return nil, nil, nil, err
	}
	return ctx, conn, func() { cancel(); conn.Close() }, nil
}

type RecordingEpisode struct {
	ID, Title, Trigger, Kind, UploadState string
	Topics                                []string
	PreviewCounts                         []uint64
	PreviewFile                           *data.File
	Started, Bytes                        int64
	Duration                              float64 // -1 when unavailable
	DetailsLoaded                         bool
}
type RecordingLibrary struct {
	Episodes       []RecordingEpisode
	Warning, Model string
}

func (b *Backend) RecordingLibrary(id uint64, selector string, secure bool) (RecordingLibrary, error) {
	ctx, conn, done, err := b.recordingConnection(id, selector, secure, time.Minute)
	if err != nil {
		return RecordingLibrary{}, err
	}
	defer done()
	lib, err := readRecordingLibrary(ctx, conn.DataService, 2*time.Second)
	if err != nil {
		return RecordingLibrary{}, err
	}
	if len(lib.Episodes) != 0 && ctx.Err() == nil {
		// Older Agents may have no DeviceInfo service. Model is optional.
		infoCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		info, _ := pb.NewWendyDeviceInfoServiceClient(conn.Conn).GetDeviceInfo(infoCtx, &pb.GetDeviceInfoRequest{})
		lib.Model = info.GetBoardModel()
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return RecordingLibrary{}, ctx.Err()
	}
	return lib, nil
}

func (b *Backend) PlayRecording(id uint64, selector string, secure bool, episode string) (*VisualSnapshot, error) {
	ctx, conn, done, err := b.recordingConnection(id, selector, secure, 5*time.Minute)
	if err != nil {
		return nil, err
	}
	defer done()
	return readVisualEpisode(ctx, conn.DataService, episode)
}
func (b *Backend) ExportRecordings(id uint64, selector string, secure bool, episodes []string) ([]string, error) {
	if len(episodes) == 0 {
		return nil, errors.New("choose recordings to export")
	}
	ctx, conn, done, err := b.recordingConnection(id, selector, secure, 30*time.Minute)
	if err != nil {
		return nil, err
	}
	defer done()
	return exportRecordings(ctx, conn.DataService, episodes)
}
func exportRecordings(ctx context.Context, client pb.DataServiceClient, episodes []string) ([]string, error) {
	var paths []string
	for _, episode := range episodes {
		filename, err := exportVisualEpisode(ctx, client, episode)
		if err != nil {
			for _, file := range paths {
				_ = os.Remove(file)
			}
			return nil, fmt.Errorf("export %s: %w", episode, err)
		}
		paths = append(paths, filename)
	}
	return paths, nil
}

func readRecordingLibrary(ctx context.Context, client pb.DataServiceClient, manifestBudget time.Duration) (RecordingLibrary, error) {
	items, err := inspectSavedEpisodes(ctx, client, manifestBudget, summarizeRecording)
	if err != nil {
		return RecordingLibrary{}, err
	}
	return recordingRows(items), nil
}

const RecordingDetailBatchLimit = 64

// Each continuation reads only the missing IDs selected from the index. On an
// older Agent this is a bounded set of Inspect RPCs, with no second Episodes RPC.
func (b *Backend) RecordingDetails(id uint64, selector string, secure bool, ids []string) (RecordingLibrary, error) {
	if err := validateRecordingBatch(ids); err != nil {
		return RecordingLibrary{}, err
	}
	ctx, conn, done, err := b.recordingConnection(id, selector, secure, time.Minute)
	if err != nil {
		return RecordingLibrary{}, err
	}
	defer done()
	return readRecordingDetails(ctx, conn.DataService, ids)
}
func validateRecordingBatch(ids []string) error {
	if len(ids) == 0 || len(ids) > RecordingDetailBatchLimit {
		return fmt.Errorf("choose between 1 and %d recordings", RecordingDetailBatchLimit)
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			return errors.New("recording IDs must be nonempty and unique")
		}
		seen[id] = true
	}
	return nil
}
func readRecordingDetails(ctx context.Context, client pb.DataServiceClient, ids []string) (RecordingLibrary, error) {
	episodes := make([]*pb.DataEpisode, len(ids))
	for i, id := range ids {
		episodes[i] = &pb.DataEpisode{Id: id}
	}
	items, err := inspectEpisodeBatch(ctx, client, episodes, 20*time.Second, summarizeRecording)
	if err != nil {
		return RecordingLibrary{}, err
	}
	return recordingRows(items), nil
}
func summarizeRecording(e *pb.DataEpisode, m *data.Manifest) RecordingEpisode {
	row := RecordingEpisode{ID: e.GetId(), Title: e.GetName(), Started: e.GetStartedUnixNanos(), Bytes: e.GetSizeBytes(), Duration: -1}
	if m != nil {
		row.DetailsLoaded = true
		row.Trigger, row.Duration, row.UploadState = m.Trigger.Reason, savedDuration(m), m.Upload.State
		row.Kind, row.Topics = recordingKind(m)
		row.PreviewCounts, row.PreviewFile = recordingPreview(m, row.Kind)
	}
	return row
}
func recordingRows(items []savedEpisode[RecordingEpisode]) RecordingLibrary {
	lib := RecordingLibrary{Episodes: make([]RecordingEpisode, len(items))}
	failed := 0
	for i, item := range items {
		lib.Episodes[i] = item.value
		if item.failed {
			failed++
		}
	}
	if failed != 0 {
		lib.Warning = fmt.Sprintf("Details unavailable for %d recordings.", failed)
	}
	return lib
}

// Read only small manifests and preserve every finalized episode from the list
// even if one of its manifests disappeared or cannot be decoded.
type savedEpisode[T any] struct {
	value   T
	missing bool
	failed  bool // attempted but unreadable, as distinct from deferred by the metadata budget
}

func inspectSavedEpisodes[T any](ctx context.Context, client pb.DataServiceClient, manifestBudget time.Duration, summarize func(*pb.DataEpisode, *data.Manifest) T) ([]savedEpisode[T], error) {
	list, err := client.Episodes(ctx, &pb.DataEpisodesRequest{})
	if err != nil {
		return nil, err
	}
	// Episodes contains finalized entries only. Agent reports captures that
	// are still recording, draining or sealing in its separate InProgress list.
	episodes := append([]*pb.DataEpisode(nil), list.GetEpisodes()...)
	sort.Slice(episodes, func(i, j int) bool {
		a, b := episodes[i], episodes[j]
		if a.GetStartedUnixNanos() == b.GetStartedUnixNanos() {
			return a.GetId() < b.GetId()
		}
		return a.GetStartedUnixNanos() > b.GetStartedUnixNanos()
	})
	return inspectEpisodeBatch(ctx, client, episodes, manifestBudget, summarize)
}
func inspectEpisodeBatch[T any](ctx context.Context, client pb.DataServiceClient, episodes []*pb.DataEpisode, manifestBudget time.Duration, summarize func(*pb.DataEpisode, *data.Manifest) T) ([]savedEpisode[T], error) {
	out := make([]savedEpisode[T], len(episodes))
	for i, e := range episodes {
		out[i] = savedEpisode[T]{value: summarize(e, nil), missing: true}
	}
	inspectCtx := ctx
	if manifestBudget > 0 {
		var cancel context.CancelFunc
		inspectCtx, cancel = context.WithTimeout(ctx, manifestBudget)
		defer cancel()
	}
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(4, len(episodes)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				e := episodes[i]
				reply, err := client.Inspect(inspectCtx, &pb.DataInspectRequest{Episode: e.GetId()})
				var m data.Manifest
				raw := reply.GetManifestJson()
				if err != nil || len(raw) > visualFileLimit || json.Unmarshal(raw, &m) != nil || m.ID != e.GetId() {
					out[i].failed = inspectCtx.Err() == nil
					continue
				}
				out[i] = savedEpisode[T]{value: summarize(e, &m)}
			}
		}()
	}
send:
	for i := range episodes {
		select {
		case jobs <- i:
		case <-inspectCtx.Done():
			break send
		}
	}
	close(jobs)
	workers.Wait()
	// A failed/timed-out list is an unavailable device. A successful list
	// remains authoritative if only metadata times out. Explicit cancellation
	// still discards the result, including account switches and backgrounding.
	if err := ctx.Err(); err != nil && (manifestBudget == 0 || !errors.Is(err, context.DeadlineExceeded)) {
		return nil, err
	}
	return out, nil
}

func savedDuration(m *data.Manifest) float64 {
	start, end := m.StartedEpisodeNS, m.StoppedEpisodeNS
	for _, s := range m.Sources {
		if s.Count > 0 {
			start = min(start, s.ActualOffset)
		}
	}
	if m.CaptureStoppedEpisodeNS != 0 {
		end = m.CaptureStoppedEpisodeNS
	}
	if (m.State == "complete" || m.State == "interrupted") && end >= start {
		return (float64(end) - float64(start)) / 1e9
	}
	return -1
}
func recordingKind(m *data.Manifest) (string, []string) {
	var camera, video, movie, audio, images, snapshots, ros bool
	var topics []string
	// Snapshot-mode campaigns save individual keyframes under cameras/, even
	// if they also produce a derived playable file for the viewer.
	for _, file := range m.Files {
		if file.Size <= 0 {
			continue
		}
		switch RecordingPreviewFormat(file) {
		case "image":
			images = true
		case "video":
			movie = movie || strings.EqualFold(path.Ext(file.Path), ".mp4")
		case "audio":
			audio = true
		}
		if !strings.HasPrefix(file.Path, "cameras/") {
			continue
		}
		switch name := path.Base(file.Path); {
		case strings.HasPrefix(name, "snapshot-"):
			snapshots = true
		case strings.HasPrefix(name, "segment-"):
			video = true
		}
	}
	for _, s := range m.Sources {
		if s.Count == 0 {
			continue
		}
		switch s.Source.Kind {
		case "ros2":
			ros = true
			if _, topic, ok := strings.Cut(s.Source.ID, ":/"); ok {
				topics = append(topics, "/"+topic)
			}
		case "camera":
			camera = true
		case "video":
			video = true
		case "audio":
			audio = true
		case "image":
			images = true
		}
	}
	switch {
	case video || ((camera || movie) && !snapshots):
		return "video", nil
	case images || snapshots:
		return "images", nil
	case ros:
		return "rosbag", topics
	case audio:
		return "audio", nil
	default:
		return "dataset", nil
	}
}
