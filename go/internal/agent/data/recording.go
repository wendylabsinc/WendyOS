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
	// An empty app id is the agent's own namespace (see Publish).
	if app == "" {
		return false, errors.New("recording stream has no application id")
	}
	if err := validateStreamRecord(cfg, r); err != nil {
		return false, err
	}
	stored, err := stampStreamRecord(app, service, stream, streamDescriptor(cfg), r)
	if err != nil {
		return false, err
	}
	if cfg.Mode == "durable" {
		j, err := m.recordings.journal(app, service, stream)
		if err != nil {
			return false, err
		}
		duplicate, err := j.appendConfigured(stored, cfg.Storage)
		if err != nil || duplicate {
			return duplicate, err
		}
	}
	// Rejected and duplicate records returned above: no viewer or episode
	// sees them.
	observer := m.publish(stored)
	if observer != nil && (cfg.Event != "" || cfg.Model != "") {
		observer(app, streamApplicationRecord(stored))
	}
	return false, nil
}

// stampStreamRecord is the stored form of an accepted stream record: a clone
// of r, so the caller may reuse its buffers, with the stream's identity, its
// descriptor and the agent's receipt.
func stampStreamRecord(app, service, stream string, descriptor *recordingpb.StreamDescriptor, r *recordingpb.Record) (*recordingpb.StoredRecord, error) {
	receipt, err := readBootTime()
	if err != nil {
		return nil, err
	}
	boot := bootID()
	stored := &recordingpb.StoredRecord{Record: proto.Clone(r).(*recordingpb.Record), AppId: app, Service: service, Stream: stream, StreamDescriptor: descriptor, ReceiptBoottimeNanos: receipt, ReceiptBootId: boot, ReceiptUnixNanos: time.Now().UnixNano()}
	stored.ClientTimestampAccepted = r.ClientBoottimeNanos != nil && boot != "unavailable" && r.BootId == boot && abs64(*r.ClientBoottimeNanos-receipt) <= preRollWindow.Nanoseconds()
	return stored, nil
}

// publish puts an accepted record on its stream. It is the one path for app
// recording streams (RecordStream) and the agent's own producers (Publish).
//
// Live viewers get the record first, so neither storage nor a seal holding
// the manager lock delays what they see. It is offered with nobody subscribed
// too: the hub keeps it as the stream's newest record and counts it for the
// catalog. Then the recording rules apply: the stream pre-roll and every open
// episode that captures applications take it, as records.wdr. The viewer's
// payload is the one the pre-roll holds, so live and recorded are the same
// record; stored must not change afterwards.
//
// It returns the application observer, read under the same lock, so
// RecordStream need not take the lock again.
func (m *Manager) publish(stored *recordingpb.StoredRecord) func(string, ApplicationRecord) {
	// Journal commits use per-stream locks. Slow storage on one stream must
	// not hold the shared episode/pre-roll intake lock. Holding streamMu
	// through the offer keeps each stream's live order its recorded order,
	// and Offer never blocks.
	m.streamMu.Lock()
	defer m.streamMu.Unlock()
	m.live.Offer(liveRecord(stored))
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bufferStreamLocked(stored)
	for _, a := range m.openEpisodesLocked() {
		if !a.capturesApplications {
			continue
		}
		copy := proto.Clone(stored).(*recordingpb.StoredRecord)
		copy.EpisodeNanos = streamRecordStamp(stored) - a.manifest.RequestBootNanos
		if err := appendRecording(filepath.Join(a.dir, RecordingLogFile), copy); err != nil {
			m.warnf("stream %s could not be copied into episode %s: %v", streamID(stored), a.manifest.ID, err)
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
	return m.appObserver
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
		return errors.Join(err, f.Close())
	}
	if err = WriteRecording(f, r); err != nil {
		rollback := f.Truncate(before.Size())
		if rollback == nil {
			rollback = f.Sync()
		}
		return errors.Join(err, rollback, f.Close())
	}
	// Episode files are flushed at seal. Durable acceptance comes from the
	// independent journal sync, not this episode copy.
	return f.Close()
}
func (m *Manager) bufferStreamLocked(r *recordingpb.StoredRecord) {
	size := proto.Size(r) + 8
	// Independent journal commits may finish out of receipt order.
	i := sort.Search(len(m.streamPreRoll), func(i int) bool { return m.streamPreRoll[i].record.ReceiptBoottimeNanos > r.ReceiptBoottimeNanos })
	m.streamPreRoll = append(m.streamPreRoll, bufferedStreamRecord{})
	copy(m.streamPreRoll[i+1:], m.streamPreRoll[i:])
	m.streamPreRoll[i] = bufferedStreamRecord{r, size}
	m.streamPreRollSize += size
	now := m.streamPreRoll[len(m.streamPreRoll)-1].record.ReceiptBoottimeNanos
	m.evictPreRoll(now)
	cutoff := now - preRollWindow.Nanoseconds()
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
func (m *Manager) flushStreamPreRoll(dir string, origin int64, window time.Duration) (count uint64, earliest *int64, retErr error) {
	if window <= 0 || window > preRollWindow {
		window = preRollWindow
	}
	currentBoot := bootID()
	var file *os.File
	defer func() {
		if file != nil {
			retErr = errors.Join(retErr, file.Close())
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
func repairRecordingTail(path string) (retErr error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
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
		j.init.Do(func() {}) // Already recovered during startup.
		m.recordings.journals[entry.Name()] = j
		for _, segment := range j.segments {
			f, err := os.Open(segment.path)
			if err != nil {
				return err
			}
			reader := io.NewSectionReader(f, 0, segment.size)
			for {
				r, _, e := ReadRecording(reader)
				if e == io.EOF {
					break
				}
				if e != nil {
					f.Close()
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
