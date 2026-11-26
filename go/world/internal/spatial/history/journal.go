package history

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

var (
	// ErrInvalid reports an argument the journal cannot act on: a component
	// without a key, version, sample time or lifetime, an unknown Axis, a
	// reversed time range, a bad Config.
	ErrInvalid = errors.New("history: invalid argument")
	// ErrConflict reports an Append of a key and version already journaled
	// with different content. Nothing is written.
	ErrConflict = errors.New("history: same key and version with different content")
	// ErrClockRegression reports that the injected clock read earlier than
	// the newest journaled record, at Append. Nothing is written: apply
	// times are the journal's order and are never invented.
	ErrClockRegression = errors.New("history: injected clock moved backwards")
	// ErrCorrupt reports stored bytes that fail their checks anywhere but in
	// a torn final frame. The error text names the file and the offset.
	ErrCorrupt = errors.New("history: corrupt journal")
	// ErrCompacted reports a query time before the retention horizon.
	ErrCompacted = errors.New("history: time is before the retention horizon")
	// ErrClosed reports use after Close.
	ErrClosed = errors.New("history: journal is closed")
	// ErrNotPose reports a Trajectory request for a component that does not
	// project to exactly one positioned spatial.Entity.
	ErrNotPose = errors.New("history: component has no pose")
)

// DefaultSegmentBytes is the default Config.SegmentBytes.
const DefaultSegmentBytes = 64 << 20

// Config holds the journal's injected clock and its storage policy.
type Config struct {
	// Now is the clock that stamps each appended record with its apply time.
	// Required. It must not run backwards, across restarts included: give it
	// the clock of the world.Replica whose state is being journaled.
	Now func() time.Time

	// SegmentBytes is the size at which the active segment is sealed and a
	// new one started. It is also the granularity of compaction, which
	// removes whole segments, and each retained segment holds one file
	// descriptor. Default DefaultSegmentBytes (64 MiB).
	SegmentBytes int64

	// Retention, when positive, compacts to (now - Retention) each time a
	// segment is sealed, so the directory holds about Retention of history
	// plus one segment. Default 0: keep everything until Compact is called.
	// The directory then grows without bound.
	Retention time.Duration
}

// Record is one journaled delta.
type Record struct {
	// Seq numbers records in the order they were appended, from 1.
	Seq uint64 `json:"seq"`
	// Applied is the injected clock at Append, in UTC.
	Applied time.Time `json:"applied"`
	// Component is the upsert, or the tombstone when Component.Tombstone.
	Component world.Component `json:"component"`
}

// Stats describes what a journal holds.
type Stats struct {
	Records  int   // records retained, the snapshot's included
	Keys     int   // distinct component keys
	Segments int   // segment files, the active one included
	Bytes    int64 // size of the segments and the snapshot
	// Horizon is the retention horizon; zero when nothing was compacted.
	Horizon time.Time
	// LastApplied is the apply time of the newest record ever appended.
	LastApplied time.Time
	// TruncatedBytes is the length of the torn tail dropped by Open.
	TruncatedBytes int64
}

// Journal is an open history directory. See the package documentation.
type Journal struct {
	mu  sync.RWMutex
	dir string
	cfg Config

	// ponytail: the index holds every retained record's metadata (about
	// 150 bytes each) in memory and Open rebuilds it by reading every
	// retained file. Fine for the hours-to-days a Retention keeps; beyond
	// that, write a per-segment index sidecar at seal and load it lazily.
	keys       map[world.Key]*series
	snapshot   *file   // nil until the first compaction
	generation uint64  // of the snapshot: each compaction writes the next
	segments   []*file // ascending by first sequence; the last takes appends
	nextSeq    uint64
	last       time.Time // newest apply time ever journaled
	horizon    time.Time
	records    int

	truncated int64
	closed    bool
	failed    error // set when a failed write could not be undone
}

// file is one segment or the snapshot.
type file struct {
	path string
	f    *os.File
	// seq is a segment's first sequence number, or the last sequence a
	// snapshot covers: every earlier record is in the snapshot or dropped.
	seq  uint64
	size int64
	last time.Time // newest apply time in a segment; zero when empty
}

// series is the retained history of one key.
type series struct {
	key     world.Key
	order   string   // JSON of key: the order world.Replica.Snapshot uses
	entries []*entry // ascending by version, which is unique within a key
}

// entry locates one record and carries what queries need without reading it.
type entry struct {
	seq       uint64
	version   world.Version
	sample    time.Time
	applied   time.Time
	expires   time.Time // on the apply axis, as world.Replica computes it
	ttl       time.Duration
	tombstone bool
	file      *file
	off       int64 // of the frame
	size      int   // of the frame
}

const (
	segmentPrefix, segmentSuffix   = "seg-", ".seg"
	snapshotPrefix, snapshotSuffix = "snap-", ".snap"
	tmpSuffix                      = ".tmp"

	// A frame is a 12-byte header (body length, CRC-32C of those four bytes,
	// CRC-32C of the body; little-endian) followed by the body. The length
	// has its own checksum so that a flipped length bit reads as corruption
	// and not as a torn tail that swallows the records behind it.
	frameHeader = 12
	// maxFrame bounds a body. A World component is at most 64 KiB on the
	// wire; this leaves room and still caps what a bad length can allocate.
	maxFrame = 4 << 20
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func appendFrame(dst, body []byte) []byte {
	var h [frameHeader]byte
	binary.LittleEndian.PutUint32(h[0:], uint32(len(body)))
	binary.LittleEndian.PutUint32(h[4:], crc32.Checksum(h[:4], castagnoli))
	binary.LittleEndian.PutUint32(h[8:], crc32.Checksum(body, castagnoli))
	return append(append(dst, h[:]...), body...)
}

type frameState int

const (
	frameOK frameState = iota
	frameEnd
	frameTorn    // an append cut short: nothing valid can follow
	frameCorrupt // damage that is not explained by a cut-short append
)

// readFrame reads the frame at the head of r, which has remaining bytes left.
// A torn frame is what a crashed append leaves: a header or body that runs
// past the end of the file, a body ending exactly at the end of the file that
// fails its checksum, or zero fill to the end of the file.
func readFrame(r io.Reader, remaining int64) ([]byte, frameState, error) {
	if remaining == 0 {
		return nil, frameEnd, nil
	}
	if remaining < frameHeader {
		return nil, frameTorn, nil
	}
	var h [frameHeader]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, 0, err
	}
	n := int64(binary.LittleEndian.Uint32(h[0:]))
	if crc32.Checksum(h[:4], castagnoli) != binary.LittleEndian.Uint32(h[4:]) || n == 0 || n > maxFrame {
		if h != [frameHeader]byte{} {
			return nil, frameCorrupt, nil
		}
		for buf := make([]byte, 32<<10); ; {
			k, err := r.Read(buf)
			if slices.ContainsFunc(buf[:k], func(b byte) bool { return b != 0 }) {
				return nil, frameCorrupt, nil
			}
			if errors.Is(err, io.EOF) {
				return nil, frameTorn, nil
			}
			if err != nil {
				return nil, 0, err
			}
		}
	}
	if frameHeader+n > remaining {
		return nil, frameTorn, nil
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, 0, err
	}
	if crc32.Checksum(body, castagnoli) != binary.LittleEndian.Uint32(h[8:]) {
		if frameHeader+n == remaining {
			return nil, frameTorn, nil
		}
		return nil, frameCorrupt, nil
	}
	return body, frameOK, nil
}

func corrupt(path string, off int64, what string) error {
	return fmt.Errorf("%w: %s at offset %d: %s", ErrCorrupt, filepath.Base(path), off, what)
}

// scan calls fn with the body and offset of each whole frame in the first
// size bytes of f. It returns the offset after the last whole frame and
// whether a torn tail follows it.
func scan(f *file, size int64, fn func(body []byte, off int64) error) (good int64, torn bool, err error) {
	r := bufio.NewReaderSize(io.NewSectionReader(f.f, 0, size), 64<<10)
	for off := int64(0); ; {
		body, state, err := readFrame(r, size-off)
		switch {
		case err != nil:
			return off, false, err
		case state == frameEnd:
			return off, false, nil
		case state == frameTorn:
			return off, true, nil
		case state == frameCorrupt:
			return off, false, corrupt(f.path, off, "frame fails its checksum")
		}
		if err := fn(body, off); err != nil {
			return off, false, err
		}
		off += frameHeader + int64(len(body))
	}
}

// check is the validation Append applies at its boundary: what the index and
// the queries depend on. Admission (scope, source, freshness, provenance) is
// the replica's judgement, not the journal's.
func check(c world.Component) error {
	switch {
	case c.Key.Entity == "" || c.Key.Type == "":
		return fmt.Errorf("%w: component key", ErrInvalid)
	case c.Version.Epoch == 0 || c.Version.Sequence == 0:
		return fmt.Errorf("%w: component version", ErrInvalid)
	case c.TTL <= 0:
		return fmt.Errorf("%w: component TTL", ErrInvalid)
	case c.Timing.Validate() != nil:
		return fmt.Errorf("%w: component timing", ErrInvalid)
	case c.Tombstone && len(c.Payload) != 0:
		return fmt.Errorf("%w: tombstone carries a payload", ErrInvalid)
	case !c.Tombstone && !json.Valid(c.Payload):
		return fmt.Errorf("%w: component payload is not JSON", ErrInvalid)
	}
	return nil
}

// expiry is the instant a record applied at now stops being live, computed as
// world.Replica.Apply does so that an ApplyTime reconstruction honouring TTL
// equals the replica's own view.
func expiry(now time.Time, c world.Component) time.Time {
	age := max(now.Sub(c.Timing.SampleTime), 0)
	if c.Timing.ErrorBound > math.MaxInt64-age {
		age = math.MaxInt64
	} else {
		age += c.Timing.ErrorBound
	}
	age = max(age, c.Timing.AgeHint)
	return now.Add(max(c.TTL-age, 0))
}

// Open opens the journal in dir, creating the directory when it is missing,
// and rebuilds the index from the snapshot and the segments. A torn final
// frame is truncated; any other damage returns ErrCorrupt. One process owns a
// directory at a time.
func Open(dir string, cfg Config) (*Journal, error) {
	if dir == "" || cfg.Now == nil || cfg.SegmentBytes < 0 || cfg.Retention < 0 {
		return nil, fmt.Errorf("%w: directory, clock, segment size or retention", ErrInvalid)
	}
	if cfg.SegmentBytes == 0 {
		cfg.SegmentBytes = DefaultSegmentBytes
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	j := &Journal{dir: dir, cfg: cfg, keys: map[world.Key]*series{}, nextSeq: 1}
	if err := j.load(); err != nil {
		return nil, errors.Join(err, j.closeFiles())
	}
	return j, nil
}

func fileName(prefix string, seq uint64, suffix string) string {
	return fmt.Sprintf("%s%016x%s", prefix, seq, suffix)
}

func parseName(name, prefix, suffix string) (uint64, bool) {
	hex, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return 0, false
	}
	if hex, ok = strings.CutSuffix(hex, suffix); !ok || len(hex) != 16 {
		return 0, false
	}
	seq, err := strconv.ParseUint(hex, 16, 64)
	return seq, err == nil
}

func (j *Journal) load() error {
	names, err := os.ReadDir(j.dir)
	if err != nil {
		return err
	}
	var snapshots, segments []uint64
	for _, d := range names {
		if strings.HasSuffix(d.Name(), tmpSuffix) {
			// A compaction that never reached its rename.
			if err := os.Remove(filepath.Join(j.dir, d.Name())); err != nil {
				return err
			}
		} else if seq, ok := parseName(d.Name(), snapshotPrefix, snapshotSuffix); ok {
			snapshots = append(snapshots, seq)
		} else if seq, ok := parseName(d.Name(), segmentPrefix, segmentSuffix); ok {
			segments = append(segments, seq)
		}
	}
	slices.Sort(snapshots)
	slices.Sort(segments)
	if n := len(snapshots); n > 0 {
		if err := j.loadSnapshot(snapshots[n-1]); err != nil {
			return err
		}
		// A crash between a snapshot's rename and the removal of what it
		// replaced leaves an older snapshot and covered segments behind.
		for _, old := range snapshots[:n-1] {
			if err := os.Remove(filepath.Join(j.dir, fileName(snapshotPrefix, old, snapshotSuffix))); err != nil {
				return err
			}
		}
		for len(segments) > 0 && segments[0] <= j.snapshot.seq {
			if err := os.Remove(filepath.Join(j.dir, fileName(segmentPrefix, segments[0], segmentSuffix))); err != nil {
				return err
			}
			segments = segments[1:]
		}
	}
	for i, seq := range segments {
		if err := j.loadSegment(seq, i == len(segments)-1); err != nil {
			return err
		}
	}
	if len(j.segments) == 0 {
		return j.addSegment()
	}
	return nil
}

func (j *Journal) loadSegment(first uint64, active bool) error {
	path := filepath.Join(j.dir, fileName(segmentPrefix, first, segmentSuffix))
	if first != j.nextSeq {
		return corrupt(path, 0, fmt.Sprintf("segment starts at sequence %d, expected %d", first, j.nextSeq))
	}
	flag := os.O_RDONLY
	if active {
		flag = os.O_RDWR | os.O_APPEND
	}
	f, err := os.OpenFile(path, flag, 0)
	if err != nil {
		return err
	}
	seg := &file{path: path, f: f, seq: first}
	j.segments = append(j.segments, seg)
	st, err := f.Stat()
	if err != nil {
		return err
	}
	// Apply times never decrease along the segments. j.last is not the
	// reference: after a compaction it is the newest apply time ever seen,
	// which the surviving segments lead up to.
	var prev time.Time
	if n := len(j.segments); n > 1 {
		prev = j.segments[n-2].last
	}
	good, torn, err := scan(seg, st.Size(), func(body []byte, off int64) error {
		var rec Record
		if json.Unmarshal(body, &rec) != nil || check(rec.Component) != nil {
			return corrupt(path, off, "record does not decode")
		}
		if rec.Seq != j.nextSeq || rec.Applied.Before(prev) {
			return corrupt(path, off, fmt.Sprintf("record %d out of order, expected %d", rec.Seq, j.nextSeq))
		}
		if !j.index(rec, seg, off, frameHeader+len(body)) {
			return corrupt(path, off, "key and version journaled twice")
		}
		j.nextSeq, prev, seg.last = rec.Seq+1, rec.Applied, rec.Applied
		if rec.Applied.After(j.last) {
			j.last = rec.Applied
		}
		return nil
	})
	if err != nil {
		return err
	}
	if torn {
		if !active {
			return corrupt(path, good, "sealed segment ends in a partial frame")
		}
		if err := f.Truncate(good); err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
		j.truncated = st.Size() - good
	}
	seg.size = good
	return nil
}

// addSegment starts the segment that takes the next record.
func (j *Journal) addSegment() error {
	path := filepath.Join(j.dir, fileName(segmentPrefix, j.nextSeq, segmentSuffix))
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	j.segments = append(j.segments, &file{path: path, f: f, seq: j.nextSeq})
	return syncDir(j.dir)
}

// roll seals the active segment, which must hold a record, and starts the next.
func (j *Journal) roll() error {
	if err := j.active().f.Sync(); err != nil {
		return err
	}
	return j.addSegment()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (j *Journal) active() *file { return j.segments[len(j.segments)-1] }

func (j *Journal) usable() error {
	if j.closed {
		return ErrClosed
	}
	return j.failed
}

// index adds a record to the in-memory index. It reports false, and changes
// nothing, when the key already holds that version.
func (j *Journal) index(rec Record, f *file, off int64, size int) bool {
	c := rec.Component
	s := j.keys[c.Key]
	if s == nil {
		order, _ := json.Marshal(c.Key) // strings only: cannot fail
		s = &series{key: c.Key, order: string(order)}
		j.keys[c.Key] = s
	}
	i, found := s.find(c.Version)
	if found {
		return false
	}
	s.entries = slices.Insert(s.entries, i, &entry{
		seq: rec.Seq, version: c.Version, sample: c.Timing.SampleTime, applied: rec.Applied,
		expires: expiry(rec.Applied, c), ttl: c.TTL, tombstone: c.Tombstone,
		file: f, off: off, size: size,
	})
	j.records++
	return true
}

func (s *series) find(v world.Version) (int, bool) {
	i := sort.Search(len(s.entries), func(i int) bool { return s.entries[i].version.Compare(v) >= 0 })
	return i, i < len(s.entries) && s.entries[i].version == v
}

// raw reads and verifies the frame of e.
func (j *Journal) raw(e *entry) (frame, body []byte, err error) {
	frame = make([]byte, e.size)
	if _, err := e.file.f.ReadAt(frame, e.off); err != nil {
		return nil, nil, err
	}
	body, state, err := readFrame(bytes.NewReader(frame), int64(len(frame)))
	if err != nil || state != frameOK {
		return nil, nil, corrupt(e.file.path, e.off, "frame fails its checksum")
	}
	return frame, body, nil
}

func (j *Journal) read(e *entry) (Record, error) {
	var rec Record
	_, body, err := j.raw(e)
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(body, &rec); err != nil {
		return rec, corrupt(e.file.path, e.off, "record does not decode")
	}
	return rec, nil
}

// sameContent compares two components as world.Replica identifies a version:
// everything but the unsigned transit age hint.
func sameContent(a, b world.Component) (bool, error) {
	a.Timing.AgeHint, b.Timing.AgeHint = 0, 0
	ja, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	jb, err := json.Marshal(b)
	return bytes.Equal(ja, jb), err
}

// Append journals one delta, stamped with the injected clock. It reports
// false when that key and version are already journaled with the same
// content, which makes a replay after a restart harmless, and ErrConflict
// when the content differs. A version below the key's newest is journaled
// too: it never changes the ApplyTime state but it is part of the SampleTime
// history.
//
// The record is in the file when Append returns and durable after the next
// Sync, Close or segment seal.
func (j *Journal) Append(c world.Component) (bool, error) {
	if err := check(c); err != nil {
		return false, err
	}
	// As world.Replica.Apply: a sample time is a cross-process assertion and
	// carries no local monotonic reading.
	c.Timing.SampleTime = c.Timing.SampleTime.Round(0)

	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.usable(); err != nil {
		return false, err
	}
	now := j.cfg.Now().Round(0).UTC()
	if now.IsZero() || now.Before(j.last) {
		return false, ErrClockRegression
	}
	if s := j.keys[c.Key]; s != nil {
		if i, found := s.find(c.Version); found {
			// ponytail: a duplicate carrying a larger age hint shortens the
			// record's life in a world.Replica; that is not journaled. Add
			// an expiry-clamp record if synced duplicates must replay exactly.
			old, err := j.read(s.entries[i])
			if err != nil {
				return false, err
			}
			if same, err := sameContent(old.Component, c); err != nil {
				return false, fmt.Errorf("%w: %v", ErrInvalid, err)
			} else if !same {
				return false, ErrConflict
			}
			return false, nil
		}
	}
	rec := Record{Seq: j.nextSeq, Applied: now, Component: c}
	body, err := json.Marshal(rec)
	if err != nil || len(body) > maxFrame {
		return false, fmt.Errorf("%w: component does not encode within %d bytes", ErrInvalid, maxFrame)
	}
	if j.active().size >= j.cfg.SegmentBytes {
		if err := j.roll(); err != nil {
			return false, err
		}
		if j.cfg.Retention > 0 {
			if _, err := j.compact(now.Add(-j.cfg.Retention)); err != nil {
				return false, err
			}
		}
	}
	seg := j.active()
	frame := appendFrame(make([]byte, 0, frameHeader+len(body)), body)
	if _, err := seg.f.Write(frame); err != nil {
		// Take the partial frame back out, or the next record would sit
		// behind garbage. If that fails too the journal stops accepting.
		if terr := seg.f.Truncate(seg.size); terr != nil {
			j.failed = fmt.Errorf("history: journal stopped after a failed write: %w", errors.Join(err, terr))
		}
		return false, err
	}
	j.index(rec, seg, seg.size, len(frame))
	seg.size += int64(len(frame))
	j.nextSeq, j.last, seg.last = rec.Seq+1, now, now
	return true, nil
}

// Sync makes every appended record durable.
func (j *Journal) Sync() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.usable(); err != nil {
		return err
	}
	return j.active().f.Sync()
}

// Close syncs and releases the directory. Every later call returns ErrClosed.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	return errors.Join(j.active().f.Sync(), j.closeFiles())
}

func (j *Journal) closeFiles() error {
	var errs []error
	if j.snapshot != nil {
		errs = append(errs, j.snapshot.f.Close())
	}
	for _, seg := range j.segments {
		errs = append(errs, seg.f.Close())
	}
	return errors.Join(errs...)
}

// Stats reports what the journal holds.
func (j *Journal) Stats() Stats {
	j.mu.RLock()
	defer j.mu.RUnlock()
	st := Stats{Records: j.records, Keys: len(j.keys), Segments: len(j.segments), Horizon: j.horizon, LastApplied: j.last, TruncatedBytes: j.truncated}
	if j.snapshot != nil {
		st.Bytes = j.snapshot.size
	}
	for _, seg := range j.segments {
		st.Bytes += seg.size
	}
	return st
}
