package data

import (
	"container/heap"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	recordingpb "github.com/wendylabsinc/wendy/go/proto/gen/recordingpb"
	"google.golang.org/protobuf/proto"
)

type bufferedStreamRecord struct {
	record *recordingpb.StoredRecord
	size   int
}

const streamPreRollBytes = 16 << 20

func streamDescriptor(c appconfig.RecordingStream) *recordingpb.StreamDescriptor {
	d := &recordingpb.StreamDescriptor{MediaType: c.MediaType, Schema: c.Schema, Event: c.Event, Model: c.Model}
	if ts := c.TimeSeries; ts != nil {
		d.Clock = ts.Clock
		d.TimestampField = ts.TimestampField
		for _, c := range ts.Channels {
			d.Channels = append(d.Channels, &recordingpb.Channel{Name: c.Name, Type: c.Type, Unit: c.Unit})
		}
	}
	return d
}
func ValidateSampleTiming(t *recordingpb.SampleTiming) error {
	if t == nil {
		return nil
	}
	if t.Clock == "" || len(t.Clock) > 128 || len(t.BootId) > 128 || t.Count == 0 || t.Count > 65536 {
		return errors.New("sample timing needs a clock and 1..65536 samples")
	}
	if t.Clock == "CLOCK_BOOTTIME" && t.BootId == "" {
		return errors.New("CLOCK_BOOTTIME sample timing needs boot_id")
	}
	if len(t.TimestampsNanos) > 0 {
		if len(t.TimestampsNanos) != int(t.Count) || t.StartNanos != nil || t.PeriodNanos != nil {
			return errors.New("explicit timestamps must match count and exclude start/period")
		}
	} else {
		if t.StartNanos == nil || t.PeriodNanos == nil || *t.PeriodNanos <= 0 {
			return errors.New("uniform timing needs start_nanos and a positive period_nanos")
		}
		steps := int64(t.Count - 1)
		if steps > 0 && *t.PeriodNanos > math.MaxInt64/steps {
			return errors.New("sample timing overflows")
		}
		delta := steps * *t.PeriodNanos
		if *t.StartNanos > math.MaxInt64-delta {
			return errors.New("sample timing overflows")
		}
	}
	return nil
}
func validateStreamRecord(cfg appconfig.RecordingStream, r *recordingpb.Record) error {
	if r == nil || len(r.Payload) > MaxRecordingPacket || cfg.Mode == "durable" && proto.Size(r) > MaxRecordingPacket {
		return errors.New("record exceeds 1 MiB")
	}
	if cfg.Mode == "durable" && (r.Id == "" || len(r.Id) > 128) {
		return errors.New("durable record requires an ID of 1..128 bytes")
	}
	if len(r.BootId) > 128 || r.ClientBoottimeNanos != nil && *r.ClientBoottimeNanos < 0 {
		return errors.New("invalid client clock")
	}
	if len(r.Inputs) > 32 {
		return errors.New("too many input references")
	}
	for _, ref := range r.Inputs {
		if ref == nil || ref.SourceId == "" || len(ref.SourceId) > 256 {
			return errors.New("invalid input reference")
		}
	}
	if r.Uncertainty != nil && (math.IsNaN(*r.Uncertainty) || math.IsInf(*r.Uncertainty, 0) || *r.Uncertainty < 0 || *r.Uncertainty > 1) {
		return errors.New("uncertainty must be finite and in 0..1")
	}
	if len(r.Inputs) > 0 || r.Uncertainty != nil {
		if cfg.Model == "" {
			return errors.New("input references and uncertainty require a model stream")
		}
	}
	if err := ValidateSampleTiming(r.Timing); err != nil {
		return err
	}
	if ts := cfg.TimeSeries; ts != nil && r.Timing != nil && ts.Clock != r.Timing.Clock {
		return errors.New("sample clock differs from configured timeSeries clock")
	}
	if ts := cfg.TimeSeries; ts != nil && cfg.Mode == "durable" && r.Timing == nil && ts.TimestampField == "" && cfg.Schema != "wendy.agent.apps.v1.TimeSeriesBatch" {
		return errors.New("time-series record needs timing or a configured timestampField")
	}
	if cfg.Schema == "wendy.agent.apps.v1.TimeSeriesBatch" {
		return validateTypedSampleBatch(cfg, r)
	}
	return nil
}

// RecordStream returns a durable acknowledgement only for the independent
// journal commit. Copying to current episodes is a separate operation. A copy
// failure is reported to the operator; it cannot undo an acknowledged journal.
func (m *Manager) RecordStream(app, service, stream string, cfg appconfig.RecordingStream, r *recordingpb.Record) (bool, error) {
	if err := validateStreamRecord(cfg, r); err != nil {
		return false, err
	}
	// Serializing intake preserves receipt ordering in the bounded pre-roll ring.
	m.streamMu.Lock()
	defer m.streamMu.Unlock()
	receipt, err := readBootTime()
	if err != nil {
		return false, err
	}
	boot := bootID()
	stored := &recordingpb.StoredRecord{Record: proto.Clone(r).(*recordingpb.Record), AppId: app, Service: service, Stream: stream, StreamDescriptor: streamDescriptor(cfg), ReceiptBoottimeNanos: receipt, ReceiptBootId: boot, ReceiptUnixNanos: time.Now().UnixNano()}
	stored.ClientTimestampAccepted = r.ClientBoottimeNanos != nil && boot != "unavailable" && r.BootId == boot && abs64(*r.ClientBoottimeNanos-receipt) <= preRollWindow.Nanoseconds()
	if cfg.Mode == "durable" {
		j, err := m.recordings.journal(app, service, stream)
		if err != nil {
			return false, err
		}
		duplicate, err := j.append(stored)
		if err != nil || duplicate {
			return duplicate, err
		}
	}
	m.mu.Lock()
	m.bufferStreamLocked(stored)
	for _, a := range m.openEpisodesLocked() {
		if !a.capturesApplications {
			continue
		}
		copy := proto.Clone(stored).(*recordingpb.StoredRecord)
		copy.EpisodeNanos = streamRecordStamp(stored) - a.manifest.RequestBootNanos
		if err := appendRecording(filepath.Join(a.dir, RecordingLogFile), copy); err != nil {
			m.warnf("application stream %s could not be copied into episode %s: %v", stream, a.manifest.ID, err)
			continue
		}
		for i := range a.manifest.Sources {
			if a.manifest.Sources[i].Source.ID == "applications" {
				a.manifest.Sources[i].Count++
			}
		}
		a.manifest.ModelIO.BinaryOutcomeLog = RecordingLogFile
		a.noteApplicationRecord(streamApplicationRecord(stored))
	}
	observer := m.appObserver
	m.mu.Unlock()
	if observer != nil && (cfg.Event != "" || cfg.Model != "") {
		observer(app, streamApplicationRecord(stored))
	}
	return false, nil
}
func streamRecordStamp(r *recordingpb.StoredRecord) int64 {
	if r.ClientTimestampAccepted {
		return r.Record.GetClientBoottimeNanos()
	}
	return r.ReceiptBoottimeNanos
}
func streamApplicationRecord(s *recordingpb.StoredRecord) ApplicationRecord {
	r := ApplicationRecord{Version: 1, Name: s.StreamDescriptor.Event, Model: s.StreamDescriptor.Model, ClientBootID: s.Record.BootId, ClientBootNanos: s.Record.GetClientBoottimeNanos()}
	if r.Model != "" {
		r.Type = "prediction"
	} else if r.Name != "" {
		r.Type = "event"
	}
	if s.Record.Uncertainty != nil {
		r.Attributes = map[string]any{"uncertainty": *s.Record.Uncertainty}
	}
	for _, ref := range s.Record.Inputs {
		r.Inputs = append(r.Inputs, SampleRef{SourceID: ref.SourceId, SampleID: ref.SampleId})
	}
	return r
}
func appendRecording(path string, r *recordingpb.StoredRecord) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	before, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	if err = WriteRecording(f, r); err != nil {
		rollback := f.Truncate(before.Size())
		if rollback == nil {
			rollback = f.Sync()
		}
		f.Close()
		return errors.Join(err, rollback)
	}
	// Episode files are flushed at seal. Durable acceptance comes from the
	// independent journal sync, not this episode copy.
	return f.Close()
}
func (m *Manager) bufferStreamLocked(r *recordingpb.StoredRecord) {
	m.evictPreRoll(r.ReceiptBoottimeNanos)
	size := proto.Size(r) + 8
	m.streamPreRoll = append(m.streamPreRoll, bufferedStreamRecord{r, size})
	m.streamPreRollSize += size
	cutoff := r.ReceiptBoottimeNanos - preRollWindow.Nanoseconds()
	for len(m.streamPreRoll) > 0 && (m.streamPreRoll[0].record.ReceiptBoottimeNanos < cutoff || m.streamPreRollSize > streamPreRollBytes) {
		first := m.streamPreRoll[0]
		m.streamPreRollSize -= first.size
		if first.record.ReceiptBoottimeNanos >= cutoff {
			m.preRollEvicted = append(m.preRollEvicted, first.record.ReceiptBoottimeNanos)
		}
		m.streamPreRoll[0] = bufferedStreamRecord{}
		m.streamPreRoll = m.streamPreRoll[1:]
	}
}
func (m *Manager) flushStreamPreRoll(dir string, origin int64, window time.Duration) (uint64, *int64, error) {
	if window <= 0 || window > preRollWindow {
		window = preRollWindow
	}
	currentBoot := bootID()
	var count uint64
	var earliest *int64
	var file *os.File
	defer func() {
		if file != nil {
			file.Close()
		}
	}()
	for _, entry := range m.streamPreRoll {
		r := entry.record
		stamp := streamRecordStamp(r)
		if r.ReceiptBootId != currentBoot || r.ReceiptBoottimeNanos < origin-window.Nanoseconds() || r.ReceiptBoottimeNanos > origin || stamp < origin-window.Nanoseconds() || stamp > origin {
			continue
		}
		copy := proto.Clone(r).(*recordingpb.StoredRecord)
		copy.EpisodeNanos = stamp - origin
		copy.Preroll = true
		if file == nil {
			var err error
			file, err = os.OpenFile(filepath.Join(dir, RecordingLogFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
			if err != nil {
				return count, earliest, err
			}
		}
		if err := WriteRecording(file, copy); err != nil {
			return count, earliest, err
		}
		count++
		if earliest == nil || copy.EpisodeNanos < *earliest {
			v := copy.EpisodeNanos
			earliest = &v
		}
	}
	if file != nil {
		if err := file.Sync(); err != nil {
			return count, earliest, err
		}
	}
	return count, earliest, nil
}

// repairRecordingTail only discards a torn final frame. Complete frames with
// a bad checksum are corruption, not an excuse to discard acknowledged data.
func repairRecordingTail(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	var offset int64
	for {
		_, n, e := ReadRecording(f)
		if e == io.EOF && n == 0 {
			return nil
		}
		if e == io.ErrUnexpectedEOF {
			if e = f.Truncate(offset); e != nil {
				return e
			}
			return f.Sync()
		}
		if e != nil {
			return e
		}
		offset += n
	}
}
func reconcileStreamOutcomes(dir string, mf *Manifest) (bool, error) {
	f, err := os.Open(filepath.Join(dir, RecordingLogFile))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	mf.ModelIO.BinaryOutcomeLog = RecordingLogFile
	for {
		r, _, e := ReadRecording(f)
		if e == io.EOF {
			break
		}
		if e != nil {
			return false, fmt.Errorf("binary outcome log: %w", e)
		}
		if r.StreamDescriptor == nil {
			return false, errors.New("binary outcome descriptor missing")
		}
		a := activeEpisode{manifest: *mf}
		a.noteApplicationRecord(streamApplicationRecord(r))
		*mf = a.manifest
	}
	return true, nil
}

// Restore only recent records from this boot into pre-roll. Previous-boot
// records remain exportable but cannot be placed on the current boot timeline.
func (m *Manager) restoreStreamPreRoll() error {
	entries, err := os.ReadDir(m.recordings.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	now, err := readBootTime()
	if err != nil {
		return err
	}
	currentBoot := bootID()
	var recent streamRecordHeap
	retainedBytes := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		j := &recordingJournal{dir: filepath.Join(m.recordings.root, entry.Name()), ids: map[string]recordingID{}, now: time.Now, syncFile: func(f *os.File) error { return f.Sync() }}
		if err := j.recover(); err != nil {
			m.warnf("recording journal %s unavailable: %v", entry.Name(), err)
			continue
		}
		m.recordings.journals[entry.Name()] = j
		files, sizes, err := j.snapshot()
		if err != nil {
			return err
		}
		for i, f := range files {
			reader := io.NewSectionReader(f, 0, sizes[i])
			for {
				r, _, e := ReadRecording(reader)
				if e == io.EOF {
					break
				}
				if e != nil {
					for _, f := range files {
						f.Close()
					}
					return e
				}
				if r.ReceiptBootId == currentBoot && r.ReceiptBoottimeNanos >= now-preRollWindow.Nanoseconds() && r.ReceiptBoottimeNanos <= now {
					heap.Push(&recent, r)
					retainedBytes += proto.Size(r) + 8
					for retainedBytes > streamPreRollBytes {
						old := heap.Pop(&recent).(*recordingpb.StoredRecord)
						retainedBytes -= proto.Size(old) + 8
						m.preRollEvicted = append(m.preRollEvicted, old.ReceiptBoottimeNanos)
					}
				}
			}
			f.Close()
		}
	}
	// Journals are traversed by stream rather than receipt order. Re-sort the
	// bounded retained subset before using receipt-order eviction.
	sort.Slice(recent, func(i, j int) bool { return recent[i].ReceiptBoottimeNanos < recent[j].ReceiptBoottimeNanos })
	m.streamPreRoll = nil
	m.streamPreRollSize = 0
	sort.Slice(m.preRollEvicted, func(i, j int) bool { return m.preRollEvicted[i] < m.preRollEvicted[j] })
	for _, r := range recent {
		m.bufferStreamLocked(r)
	}
	return nil
}

type streamRecordHeap []*recordingpb.StoredRecord

func (h streamRecordHeap) Len() int { return len(h) }
func (h streamRecordHeap) Less(i, j int) bool {
	return h[i].ReceiptBoottimeNanos < h[j].ReceiptBoottimeNanos
}
func (h streamRecordHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *streamRecordHeap) Push(v any)   { *h = append(*h, v.(*recordingpb.StoredRecord)) }
func (h *streamRecordHeap) Pop() any {
	old := *h
	n := len(old)
	v := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return v
}
