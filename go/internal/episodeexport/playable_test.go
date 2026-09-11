package episodeexport

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// realSPS and realPPS are the parameter sets from an episode recorded on a
// Jetson at 1280x720, so the SPS parser is exercised against the bitstream the
// device actually produces rather than a hand-made one.
const (
	realSPS = "6764001facb200a00b7602dc08081a94000003000400000300f23c60c920"
	realPPS = "68ebccb22c"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decoding fixture: %v", err)
	}
	return b
}

func TestSPSDimensionsFromRealStream(t *testing.T) {
	w, h, err := spsDimensions(mustHex(t, realSPS))
	if err != nil {
		t.Fatalf("spsDimensions: %v", err)
	}
	if w != 1280 || h != 720 {
		t.Errorf("got %dx%d, want 1280x720", w, h)
	}
}

func TestSplitAnnexBHandlesBothStartCodePrefixes(t *testing.T) {
	// Three units: a 4-byte prefix, then a 3-byte prefix, then a 4-byte prefix.
	stream := []byte{0, 0, 0, 1, 0x67, 0xAA, 0, 0, 1, 0x68, 0xBB, 0xCC, 0, 0, 0, 1, 0x65, 0xDD}
	units := splitAnnexB(stream)
	if len(units) != 3 {
		t.Fatalf("got %d units, want 3: %v", len(units), units)
	}
	for i, want := range [][]byte{{0x67, 0xAA}, {0x68, 0xBB, 0xCC}, {0x65, 0xDD}} {
		if string(units[i]) != string(want) {
			t.Errorf("unit %d = % x, want % x", i, units[i], want)
		}
	}
}

// annexB frames the given NAL units as an Annex-B access unit.
func annexB(nals ...[]byte) []byte {
	var out []byte
	for _, n := range nals {
		out = append(out, 0, 0, 0, 1)
		out = append(out, n...)
	}
	return out
}

// buildEpisode lays out a synthetic episode with two camera sources. cam-a
// spans two segments with deliberately irregular frame intervals; cam-b holds
// one frame whose index entry points past the end of its segment plus a partial
// trailing line, which is the shape an interrupted episode leaves behind.
func buildEpisode(t *testing.T) (dir string, wantIntervals []time.Duration) {
	t.Helper()
	dir = t.TempDir()
	sps, pps := mustHex(t, realSPS), mustHex(t, realPPS)
	idr := append([]byte{0x65}, make([]byte, 40)...)
	slice := append([]byte{0x41}, make([]byte, 30)...)

	// Intervals chosen so that no two are equal: a constant rate could not
	// reproduce them, so the test fails if one is ever assumed.
	gaps := []time.Duration{25 * time.Millisecond, 61 * time.Millisecond, 104 * time.Millisecond, 33 * time.Millisecond}
	wantIntervals = gaps

	type entry struct {
		CanonicalEpisodeNanos int64  `json:"canonical_episode_nanos"`
		Segment               string `json:"segment"`
		ByteOffset            int64  `json:"byte_offset"`
		ByteSize              int    `json:"byte_size"`
		Codec                 string `json:"codec"`
	}

	camA := filepath.Join(dir, "cameras", "cam-a")
	if err := os.MkdirAll(camA, 0o755); err != nil {
		t.Fatal(err)
	}
	// Each segment opens with SPS/PPS/IDR, as the capture pipeline arranges.
	first := annexB(sps, pps, idr)
	later := annexB(slice)
	var (
		seg1, seg2 []byte
		lines      []entry
		now        = int64(5_000_000_000) // an arbitrary non-zero episode origin
	)
	for i := 0; i < 3; i++ {
		payload := later
		if i == 0 {
			payload = first
		}
		lines = append(lines, entry{now, "cameras/cam-a/segment-000001.h264", int64(len(seg1)), len(payload), "h264"})
		seg1 = append(seg1, payload...)
		now += int64(gaps[i])
	}
	// A second segment, opening with its own parameter sets and keyframe.
	lines = append(lines, entry{now, "cameras/cam-a/segment-000002.h264", 0, len(first), "h264"})
	seg2 = append(seg2, first...)
	now += int64(gaps[3])
	lines = append(lines, entry{now, "cameras/cam-a/segment-000002.h264", int64(len(seg2)), len(later), "h264"})
	seg2 = append(seg2, later...)

	write := func(path string, b []byte) {
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(camA, "segment-000001.h264"), seg1)
	write(filepath.Join(camA, "segment-000002.h264"), seg2)
	var index []byte
	for _, l := range lines {
		b, _ := json.Marshal(l)
		index = append(index, b...)
		index = append(index, '\n')
	}
	write(filepath.Join(camA, "index.jsonl"), index)

	camB := filepath.Join(dir, "cameras", "cam-b")
	if err := os.MkdirAll(camB, 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(camB, "segment-000001.h264"), first)
	bIndex, _ := json.Marshal(entry{1_000, "cameras/cam-b/segment-000001.h264", 0, len(first), "h264"})
	missing, _ := json.Marshal(entry{50_000_000, "cameras/cam-b/segment-000001.h264", 1 << 20, 999, "h264"})
	write(filepath.Join(camB, "index.jsonl"),
		append(append(append(bIndex, '\n'), append(missing, '\n')...), []byte(`{"canonical_episode_na`)...))
	return dir, wantIntervals
}

func TestConvertRealShapes(t *testing.T) {
	dir, gaps := buildEpisode(t)
	before := snapshot(t, dir)
	out := filepath.Join(t.TempDir(), "playable")

	results, errs := Convert(dir, out)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want one per camera source", len(results))
	}

	byName := map[string]ClipResult{}
	for _, r := range results {
		byName[r.Source] = r
	}

	a := byName["cam-a"]
	if a.IndexLines != 5 || a.Frames != 5 {
		t.Errorf("cam-a: index lines %d, frames %d; want 5 and 5", a.IndexLines, a.Frames)
	}
	if a.Segments != 2 {
		t.Errorf("cam-a read %d segments, want 2", a.Segments)
	}
	if a.Skipped != 0 {
		t.Errorf("cam-a skipped %d frames, want 0", a.Skipped)
	}
	var wantSpan time.Duration
	for _, g := range gaps {
		wantSpan += g
	}
	if a.Span != wantSpan {
		t.Errorf("cam-a span %s, want %s", a.Span, wantSpan)
	}
	// The whole point: the spread of intervals survives. A muxer that assumed
	// any single rate would report min == max == mean here.
	if a.MinInterval != 25*time.Millisecond || a.MaxInterval != 104*time.Millisecond {
		t.Errorf("cam-a intervals min %s max %s; want 25ms and 104ms", a.MinInterval, a.MaxInterval)
	}
	if a.MinInterval == a.MaxInterval {
		t.Error("cam-a interval spread was flattened to a constant rate")
	}
	// Segment 2 re-inlines the same SPS/PPS before its keyframe, which is the
	// normal encoder habit; identical repeats are not a parameter set change.
	if a.ParameterSetChanges != 0 {
		t.Errorf("cam-a reports %d parameter set changes for identical repeated SPS/PPS, want 0", a.ParameterSetChanges)
	}

	// A frame whose bytes are missing is reported and skipped, and a partial
	// trailing line is counted as unusable, but the clip is still written.
	b := byName["cam-b"]
	if b.Frames != 1 || b.Skipped != 1 || b.Unparsed != 1 {
		t.Errorf("cam-b: frames %d, skipped %d, unparsed %d; want 1, 1, 1", b.Frames, b.Skipped, b.Unparsed)
	}
	if b.Output == "" {
		t.Error("cam-b produced no file despite having one good frame")
	}

	// The MP4's sample table must carry each recorded interval explicitly.
	assertSampleDurations(t, a.Output, gaps)

	// The episode itself is archival truth and must be untouched.
	if after := snapshot(t, dir); after != before {
		t.Errorf("episode directory was modified:\nbefore %s\nafter  %s", before, after)
	}
}

// assertSampleDurations decodes the stts box and checks that the durations are
// exactly the recorded intervals, with the final sample held for the last
// measured interval.
func assertSampleDurations(t *testing.T, path string, gaps []time.Duration) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	i := indexOf(b, []byte("stts"))
	if i < 0 {
		t.Fatal("no stts box in output")
	}
	count := binary.BigEndian.Uint32(b[i+8:])
	var got []time.Duration
	for e := 0; e < int(count); e++ {
		off := i + 12 + e*8
		n := binary.BigEndian.Uint32(b[off:])
		d := binary.BigEndian.Uint32(b[off+4:])
		for k := uint32(0); k < n; k++ {
			got = append(got, time.Duration(d)*time.Microsecond)
		}
	}
	want := append(append([]time.Duration{}, gaps...), gaps[len(gaps)-1])
	if len(got) != len(want) {
		t.Fatalf("stts holds %d samples (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for k := range want {
		if got[k] != want[k] {
			t.Errorf("sample %d duration %s, want %s", k, got[k], want[k])
		}
	}
}

func indexOf(haystack, needle []byte) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// snapshot fingerprints every file in dir by name, size and content, so any
// modification to the episode is detectable.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	out := ""
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out += fmt.Sprintf("%s:%d:%x\n", rel, len(b), sum(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sum(b []byte) uint64 {
	var h uint64 = 14695981039346656037
	for _, c := range b {
		h = (h ^ uint64(c)) * 1099511628211
	}
	return h
}

func TestConvertSourceInPlaceWritesBesideRawCapture(t *testing.T) {
	dir, _ := buildEpisode(t)
	before := snapshot(t, dir)

	result, err := ConvertSourceInPlace(dir, filepath.Join(dir, "cameras", "cam-a"))
	if err != nil {
		t.Fatalf("ConvertSourceInPlace: %v", err)
	}
	if result.Source != "cam-a" || result.Frames != 5 {
		t.Errorf("got source %q with %d frames, want cam-a with 5", result.Source, result.Frames)
	}
	out := filepath.Join(dir, "cameras", "cam-a", PlayableFileName)
	if result.Output != out {
		t.Errorf("output %q, want %q", result.Output, out)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("no playable file in place: %v", err)
	}
	if _, err := os.Stat(out + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temporary mux file left behind")
	}

	// Everything that existed before must be byte-identical: the raw stream
	// and index are checksummed archival truth, and index.jsonl addresses
	// frames by byte offset into the raw segments.
	if err := os.Remove(out); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, dir); after != before {
		t.Errorf("raw capture was modified:\nbefore %s\nafter  %s", before, after)
	}
}

// restartSPS and restartPPS are a second camera's parameter sets at 640x480,
// the shape a producer restart splices into a running episode when it comes
// back at a different resolution.
const (
	restartSPS = "6742c01e8c8d40501e900f08846a"
	restartPPS = "68ce3c80"
)

// A producer restart mid-episode splices new parameter sets into the stream.
// The MP4 written here carries exactly one decoder configuration (the first
// SPS/PPS), so every frame after the change would decode against the wrong
// parameters; the result must say so, because nothing else about the mux
// fails: the frames all copy cleanly, slice headers parse, and sync samples
// exist.
func TestConvertSourceInPlaceFlagsParameterSetChange(t *testing.T) {
	dir := t.TempDir()
	sps1, pps1 := mustHex(t, realSPS), mustHex(t, realPPS)
	sps2, pps2 := mustHex(t, restartSPS), mustHex(t, restartPPS)
	if w, h, err := spsDimensions(sps2); err != nil || (w == 1280 && h == 720) {
		t.Fatalf("restart SPS fixture must parse to a different resolution, got %dx%d err=%v", w, h, err)
	}
	idr := append([]byte{0x65, 0x88}, make([]byte, 40)...)
	p := append([]byte{0x41, 0xC0}, make([]byte, 30)...)

	cam := filepath.Join(dir, "cameras", "cam-a")
	if err := os.MkdirAll(cam, 0o755); err != nil {
		t.Fatal(err)
	}
	type entry struct {
		CanonicalEpisodeNanos int64  `json:"canonical_episode_nanos"`
		Segment               string `json:"segment"`
		ByteOffset            int64  `json:"byte_offset"`
		ByteSize              int    `json:"byte_size"`
		Codec                 string `json:"codec"`
	}
	units := [][]byte{
		annexB(sps1, pps1, idr), annexB(p),
		annexB(sps2, pps2, idr), annexB(p), // the producer came back at 640x480
	}
	var seg, index []byte
	now := int64(1_000_000_000)
	for _, u := range units {
		line, err := json.Marshal(entry{now, "cameras/cam-a/segment-000001.h264", int64(len(seg)), len(u), "h264"})
		if err != nil {
			t.Fatal(err)
		}
		index = append(index, line...)
		index = append(index, '\n')
		seg = append(seg, u...)
		now += 33_000_000
	}
	if err := os.WriteFile(filepath.Join(cam, "segment-000001.h264"), seg, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cam, "index.jsonl"), index, 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := ConvertSourceInPlace(dir, cam)
	if err != nil {
		t.Fatalf("ConvertSourceInPlace: %v", err)
	}
	if r.ParameterSetChanges != 2 {
		t.Errorf("ParameterSetChanges = %d, want 2 (one changed SPS, one changed PPS)", r.ParameterSetChanges)
	}
	// The other gates must stay quiet: this failure mode is invisible to them,
	// which is exactly why it needs its own counter.
	if r.Frames != 4 || r.Skipped != 0 || r.BFrames || r.UndecodedSliceHeaders != 0 || r.SyncSamples == 0 {
		t.Errorf("unexpected companion signals: frames %d skipped %d bframes %v undecoded %d sync %d",
			r.Frames, r.Skipped, r.BFrames, r.UndecodedSliceHeaders, r.SyncSamples)
	}
}

func TestConvertRejectsNonEpisodeDirectory(t *testing.T) {
	_, errs := Convert(t.TempDir(), filepath.Join(t.TempDir(), "out"))
	if len(errs) == 0 {
		t.Fatal("expected an error for a directory holding no camera index")
	}
}

func TestIsBSlice(t *testing.T) {
	// first_mb_in_slice = 0 ("1"), slice_type = 1 ("010") -> B slice.
	if isB, parsed := isBSlice([]byte{0x41, 0xA0}); !isB || !parsed {
		t.Errorf("slice_type 1 should be a parsed B slice, got isB=%v parsed=%v", isB, parsed)
	}
	// first_mb_in_slice = 0 ("1"), slice_type = 0 ("1") -> P slice.
	if isB, parsed := isBSlice([]byte{0x41, 0xC0}); isB || !parsed {
		t.Errorf("slice_type 0 should be a parsed P slice, got isB=%v parsed=%v", isB, parsed)
	}
	// A header too short to carry a slice type is unknown, not B-free: the
	// caller must be able to tell "no B slices" from "could not tell".
	if isB, parsed := isBSlice([]byte{0x41}); isB || parsed {
		t.Errorf("a truncated slice header should report unknown, got isB=%v parsed=%v", isB, parsed)
	}
}

// writeSingleSourceEpisode lays out an episode with one camera source whose
// index holds exactly the given entries, all pointing into one segment built
// from the payloads. It is the smallest fixture that lets a test choose the
// canonical timestamps and byte ranges an index records.
func writeSingleSourceEpisode(t *testing.T, stamps []int64, payloads [][]byte) (dir, sourceDir string) {
	t.Helper()
	if len(stamps) != len(payloads) {
		t.Fatalf("fixture needs one timestamp per payload: %d vs %d", len(stamps), len(payloads))
	}
	dir = t.TempDir()
	sourceDir = filepath.Join(dir, "cameras", "cam")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	type entry struct {
		CanonicalEpisodeNanos int64  `json:"canonical_episode_nanos"`
		Segment               string `json:"segment"`
		ByteOffset            int64  `json:"byte_offset"`
		ByteSize              int    `json:"byte_size"`
		Codec                 string `json:"codec"`
	}
	var segment, index []byte
	for i, payload := range payloads {
		b, _ := json.Marshal(entry{stamps[i], "cameras/cam/segment-000001.h264", int64(len(segment)), len(payload), "h264"})
		index = append(append(index, b...), '\n')
		segment = append(segment, payload...)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "segment-000001.h264"), segment, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "index.jsonl"), index, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, sourceDir
}

// keyframeAndInter returns one random-access access unit and one inter frame,
// both built from the real parameter sets.
func keyframeAndInter(t *testing.T) (key, inter []byte) {
	t.Helper()
	sps, pps := mustHex(t, realSPS), mustHex(t, realPPS)
	return annexB(sps, pps, append([]byte{0x65}, make([]byte, 40)...)),
		annexB(append([]byte{0x41}, make([]byte, 30)...))
}

// A gap a 32-bit sample duration cannot carry must be refused by name rather
// than wrapping silently to a short duration.
func TestMuxRefusesGapBeyondSampleDurationLimit(t *testing.T) {
	key, inter := keyframeAndInter(t)
	// Two frames 80 minutes apart: beyond the 71 minutes and change that fits
	// in a 32-bit duration at microsecond ticks.
	gap := int64(80 * time.Minute)
	dir, sourceDir := writeSingleSourceEpisode(t, []int64{0, gap}, [][]byte{key, inter})

	_, err := ConvertSourceInPlace(dir, sourceDir)
	if err == nil {
		t.Fatal("an 80-minute inter-frame gap was muxed; it cannot fit a 32-bit sample duration")
	}
	if !strings.Contains(err.Error(), "exceeds") || !strings.Contains(err.Error(), "sample duration") {
		t.Fatalf("error does not name the limit: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(sourceDir, PlayableFileName)); !os.IsNotExist(statErr) {
		t.Fatalf("a refused mux left a playable file behind: %v", statErr)
	}
}

// Two inverted timestamps must not reorder the coded frames: capture order is
// kept, the inversion is counted, and the clip is refused at seal time.
func TestInvertedTimestampsKeepCaptureOrderAndAreCounted(t *testing.T) {
	key, inter := keyframeAndInter(t)
	// The third and fourth entries are swapped in time but not in the index.
	stamps := []int64{0, 10_000_000, 30_000_000, 20_000_000}
	payloads := [][]byte{key, inter, inter, inter}
	dir, sourceDir := writeSingleSourceEpisode(t, stamps, payloads)

	frames, result, err := readCameraIndex(filepath.Join(sourceDir, "index.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if result.TimestampInversions != 1 {
		t.Fatalf("inversions counted = %d, want 1", result.TimestampInversions)
	}
	for i, want := range stamps {
		if frames[i].CanonicalEpisodeNanos != want {
			t.Fatalf("frame %d canonical = %d, want %d: capture order was not preserved",
				i, frames[i].CanonicalEpisodeNanos, want)
		}
	}

	clip, err := ConvertSourceInPlace(dir, sourceDir)
	if err != nil {
		t.Fatalf("the clip should still be written for the caller to judge: %v", err)
	}
	if clip.TimestampInversions != 1 {
		t.Fatalf("ClipResult inversions = %d, want 1", clip.TimestampInversions)
	}
}

// An index naming a byte_size near MaxInt64 must be refused by the bounds
// check rather than overflowing the sum and panicking in make().
func TestSegmentReadRefusesByteSizeNearMaxInt64(t *testing.T) {
	dir := t.TempDir()
	segDir := filepath.Join(dir, "cameras", "cam")
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(segDir, "segment-000001.h264"), make([]byte, 64), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &segmentReader{root: dir, seen: map[string]bool{}}
	defer s.Close()

	// An offset whose sum with the size wraps back into a small positive
	// number is the case a naive `offset+size > size-on-disk` check passes.
	huge := int(int64(1)<<62) + 8
	for _, frame := range []cameraIndexLine{
		{Segment: "cameras/cam/segment-000001.h264", ByteOffset: 1 << 62, ByteSize: huge},
		{Segment: "cameras/cam/segment-000001.h264", ByteOffset: 0, ByteSize: huge},
		{Segment: "cameras/cam/segment-000001.h264", ByteOffset: -1, ByteSize: 8},
	} {
		if _, err := s.read(frame); err == nil {
			t.Fatalf("read accepted offset %d size %d against a 64-byte segment", frame.ByteOffset, frame.ByteSize)
		}
	}
}

// A clip holding one frame gets a nominal hold rather than a zero-duration
// sample, and says so.
func TestSingleFrameClipGetsNominalHold(t *testing.T) {
	key, _ := keyframeAndInter(t)
	dir, sourceDir := writeSingleSourceEpisode(t, []int64{7_000_000_000}, [][]byte{key})

	clip, err := ConvertSourceInPlace(dir, sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	if clip.Frames != 1 {
		t.Fatalf("frames written = %d, want 1", clip.Frames)
	}
	want := time.Duration(singleFrameNominalTicks) * (time.Second / playableTimescale)
	if clip.NominalHold != want {
		t.Fatalf("nominal hold = %s, want %s", clip.NominalHold, want)
	}
	// The movie header must report that duration, not zero.
	b, err := os.ReadFile(clip.Output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, u32(uint32(singleFrameNominalTicks))) {
		t.Fatal("the sample table carries no nominal duration; players would see an empty movie")
	}
}
