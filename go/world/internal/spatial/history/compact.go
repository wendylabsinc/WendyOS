package history

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Compaction reports what a Compact call did.
type Compaction struct {
	// Horizon is the retention horizon now in force.
	Horizon time.Time
	// Dropped is the number of records removed, Kept the number the new
	// snapshot holds, Segments the number of segment files removed.
	Dropped, Kept, Segments int
}

// snapshotHeader is the first frame of a snapshot file.
type snapshotHeader struct {
	Through uint64    `json:"through"` // every record up to this sequence is in the snapshot or dropped
	Horizon time.Time `json:"horizon"`
	Last    time.Time `json:"last"`    // newest apply time journaled when the snapshot was written
	Records int       `json:"records"` // record frames that follow
}

// Compact moves the retention horizon forward to horizon. Segments whose
// every record was applied before it are replaced by a snapshot holding only
// what a query at or after the horizon can still return: per key, the record
// in force at the horizon on each axis, and any record sampled at or after
// it. Queries at or after the horizon are unaffected; queries before it
// return ErrCompacted from now on. A horizon at or before the current one
// changes nothing.
//
// ponytail: space comes back a whole segment at a time. A segment that
// straddles the horizon stays until a later compaction passes its end; lower
// Config.SegmentBytes for a tighter bound, or rewrite the straddling segment
// if that ever matters. Tombstoned keys keep their tombstone for good, as a
// world.Replica keeps its high-water mark.
func (j *Journal) Compact(horizon time.Time) (Compaction, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.usable(); err != nil {
		return Compaction{}, err
	}
	if horizon.IsZero() {
		return Compaction{}, fmt.Errorf("%w: zero horizon", ErrInvalid)
	}
	return j.compact(horizon.Round(0).UTC())
}

func (j *Journal) compact(horizon time.Time) (Compaction, error) {
	if !horizon.After(j.horizon) {
		return Compaction{Horizon: j.horizon}, nil
	}
	if seg := j.active(); seg.size > 0 && seg.last.Before(horizon) {
		if err := j.roll(); err != nil {
			return Compaction{}, err
		}
	}
	// Apply times never decrease along the segments, so the ones wholly
	// before the horizon are a prefix. The active segment always stays.
	n := 0
	for n < len(j.segments)-1 && j.segments[n].last.Before(horizon) {
		n++
	}
	old := map[*file]bool{}
	var through uint64
	if j.snapshot != nil {
		old[j.snapshot], through = true, j.snapshot.seq
	}
	for _, seg := range j.segments[:n] {
		old[seg] = true
	}
	if n > 0 {
		through = j.segments[n].seq - 1
	}

	// Every record in the old files was applied before the horizon. One is
	// still owed to a query at T >= horizon only if it is the key's state at
	// the horizon on either axis, or was sampled at or after the horizon:
	// anything else is outranked at every such T by the record in force at
	// the horizon, which is kept or lives in a later segment.
	var keep []*entry
	dropped := 0
	for _, s := range j.keys {
		byApply, bySample := s.at(horizon, ApplyTime), s.at(horizon, SampleTime)
		for _, e := range s.entries {
			switch {
			case !old[e.file]:
			case e == byApply || e == bySample || !e.sample.Before(horizon):
				keep = append(keep, e)
			default:
				dropped++
			}
		}
	}
	slices.SortFunc(keep, func(a, b *entry) int { return cmp.Compare(a.seq, b.seq) })

	snap, offsets, err := j.writeSnapshot(snapshotHeader{Through: through, Horizon: horizon, Last: j.last, Records: len(keep)}, keep)
	if err != nil {
		return Compaction{}, err
	}
	// The snapshot is in place: from here the old files are garbage that
	// Open would also remove, so commit in memory before deleting them.
	for i, e := range keep {
		e.file, e.off = snap, offsets[i]
	}
	for _, s := range j.keys {
		s.entries = slices.DeleteFunc(s.entries, func(e *entry) bool { return old[e.file] })
	}
	var errs []error
	for f := range old {
		errs = append(errs, f.f.Close(), os.Remove(f.path))
	}
	j.snapshot, j.segments, j.horizon = snap, j.segments[n:], horizon
	j.generation++
	j.records -= dropped
	return Compaction{Horizon: horizon, Dropped: dropped, Kept: len(keep), Segments: n}, errors.Join(errs...)
}

// writeSnapshot writes the header and the frames of keep, verbatim and in
// order, to a temporary file, makes it durable and renames it into place as
// the next snapshot generation. It returns the open snapshot and the offset
// of each frame in it.
func (j *Journal) writeSnapshot(h snapshotHeader, keep []*entry) (snap *file, offsets []int64, err error) {
	path := filepath.Join(j.dir, fileName(snapshotPrefix, j.generation+1, snapshotSuffix))
	tmp := path + tmpSuffix
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	head, err := json.Marshal(h)
	if err != nil {
		return nil, nil, err
	}
	w := bufio.NewWriterSize(f, 64<<10)
	size := int64(frameHeader + len(head))
	if _, err = w.Write(appendFrame(nil, head)); err != nil {
		return nil, nil, err
	}
	offsets = make([]int64, len(keep))
	for i, e := range keep {
		frame, _, rerr := j.raw(e)
		if err = rerr; err != nil {
			return nil, nil, err
		}
		if _, err = w.Write(frame); err != nil {
			return nil, nil, err
		}
		offsets[i] = size
		size += int64(len(frame))
	}
	if err = w.Flush(); err != nil {
		return nil, nil, err
	}
	if err = f.Sync(); err != nil {
		return nil, nil, err
	}
	// Close before the rename and reopen after: renaming an open file does
	// not work on every platform.
	if err = f.Close(); err != nil {
		return nil, nil, err
	}
	if err = os.Rename(tmp, path); err != nil {
		return nil, nil, err
	}
	if err = syncDir(j.dir); err != nil {
		return nil, nil, err
	}
	r, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return &file{path: path, f: r, seq: h.Through, size: size}, offsets, nil
}

func (j *Journal) loadSnapshot(generation uint64) error {
	path := filepath.Join(j.dir, fileName(snapshotPrefix, generation, snapshotSuffix))
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	snap := &file{path: path, f: f}
	j.snapshot, j.generation = snap, generation
	st, err := f.Stat()
	if err != nil {
		return err
	}
	var head snapshotHeader
	var prev uint64
	records := -1 // the header has not been read
	good, torn, err := scan(snap, st.Size(), func(body []byte, off int64) error {
		if records < 0 {
			if json.Unmarshal(body, &head) != nil || head.Horizon.IsZero() || head.Records < 0 {
				return corrupt(path, off, "snapshot header")
			}
			records = 0
			return nil
		}
		var rec Record
		if json.Unmarshal(body, &rec) != nil || check(rec.Component) != nil {
			return corrupt(path, off, "record does not decode")
		}
		if rec.Seq <= prev || rec.Seq > head.Through || rec.Applied.After(head.Last) {
			return corrupt(path, off, fmt.Sprintf("record %d out of order", rec.Seq))
		}
		if !j.index(rec, snap, off, frameHeader+len(body)) {
			return corrupt(path, off, "key and version journaled twice")
		}
		prev = rec.Seq
		records++
		return nil
	})
	if err != nil {
		return err
	}
	// A snapshot is renamed into place whole, so it has no torn tail.
	if torn || records != head.Records {
		return corrupt(path, good, fmt.Sprintf("snapshot holds %d of %d records", records, head.Records))
	}
	snap.seq, snap.size = head.Through, good
	j.nextSeq, j.horizon, j.last = head.Through+1, head.Horizon, head.Last
	return nil
}
