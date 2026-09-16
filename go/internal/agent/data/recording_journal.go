package data

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	recordingpb "github.com/wendylabsinc/wendy/go/proto/gen/recordingpb"
	"google.golang.org/protobuf/proto"
)

const (
	MaxRecordingPacket    = 1 << 20
	maxStoredRecording    = MaxRecordingPacket + 64<<10
	recordingSegmentBytes = 4 << 20
	recordingQuotaBytes   = 64 << 20
	recordingRetention    = 24 * time.Hour
	RecordingLogFile      = "records.wdr"
)

// Journals have configurable byte and age limits per (app, service, stream).
// Only expiry or an authenticated export acknowledgement reclaims records.
// Record IDs deduplicate within the retained journal only.
type recordingStore struct {
	mu       sync.Mutex
	root     string
	journals map[string]*recordingJournal
}
type recordingJournal struct {
	init        sync.Once
	initErr     error
	state       recordingJournalState
	nextSegment uint64
	mu          sync.Mutex
	dir         string
	segments    []*recordingSegment
	ids         map[string]recordingID
	size        int64
	failure     error
	now         func() time.Time
	syncFile    func(*os.File) error
}
type recordingSegment struct {
	sequence uint64
	path     string
	size     int64
	last     time.Time
	created  time.Time
	ids      []string
}
type recordingID struct {
	hash [32]byte
}

func recordingKey(app, service, stream string) string {
	h := sha256.Sum256([]byte(app + "\x00" + service + "\x00" + stream))
	return hex.EncodeToString(h[:])
}
func (s *recordingStore) journal(app, service, stream string) (*recordingJournal, error) {
	s.mu.Lock()
	key := recordingKey(app, service, stream)
	j := s.journals[key]
	if j == nil {
		j = &recordingJournal{dir: filepath.Join(s.root, key), ids: map[string]recordingID{}, now: time.Now, syncFile: func(f *os.File) error { return f.Sync() }}
		s.journals[key] = j
	}
	s.mu.Unlock()
	// Recovery may read gigabytes. Other streams can initialize and commit
	// while this journal recovers.
	j.init.Do(func() { j.initErr = j.recover() })
	return j, j.initErr
}

func syncDirectory(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}

// On-disk frames are big-endian uint32 length + CRC32C + StoredRecord protobuf.
// This framing belongs to Wendy's storage/export format, never raw app packets.
func WriteRecording(w io.Writer, r *recordingpb.StoredRecord) error {
	if r == nil || r.Record == nil || r.StreamDescriptor == nil {
		return errors.New("missing recording payload or descriptor")
	}
	b, err := proto.Marshal(r)
	if err != nil {
		return err
	}
	if len(b) > maxStoredRecording {
		return errors.New("stored recording exceeds limit")
	}
	var h [8]byte
	binary.BigEndian.PutUint32(h[:4], uint32(len(b)))
	binary.BigEndian.PutUint32(h[4:], crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli)))
	n, err := w.Write(h[:])
	if err != nil {
		return err
	}
	if n != len(h) {
		return io.ErrShortWrite
	}
	n, err = w.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	return err
}
func ReadRecording(r io.Reader) (*recordingpb.StoredRecord, int64, error) {
	var h [8]byte
	n, e := io.ReadFull(r, h[:])
	if e != nil {
		return nil, int64(n), e
	}
	size := binary.BigEndian.Uint32(h[:4])
	if size == 0 || size > maxStoredRecording {
		return nil, 8, errors.New("invalid recording frame size")
	}
	b := make([]byte, size)
	n, e = io.ReadFull(r, b)
	if e == io.EOF {
		e = io.ErrUnexpectedEOF
	}
	if e != nil {
		return nil, int64(n + 8), e
	}
	if crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli)) != binary.BigEndian.Uint32(h[4:]) {
		return nil, int64(n + 8), errors.New("recording checksum mismatch")
	}
	out := new(recordingpb.StoredRecord)
	e = proto.Unmarshal(b, out)
	if e == nil && (out.Record == nil || out.StreamDescriptor == nil) {
		e = errors.New("missing recording payload or descriptor")
	}
	return out, int64(n + 8), e
}
func recordHash(r *recordingpb.Record) [32]byte {
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(r)
	return sha256.Sum256(b)
}
func (j *recordingJournal) recover() error {
	if err := os.MkdirAll(j.dir, 0o750); err != nil {
		return err
	}
	// Persist both the stream directory and the journal root on first use.
	if err := syncDirectory(filepath.Dir(j.dir)); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(filepath.Dir(j.dir))); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(filepath.Dir(filepath.Dir(j.dir)))); err != nil {
		return err
	}
	if err := j.loadState(); err != nil {
		return err
	}
	entries, err := os.ReadDir(j.dir)
	if err != nil {
		return err
	}
	currentBoot := bootID()
	bootNow, _ := readBootTime()
	observedNow := j.now()
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".wdr") {
			continue
		}
		sequence, err := strconv.ParseUint(strings.TrimSuffix(entry.Name(), ".wdr"), 10, 64)
		if err != nil || sequence == ^uint64(0) {
			return errors.New("invalid recording segment name")
		}
		if j.state.AcknowledgedThrough != nil && sequence <= *j.state.AcknowledgedThrough {
			if err := os.Remove(filepath.Join(j.dir, entry.Name())); err != nil {
				return err
			}
			continue
		}
		if sequence >= j.nextSegment {
			j.nextSegment = sequence + 1
		}
		if !entry.Type().IsRegular() {
			return errors.New("journal contains non-regular segment")
		}
		p := filepath.Join(j.dir, entry.Name())
		f, e := os.OpenFile(p, os.O_RDWR, 0)
		if e != nil {
			return e
		}
		seg := &recordingSegment{path: p, sequence: sequence}
		for {
			r, n, e := ReadRecording(f)
			if e == io.EOF && n == 0 {
				break
			}
			if e == io.ErrUnexpectedEOF {
				if e = f.Truncate(seg.size); e != nil {
					f.Close()
					return e
				}
				break
			}
			if e != nil {
				f.Close()
				return fmt.Errorf("recover recording: %w", e)
			}
			stamp := time.Unix(0, r.ReceiptUnixNanos)
			if currentBoot != "unavailable" && r.ReceiptBootId == currentBoot && r.ReceiptBoottimeNanos >= 0 && r.ReceiptBoottimeNanos <= bootNow {
				stamp = observedNow.Add(time.Duration(r.ReceiptBoottimeNanos - bootNow))
			}
			if seg.size == 0 {
				seg.created = stamp
			}
			seg.size += n
			seg.last = stamp
			if r.Record.Id != "" {
				j.ids[r.Record.Id] = recordingID{hash: recordHash(r.Record)}
				seg.ids = append(seg.ids, r.Record.Id)
			}
		}
		// Recovery may find a complete write whose acknowledgement was lost or
		// whose previous sync failed. Re-sync before any duplicate acknowledgement.
		e = j.syncFile(f)
		f.Close()
		if e != nil {
			return e
		}
		j.segments = append(j.segments, seg)
		j.size += seg.size
	}
	if err := j.saveState(); err != nil {
		return err
	}
	return j.expire()
}
func (j *recordingJournal) expire() error {
	if j.state.RetentionSeconds == 0 {
		return nil
	}
	cutoff := j.now().Add(-time.Duration(j.state.RetentionSeconds) * time.Second)
	changed := false
	for len(j.segments) > 0 && j.segments[0].last.Before(cutoff) {
		seg := j.segments[0]
		if err := os.Remove(seg.path); err != nil {
			return err
		}
		for _, id := range seg.ids {
			delete(j.ids, id)
		}
		j.size -= seg.size
		j.segments = j.segments[1:]
		changed = true
	}
	if changed {
		return syncDirectory(j.dir)
	}
	return nil
}
func (j *recordingJournal) appendConfigured(r *recordingpb.StoredRecord, storage *appconfig.RecordingStorage) (bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failure != nil {
		return false, j.failure
	}
	if err := j.configure(storage); err != nil {
		return false, err
	}
	if err := j.expire(); err != nil {
		return false, err
	}
	h := recordHash(r.Record)
	if old, ok := j.ids[r.Record.Id]; ok {
		if old.hash != h {
			return false, errors.New("record ID already used with different content")
		}
		return true, nil
	}
	size := int64(proto.Size(r) + 8)
	if j.size+size > j.state.MaxBytes {
		return false, errors.New("recording quota reached; unexpired records retained")
	}
	if len(j.ids) >= 1_000_000 {
		return false, errors.New("recording ID limit reached; export retained records or batch samples")
	}
	var seg *recordingSegment
	if len(j.segments) > 0 {
		seg = j.segments[len(j.segments)-1]
	}
	if seg == nil || j.state.SealedThrough != nil && seg.sequence <= *j.state.SealedThrough || seg.size+size > recordingSegmentBytes || j.now().Sub(seg.created) > time.Hour {
		sequence := j.nextSegment
		if sequence == ^uint64(0) {
			return false, errors.New("recording segment sequence exhausted")
		}
		j.nextSegment++
		// Never reuse a sequence, including after every segment is reclaimed.
		if err := j.saveState(); err != nil {
			return false, err
		}
		p := filepath.Join(j.dir, fmt.Sprintf("%020d.wdr", sequence))
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return false, err
		}
		if err = f.Close(); err != nil {
			return false, err
		}
		seg = &recordingSegment{path: p, sequence: sequence, created: j.now()}
		j.segments = append(j.segments, seg)
	}
	f, err := os.OpenFile(seg.path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return false, err
	}
	err = WriteRecording(f, r)
	if err == nil {
		err = j.syncFile(f)
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil && seg.size == 0 {
		err = syncDirectory(j.dir)
	}
	if err != nil {
		j.failure = fmt.Errorf("journal requires recovery after write failure: %w", err)
		return false, j.failure
	}
	seg.size += size
	seg.last = j.now()
	seg.ids = append(seg.ids, r.Record.Id)
	j.size += size
	j.ids[r.Record.Id] = recordingID{hash: h}
	return false, nil
}

// Open bounded readers under the journal lock; unlinking reclaimed segments
// cannot invalidate an in-flight export's descriptors.
func (j *recordingJournal) openSnapshot(segments []*recordingSegment) ([]*os.File, []int64, error) {
	var files []*os.File
	var sizes []int64
	for _, seg := range segments {
		f, err := os.Open(seg.path)
		if err != nil {
			for _, f := range files {
				f.Close()
			}
			return nil, nil, err
		}
		files = append(files, f)
		sizes = append(sizes, seg.size)
	}
	return files, sizes, nil
}
func (m *Manager) ExportRecording(app, service, stream string, visit func(*recordingpb.StoredRecord) error) error {
	_, err := m.ExportRecordingCheckpoint(app, service, stream, false, visit)
	return err
}
func (m *Manager) ExportRecordingCheckpoint(app, service, stream string, checkpoint bool, visit func(*recordingpb.StoredRecord) error) (string, error) {
	if _, err := os.Stat(filepath.Join(m.recordings.root, recordingKey(app, service, stream))); err != nil {
		return "", err
	}
	j, err := m.recordings.journal(app, service, stream)
	if err != nil {
		return "", err
	}
	files, sizes, token, err := j.snapshotCheckpoint(checkpoint)
	if err != nil {
		return "", err
	}
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	for i, f := range files {
		r := io.NewSectionReader(f, 0, sizes[i])
		for {
			v, _, err := ReadRecording(r)
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", err
			}
			if err = visit(v); err != nil {
				return "", err
			}
		}
	}
	return token, nil
}
