package data

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	recordingpb "github.com/wendylabsinc/wendy/go/proto/gen/recordingpb"
	"google.golang.org/protobuf/proto"
)

func continuousTestStream() appconfig.RecordingStream {
	return appconfig.RecordingStream{Mode: "durable", MediaType: "application/octet-stream", Storage: &appconfig.RecordingStorage{MaxBytes: 1 << 20, RetentionSeconds: proto.Int64(0)}}
}
func checkpointStream(t *testing.T, m *Manager) (string, []*recordingpb.StoredRecord) {
	t.Helper()
	var records []*recordingpb.StoredRecord
	token, err := m.ExportRecordingCheckpoint("test.app", "", "vibration", true, func(r *recordingpb.StoredRecord) error { records = append(records, r); return nil })
	if err != nil {
		t.Fatal(err)
	}
	return token, records
}
func appendVibration(t *testing.T, m *Manager, id string, size int) {
	t.Helper()
	if _, err := m.RecordStream("test.app", "", "vibration", continuousTestStream(), &recordingpb.Record{Id: id, Payload: make([]byte, size)}); err != nil {
		t.Fatal(err)
	}
}
func ackVibration(t *testing.T, m *Manager, token string) {
	t.Helper()
	if err := m.AcknowledgeRecordingExport("test.app", "", "vibration", token); err != nil {
		t.Fatal(err)
	}
}

func TestRecordingContinuousCaptureReusesQuota(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	// 8 MiB through a 1 MiB spool, with 100ms / 25.6kHz / three-axis float32
	// payloads. Check sequence continuity, metadata and accounting after restart.
	for cycle := 0; cycle < 16; cycle++ {
		for batch := 0; batch < 16; batch++ {
			appendVibration(t, m, fmt.Sprintf("%d", total+batch), 2560*3*4)
		}
		token, records := checkpointStream(t, m)
		if len(records) != 16 {
			t.Fatalf("cycle %d: got %d records", cycle, len(records))
		}
		for i, r := range records {
			if r.Record.Id != fmt.Sprintf("%d", total+i) || len(r.Record.Payload) != 2560*3*4 || r.StreamDescriptor.MediaType != "application/octet-stream" {
				t.Fatalf("lost or reordered batch: %v", r.Record.Id)
			}
		}
		total += len(records)
		ackVibration(t, m, token)
		j, _ := m.recordings.journal("test.app", "", "vibration")
		if j.size != 0 || len(j.ids) != 0 || len(j.segments) != 0 {
			t.Fatal("spool was not reclaimed")
		}
		if cycle%4 == 0 {
			m, err = NewManager(root)
			if err != nil {
				t.Fatal(err)
			}
			// Retrying an old acknowledgement cannot reclaim future segments.
			appendVibration(t, m, "retry-probe", 128)
			ackVibration(t, m, token)
			probe, records := checkpointStream(t, m)
			if len(records) != 1 || records[0].Record.Id != "retry-probe" {
				t.Fatal("old token deleted a future segment")
			}
			ackVibration(t, m, probe)
		}
	}
}

func TestRecordingCheckpointIsolationAndInterruptedExport(t *testing.T) {
	m, _ := NewManager(t.TempDir())
	appendVibration(t, m, "before", 100)
	injected := errors.New("disconnected")
	token, err := m.ExportRecordingCheckpoint("test.app", "", "vibration", true, func(*recordingpb.StoredRecord) error { return injected })
	if !errors.Is(err, injected) || token != "" {
		t.Fatal("failed export exposed checkpoint", token, err)
	}
	token, records := checkpointStream(t, m)
	if len(records) != 1 {
		t.Fatal("failed export lost data")
	}
	appendVibration(t, m, "after", 100)
	// Tokens are scoped to app/service/stream and are tamper-evident.
	if _, err = m.RecordStream("test.app", "other", "vibration", continuousTestStream(), &recordingpb.Record{Id: "other", Payload: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	if err = m.AcknowledgeRecordingExport("test.app", "other", "vibration", token); !errors.Is(err, ErrInvalidRecordingCheckpoint) {
		t.Fatal("cross-stream token accepted", err)
	}
	if err = m.AcknowledgeRecordingExport("test.app", "", "vibration", token+"x"); !errors.Is(err, ErrInvalidRecordingCheckpoint) {
		t.Fatal("tampered token accepted", err)
	}
	ackVibration(t, m, token)
	_, records = checkpointStream(t, m)
	if len(records) != 1 || records[0].Record.Id != "after" {
		t.Fatal("checkpoint reclaimed new records")
	}
}

func TestRecordingReclamationCrashRecovery(t *testing.T) {
	m, _ := NewManager(t.TempDir())
	appendVibration(t, m, "exported", 100)
	token, _ := checkpointStream(t, m)
	j, _ := m.recordings.journal("test.app", "", "vibration")
	// Simulate a crash after the durable watermark but before unlink.
	sequence := j.segments[0].sequence
	j.state.AcknowledgedThrough = &sequence
	if err := j.saveState(); err != nil {
		t.Fatal(err)
	}
	oldPath := j.segments[0].path
	fresh, err := NewManager(m.root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("acknowledged segment resurrected", err)
	}
	appendVibration(t, fresh, "new", 100)
	ackVibration(t, fresh, token)
	_, records := checkpointStream(t, fresh)
	if len(records) != 1 || records[0].Record.Id != "new" {
		t.Fatal("acknowledgement replay lost new data")
	}
}

func TestRecordingCheckpointSealSurvivesRestart(t *testing.T) {
	m, _ := NewManager(t.TempDir())
	appendVibration(t, m, "exported", 100)
	token, _ := checkpointStream(t, m)
	fresh, err := NewManager(m.root)
	if err != nil {
		t.Fatal(err)
	}
	appendVibration(t, fresh, "after-restart", 100)
	ackVibration(t, fresh, token)
	_, records := checkpointStream(t, fresh)
	if len(records) != 1 || records[0].Record.Id != "after-restart" {
		t.Fatal("restart appended into an exported segment")
	}
}

func TestRecordingContinuousRetentionAndQuota(t *testing.T) {
	m, _ := NewManager(t.TempDir())
	for i := 0; i < 3; i++ {
		appendVibration(t, m, fmt.Sprint(i), 300<<10)
	}
	j, _ := m.recordings.journal("test.app", "", "vibration")
	j.now = func() time.Time { return time.Now().Add(365 * 24 * time.Hour) }
	if err := j.expire(); err != nil {
		t.Fatal(err)
	}
	if len(j.ids) != 3 {
		t.Fatal("retain-until-export expired records")
	}
	if _, err := m.RecordStream("test.app", "", "vibration", continuousTestStream(), &recordingpb.Record{Id: "full", Payload: make([]byte, 300<<10)}); err == nil {
		t.Fatal("quota ignored")
	}
	fresh, err := NewManager(m.root)
	if err != nil {
		t.Fatal(err)
	}
	recovered, _ := fresh.recordings.journal("test.app", "", "vibration")
	if recovered.state.MaxBytes != 1<<20 || recovered.state.RetentionSeconds != 0 {
		t.Fatal("storage policy lost on restart")
	}
	token, records := checkpointStream(t, fresh)
	if len(records) != 3 {
		t.Fatal("quota rejection discarded records")
	}
	ackVibration(t, fresh, token)
	appendVibration(t, fresh, "resumed", 300<<10)
}

func TestRecordingSlowSyncDoesNotBlockOtherStreams(t *testing.T) {
	m, _ := NewManager(t.TempDir())
	j, _ := m.recordings.journal("test.app", "", "slow")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	j.syncFile = func(f *os.File) error { close(entered); <-release; return f.Sync() }
	slowDone := make(chan error, 1)
	go func() {
		_, err := m.RecordStream("test.app", "", "slow", continuousTestStream(), &recordingpb.Record{Id: "slow", Payload: []byte{1}})
		slowDone <- err
	}()
	<-entered
	fastDone := make(chan error, 1)
	go func() {
		_, err := m.RecordStream("test.app", "", "fast", continuousTestStream(), &recordingpb.Record{Id: "fast", Payload: []byte{2}})
		fastDone <- err
	}()
	select {
	case err := <-fastDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slow journal blocked unrelated stream")
	}
	unblock()
	if err := <-slowDone; err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.streamPreRoll) != 2 || m.streamPreRoll[0].record.Record.Id != "slow" || m.streamPreRoll[1].record.Record.Id != "fast" {
		t.Fatal("out-of-order commits broke receipt ordering")
	}
}

func TestRecordingCheckpointStateFailureRetainsData(t *testing.T) {
	m, _ := NewManager(t.TempDir())
	appendVibration(t, m, "retained", 100)
	token, _ := checkpointStream(t, m)
	j, _ := m.recordings.journal("test.app", "", "vibration")
	// A directory at the temporary state path forces persistence to fail.
	if err := os.Mkdir(filepath.Join(j.dir, "state.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := j.acknowledge(token); err == nil {
		t.Fatal("acknowledged without persisting watermark")
	}
	if _, err := os.Stat(j.segments[0].path); err != nil {
		t.Fatal("failed acknowledgement removed data", err)
	}
	if _, err := m.RecordStream("test.app", "", "vibration", continuousTestStream(), &recordingpb.Record{Id: "next", Payload: []byte{1}}); err == nil {
		t.Fatal("state failure did not fail closed")
	}
	if err := os.Remove(filepath.Join(j.dir, "state.tmp")); err != nil {
		t.Fatal(err)
	}
	fresh, err := NewManager(m.root)
	if err != nil {
		t.Fatal(err)
	}
	_, records := checkpointStream(t, fresh)
	if len(records) != 1 {
		t.Fatal("record disappeared after failed acknowledgement")
	}
}

func BenchmarkRecordingVibration(b *testing.B) {
	for _, rate := range []int{25600, 51200, 100000} {
		b.Run(fmt.Sprint(rate), func(b *testing.B) {
			m, err := NewManager(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			cfg := continuousTestStream()
			payload := make([]byte, rate/10*3*4)
			b.SetBytes(int64(len(payload)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err = m.RecordStream("test.app", "", "vibration", cfg, &recordingpb.Record{Id: fmt.Sprint(i), Payload: payload}); err != nil {
					b.Fatal(err)
				}
				if (i+1)%8 == 0 {
					token, err := m.ExportRecordingCheckpoint("test.app", "", "vibration", true, func(r *recordingpb.StoredRecord) error { return WriteRecording(io.Discard, r) })
					if err != nil {
						b.Fatal(err)
					}
					if err = m.AcknowledgeRecordingExport("test.app", "", "vibration", token); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}

func TestRecordingCheckpointChunkBound(t *testing.T) {
	m, _ := NewManager(t.TempDir())
	cfg := continuousTestStream()
	cfg.Storage.MaxBytes = 32 << 20
	for i := 0; i < 24; i++ {
		if _, err := m.RecordStream("test.app", "", "vibration", cfg, &recordingpb.Record{Id: fmt.Sprint(i), Payload: make([]byte, 900<<10)}); err != nil {
			t.Fatal(err)
		}
	}
	token, records := checkpointStream(t, m)
	if len(records) == 0 || len(records) >= 24 {
		t.Fatalf("unbounded checkpoint chunk: %d", len(records))
	}
	ackVibration(t, m, token)
	_, rest := checkpointStream(t, m)
	if len(rest)+len(records) != 24 || rest[0].Record.Id != fmt.Sprint(len(records)) {
		t.Fatal("partial checkpoint lost following data")
	}
}
