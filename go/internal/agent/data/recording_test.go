package data

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	recordingpb "github.com/wendylabsinc/wendy/go/proto/gen/recordingpb"
	"google.golang.org/protobuf/proto"
)

var durableTestStream = appconfig.RecordingStream{Mode: "durable", MediaType: "image/png", Model: "detector"}

func testRecord(id string) *recordingpb.Record {
	return &recordingpb.Record{Id: id, Payload: []byte{0, 255, '\n', 128}, Uncertainty: proto.Float64(0.9), Inputs: []*recordingpb.SampleRef{{SourceId: "camera", SampleId: 1}}}
}
func collectStream(t *testing.T, m *Manager) []*recordingpb.StoredRecord {
	t.Helper()
	var out []*recordingpb.StoredRecord
	if err := m.ExportRecording("test.app", "", "frames", func(r *recordingpb.StoredRecord) error { out = append(out, r); return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestRecordingDurableRestartDedupAndEpisode(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	r := testRecord("stable-id")
	if duplicate, err := m.RecordStream("test.app", "", "frames", durableTestStream, r); err != nil || duplicate {
		t.Fatalf("first: %v %v", duplicate, err)
	}
	// No episode is required for durable acceptance or retrieval.
	if got := collectStream(t, m); len(got) != 1 || !bytes.Equal(got[0].Record.Payload, r.Payload) {
		t.Fatalf("record changed: %v", got)
	}
	m, err = NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := m.RecordStream("test.app", "", "frames", durableTestStream, r); err != nil || !duplicate {
		t.Fatalf("retry after restart: %v %v", duplicate, err)
	}
	changed := proto.Clone(r).(*recordingpb.Record)
	changed.Payload = []byte("different")
	if _, err = m.RecordStream("test.app", "", "frames", durableTestStream, changed); err == nil {
		t.Fatal("ID reused with new content")
	}
	if got := collectStream(t, m); len(got) != 1 {
		t.Fatalf("duplicate written: %d", len(got))
	}
	episode, err := m.Start(StartOptions{Sources: []string{"applications"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.RecordStream("test.app", "", "frames", durableTestStream, testRecord("live")); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Stop(AdHocEpisodeKey); err != nil {
		t.Fatal(err)
	}
	mf, failures, err := m.Inspect(episode.ID, true)
	if err != nil || len(failures) > 0 {
		t.Fatalf("inspect: %v %v", err, failures)
	}
	if mf.ModelIO.BinaryOutcomeLog != RecordingLogFile {
		t.Fatalf("binary outcomes not declared: %+v", mf.ModelIO)
	}
	f, err := os.Open(filepath.Join(root, episode.ID, RecordingLogFile))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	count := 0
	for {
		stored, _, err := ReadRecording(f)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		count++
		if stored.AppId != "test.app" || !bytes.Equal(stored.Record.Payload, r.Payload) {
			t.Fatal("lost identity or bytes")
		}
	}
	// On Linux restoration includes the original record in pre-roll. Other
	// platforms use the package's synthetic boot clock and may not align it.
	if count < 1 {
		t.Fatal("live record absent")
	}
}
func TestRecordingRecoveryTornTailAndCorruption(t *testing.T) {
	m, _ := NewManager(t.TempDir())
	m.RecordStream("test.app", "", "frames", durableTestStream, testRecord("one"))
	j, _ := m.recordings.journal("test.app", "", "frames")
	path := j.segments[0].path
	size := j.size
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.Write([]byte{0, 0, 0, 99, 0, 0, 0, 0, 1, 2})
	f.Close()
	m2, err := NewManager(m.root)
	if err != nil {
		t.Fatal(err)
	}
	if got := collectStream(t, m2); len(got) != 1 {
		t.Fatal("torn tail damaged preceding record")
	}
	info, _ := os.Stat(path)
	if info.Size() != size {
		t.Fatalf("tail not truncated: %d != %d", info.Size(), size)
	}
	f, _ = os.OpenFile(path, os.O_RDWR, 0)
	f.WriteAt([]byte{77}, size-1)
	f.Close()
	// Corruption fails the affected stream closed, without preventing other
	// streams or the agent itself from starting.
	m3, err := NewManager(m.root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m3.RecordStream("test.app", "", "frames", durableTestStream, testRecord("two")); err == nil {
		t.Fatal("accepted after corrupt frame")
	}
}
func TestRecordingSyncFailureCannotAcknowledge(t *testing.T) {
	m, _ := NewManager(t.TempDir())
	j, _ := m.recordings.journal("test.app", "", "frames")
	j.syncFile = func(*os.File) error { return errors.New("injected sync failure") }
	if _, err := m.RecordStream("test.app", "", "frames", durableTestStream, testRecord("one")); err == nil {
		t.Fatal("sync failure accepted")
	}
	if _, err := m.RecordStream("test.app", "", "frames", durableTestStream, testRecord("one")); err == nil {
		t.Fatal("unsynced duplicate accepted")
	}
	// A new manager must sync recovered bytes before declaring them committed.
	m2, err := NewManager(m.root)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := m2.RecordStream("test.app", "", "frames", durableTestStream, testRecord("one")); err != nil || !duplicate {
		t.Fatalf("recovery: %v %v", duplicate, err)
	}
}
func TestRecordingQuotaAndRetention(t *testing.T) {
	m, _ := NewManager(t.TempDir())
	m.RecordStream("test.app", "", "frames", durableTestStream, testRecord("one"))
	j, _ := m.recordings.journal("test.app", "", "frames")
	actual := j.size
	j.size = recordingQuotaBytes
	if _, err := m.RecordStream("test.app", "", "frames", durableTestStream, testRecord("two")); err == nil {
		t.Fatal("quota accepted")
	}
	if duplicate, err := m.RecordStream("test.app", "", "frames", durableTestStream, testRecord("one")); err != nil || !duplicate {
		t.Fatal("quota blocks committed retry")
	}
	j.size = actual
	j.now = func() time.Time { return time.Now().Add(recordingRetention + time.Hour) }
	if err := j.expire(); err != nil {
		t.Fatal(err)
	}
	if len(j.ids) != 0 || j.size != 0 || len(j.segments) != 0 {
		t.Fatal("expired journal retained IDs or bytes")
	}
}
func TestRecordingLightweightOpaqueAndNoJournal(t *testing.T) {
	m, _ := NewManager(t.TempDir())
	cfg := appconfig.RecordingStream{Mode: "lightweight", MediaType: "text/csv", TimeSeries: &appconfig.RecordingTimeSeries{Clock: "CLOCK_BOOTTIME", TimestampField: "time_ns", Channels: []appconfig.RecordingChannel{{Name: "temperature", Type: "float64", Unit: "Cel"}}}}
	payload := []byte("100,21.5\n200,21.6\n")
	if _, err := m.RecordStream("test.app", "", "temperature", cfg, &recordingpb.Record{Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if len(m.recordings.journals) != 0 {
		t.Fatal("raw stream created a durable journal")
	}
	got := m.streamPreRoll[0].record
	if !bytes.Equal(got.Record.Payload, payload) || got.StreamDescriptor.TimestampField != "time_ns" || got.Record.Timing != nil {
		t.Fatal("payload timing replaced by receipt timing")
	}
	if _, err := m.RecordStream("test.app", "", "temperature", cfg, &recordingpb.Record{Payload: make([]byte, MaxRecordingPacket)}); err != nil {
		t.Fatal("maximum raw packet rejected", err)
	}
}
func TestSampleTimingValidation(t *testing.T) {
	valid := &recordingpb.SampleTiming{Clock: "CLOCK_BOOTTIME", BootId: "boot", Count: 2, StartNanos: proto.Int64(10), PeriodNanos: proto.Int64(5)}
	if err := ValidateSampleTiming(valid); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*recordingpb.SampleTiming){
		func(r *recordingpb.SampleTiming) { r.Count = 0 },
		func(r *recordingpb.SampleTiming) { r.BootId = "" },
		func(r *recordingpb.SampleTiming) { r.PeriodNanos = proto.Int64(0) },
		func(r *recordingpb.SampleTiming) { r.StartNanos = proto.Int64(9223372036854775807) },
		func(r *recordingpb.SampleTiming) { r.TimestampsNanos = []int64{1, 2} },
	} {
		r := proto.Clone(valid).(*recordingpb.SampleTiming)
		mutate(r)
		if ValidateSampleTiming(r) == nil {
			t.Fatalf("invalid timing accepted: %v", r)
		}
	}
	irregular := &recordingpb.SampleTiming{Clock: "UNIX", Count: 3, TimestampsNanos: []int64{3, 5, 9}}
	if err := ValidateSampleTiming(irregular); err != nil {
		t.Fatal(err)
	}
}
func TestRecordingRecoveryRecountsBinaryPredictions(t *testing.T) {
	m, _ := NewManager(t.TempDir())
	episode, err := m.Start(StartOptions{Sources: []string{"applications"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.RecordStream("test.app", "", "frames", durableTestStream, testRecord("one")); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	a := m.active[AdHocEpisodeKey]
	a.cancel()
	m.mu.Unlock()
	<-a.done
	m2, err := NewManager(m.root)
	if err != nil {
		t.Fatal(err)
	}
	mf, _, err := m2.Inspect(episode.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if mf.ModelIO.Predictions != 1 || mf.ModelIO.PredictionsWithInputs != 1 {
		t.Fatalf("lost binary outcomes: %+v", mf.ModelIO)
	}
}

func TestRecordingTypedTimeSeries(t *testing.T) {
	cfg := appconfig.RecordingStream{Mode: "durable", MediaType: "application/protobuf", Schema: "wendy.agent.apps.v1.TimeSeriesBatch", TimeSeries: &appconfig.RecordingTimeSeries{Clock: "UNIX", Channels: []appconfig.RecordingChannel{{Name: "temperature", Type: "float64", Unit: "Cel"}}}}
	batch := &recordingpb.TimeSeriesBatch{Timing: &recordingpb.SampleTiming{Clock: "UNIX", Count: 2, TimestampsNanos: []int64{100, 200}}, Columns: []*recordingpb.SampleColumn{{Values: &recordingpb.SampleColumn_Float64Values{Float64Values: &recordingpb.Float64Values{Values: []float64{21.5, 21.6}}}}}}
	encoded, _ := proto.Marshal(batch)
	r := &recordingpb.Record{Id: "batch", Payload: encoded}
	if err := validateStreamRecord(cfg, r); err != nil {
		t.Fatal(err)
	}
	r.Timing = &recordingpb.SampleTiming{Clock: "UNIX", Count: 2, TimestampsNanos: []int64{101, 200}}
	if err := validateStreamRecord(cfg, r); err == nil {
		t.Fatal("conflicting timing accepted")
	}
	r.Timing = nil
	batch.Timing.Count = 3
	r.Payload, _ = proto.Marshal(batch)
	if err := validateStreamRecord(cfg, r); err == nil {
		t.Fatal("column/sample count mismatch accepted")
	}
	batch.Timing.Count = 2
	cfg.TimeSeries.Channels[0].Type = "int64"
	r.Payload, _ = proto.Marshal(batch)
	if err := validateStreamRecord(cfg, r); err == nil {
		t.Fatal("column type mismatch accepted")
	}
}

func TestRecordingSegmentOrderSurvivesWallClockStep(t *testing.T) {
	m, _ := NewManager(t.TempDir())
	j, _ := m.recordings.journal("test.app", "", "frames")
	now := time.Now()
	j.now = func() time.Time { return now }
	m.RecordStream("test.app", "", "frames", durableTestStream, testRecord("first"))
	j.segments[0].size = recordingSegmentBytes // force rotation without allocating a large fixture
	now = now.Add(-time.Hour)
	m.RecordStream("test.app", "", "frames", durableTestStream, testRecord("second"))
	fresh, err := NewManager(m.root)
	if err != nil {
		t.Fatal(err)
	}
	got := collectStream(t, fresh)
	if len(got) != 2 || got[0].Record.Id != "first" || got[1].Record.Id != "second" {
		t.Fatalf("wall clock reordered records: %v", got)
	}
}

func TestRecordingHeaderOnlyTailIsRecoverable(t *testing.T) {
	for _, tail := range [][]byte{{0}, {0, 0, 0, 99, 0, 0, 0}, {0, 0, 0, 99, 0, 0, 0, 0}} {
		t.Run(string(rune(len(tail)+'0')), func(t *testing.T) {
			m, _ := NewManager(t.TempDir())
			if _, err := m.RecordStream("test.app", "", "frames", durableTestStream, testRecord("one")); err != nil {
				t.Fatal(err)
			}
			j, _ := m.recordings.journal("test.app", "", "frames")
			path := j.segments[0].path
			f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			f.Write(tail)
			f.Close()
			fresh, err := NewManager(m.root)
			if err != nil {
				t.Fatal(err)
			}
			if len(collectStream(t, fresh)) != 1 {
				t.Fatal("header-only tail lost previous record")
			}
		})
	}
}
