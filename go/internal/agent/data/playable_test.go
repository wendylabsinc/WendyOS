package data

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/episodeexport"
)

// The parameter sets are the ones a Jetson episode really carried, matching
// the episodeexport package's own fixtures, so the seal-time mux is exercised
// against the bitstream shape the device produces.
const (
	sealTestSPS = "6764001facb200a00b7602dc08081a94000003000400000300f23c60c920"
	sealTestPPS = "68ebccb22c"
)

// Slice NAL units whose headers the muxer can and cannot parse. The seal only
// keeps a clip whose timing it can vouch for, so the distinction is load
// bearing: an IDR or P slice with a parseable header is publishable, a B
// slice or an unparseable header is not.
var (
	parsedIDR     = append([]byte{0x65, 0x88}, make([]byte, 40)...) // slice_type 7 (I), parses
	parsedPSlice  = append([]byte{0x41, 0xC0}, make([]byte, 30)...) // slice_type 0 (P), parses
	parsedBSlice  = append([]byte{0x41, 0xA0}, make([]byte, 30)...) // slice_type 1 (B), parses
	unparsedSlice = append([]byte{0x41}, make([]byte, 30)...)       // header is all zero bits, cannot parse
)

func annexB(nals ...[]byte) []byte {
	var out []byte
	for _, n := range nals {
		out = append(out, 0, 0, 0, 1)
		out = append(out, n...)
	}
	return out
}

// writeCameraSource lays one camera source into an active episode directory:
// a single segment holding the given access units at irregular intervals, and
// the index.jsonl that records where each frame's bytes landed.
func writeCameraSource(t *testing.T, episodeDir, source string, accessUnits [][]byte) {
	t.Helper()
	sps, err := hex.DecodeString(sealTestSPS)
	if err != nil {
		t.Fatal(err)
	}
	pps, err := hex.DecodeString(sealTestPPS)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(episodeDir, "cameras", source)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	segment := "cameras/" + source + "/segment-000001.h264"
	// Irregular gaps on purpose: the remux must carry recorded timestamps,
	// never a rate, and equal gaps could hide a muxer that assumed one.
	gaps := []int64{0, 33_000_000, 95_000_000, 41_000_000, 62_000_000, 27_000_000}
	var (
		seg   []byte
		index []byte
		now   = int64(2_000_000_000)
	)
	for i, unit := range accessUnits {
		payload := unit
		if i == 0 {
			payload = append(annexB(sps, pps), unit...)
		}
		now += gaps[i%len(gaps)]
		line, err := json.Marshal(map[string]any{
			"canonical_episode_nanos": now,
			"segment":                 segment,
			"byte_offset":             len(seg),
			"byte_size":               len(payload),
			"codec":                   "h264",
		})
		if err != nil {
			t.Fatal(err)
		}
		seg = append(seg, payload...)
		index = append(index, line...)
		index = append(index, '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "segment-000001.h264"), seg, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.jsonl"), index, 0o644); err != nil {
		t.Fatal(err)
	}
}

func fileByPath(files []File, path string) (File, bool) {
	for _, f := range files {
		if f.Path == path {
			return f, true
		}
	}
	return File{}, false
}

func TestSealWritesPlayableClipListedInManifest(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The camera is declared as an episode source, exactly as a device with a
	// camera declares it. A sealed file is attributed to the source the
	// manifest names, never to the directory component it happens to sit
	// under, which is a lossy encoding of an identifier and not one.
	m.SetSourceProvider(func(context.Context) []Source {
		return []Source{{ID: "cam-front", Kind: "camera", ClockDomain: "CLOCK_BOOTTIME", Healthy: true}}
	})
	if _, err = m.Start(StartOptions{Sources: []string{"applications", "cam-front"}}); err != nil {
		t.Fatal(err)
	}
	session, ok := m.ActiveSession(AdHocEpisodeKey)
	if !ok {
		t.Fatal("no active session")
	}
	writeCameraSource(t, session.Directory, "cam-front", [][]byte{
		annexB(parsedIDR), annexB(parsedPSlice), annexB(parsedPSlice),
	})
	stopped, err := m.Stop(AdHocEpisodeKey)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != "complete" {
		t.Fatalf("state %q, want complete", stopped.State)
	}
	if len(stopped.PlayableNotes) != 0 {
		t.Fatalf("clean mux left notes: %v", stopped.PlayableNotes)
	}

	rel := "cameras/cam-front/" + episodeexport.PlayableFileName
	entry, ok := fileByPath(stopped.Files, rel)
	if !ok {
		t.Fatalf("manifest does not list %s: %+v", rel, stopped.Files)
	}
	if entry.Role != FileRoleDerived {
		t.Errorf("derived clip role %q, want %q", entry.Role, FileRoleDerived)
	}
	if entry.Format != "mp4" || entry.MediaType != "video/mp4" {
		t.Errorf("derived clip format %q media type %q, want mp4/video/mp4", entry.Format, entry.MediaType)
	}
	if entry.SourceID != "cam-front" {
		t.Errorf("derived clip source %q, want cam-front", entry.SourceID)
	}

	// The listed size and SHA-256 must be the finished file's, so the
	// transfer worker uploads it and commit-time verification covers it with
	// no special casing.
	onDisk := filepath.Join(m.root, stopped.ID, rel)
	hash, size, err := checksum(onDisk)
	if err != nil {
		t.Fatalf("derived clip missing from sealed episode: %v", err)
	}
	if entry.SHA256 != hash || entry.Size != size {
		t.Errorf("manifest entry (%d bytes, %s) does not match file (%d bytes, %s)", entry.Size, entry.SHA256, size, hash)
	}
	if size == 0 {
		t.Error("derived clip is empty")
	}

	// The raw capture must still be listed, unmarked: only the remux is
	// derived, and payload accounting keys on the raw files via the index.
	for _, raw := range []string{"cameras/cam-front/segment-000001.h264", "cameras/cam-front/index.jsonl"} {
		f, ok := fileByPath(stopped.Files, raw)
		if !ok {
			t.Fatalf("raw capture file %s missing from manifest", raw)
		}
		if f.Role != "" {
			t.Errorf("raw capture file %s carries role %q, want none", raw, f.Role)
		}
	}

	// Full verification passes with the derived file treated like any other
	// listed file.
	if _, failures, err := m.Inspect(stopped.ID, true); err != nil || len(failures) != 0 {
		t.Fatalf("verification: err=%v failures=%v", err, failures)
	}

	// The derived clip counts against the episode's size, and so against the
	// quota that size feeds.
	infos, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, f := range stopped.Files {
		total += f.Size
	}
	if len(infos) != 1 || infos[0].SizeBytes != total {
		t.Fatalf("episode size %d does not include every listed file (want %d)", infos[0].SizeBytes, total)
	}
	var withoutDerived int64
	for _, f := range stopped.Files {
		if f.Role != FileRoleDerived {
			withoutDerived += f.Size
		}
	}
	if withoutDerived >= total {
		t.Error("derived clip contributed nothing to the episode size")
	}
}

func TestSealRefusesClipItCannotVouchFor(t *testing.T) {
	cases := []struct {
		name       string
		accessUnit []byte
		wantNote   string
	}{
		{"b_slices", annexB(parsedBSlice), "B slices"},
		{"unparsed_slice_header", annexB(unparsedSlice), "slice header"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := NewManager(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = m.Start(StartOptions{Sources: []string{"applications"}}); err != nil {
				t.Fatal(err)
			}
			session, ok := m.ActiveSession(AdHocEpisodeKey)
			if !ok {
				t.Fatal("no active session")
			}
			writeCameraSource(t, session.Directory, "cam-front", [][]byte{
				annexB(parsedIDR), tc.accessUnit,
			})
			stopped, err := m.Stop(AdHocEpisodeKey)
			if err != nil {
				t.Fatalf("a refused mux must never fail the seal: %v", err)
			}
			if stopped.State != "complete" {
				t.Fatalf("state %q, want complete", stopped.State)
			}
			rel := "cameras/cam-front/" + episodeexport.PlayableFileName
			if _, ok := fileByPath(stopped.Files, rel); ok {
				t.Errorf("manifest lists %s despite the refusal", rel)
			}
			if _, err := os.Stat(filepath.Join(m.root, stopped.ID, rel)); !os.IsNotExist(err) {
				t.Errorf("refused clip left on disk (stat err %v)", err)
			}
			if len(stopped.PlayableNotes) != 1 || !strings.Contains(stopped.PlayableNotes[0], tc.wantNote) {
				t.Errorf("notes %v do not name the refusal (%q)", stopped.PlayableNotes, tc.wantNote)
			}
			if !strings.Contains(stopped.PlayableNotes[0], rel+" not written") {
				t.Errorf("note %q does not name the missing file", stopped.PlayableNotes[0])
			}
			if _, failures, err := m.Inspect(stopped.ID, true); err != nil || len(failures) != 0 {
				t.Fatalf("verification: err=%v failures=%v", err, failures)
			}
		})
	}
}

// A producer restart mid-episode splices new SPS/PPS into the raw stream. The
// derived clip carries exactly one decoder configuration, so such a stream
// must seal without its playable.mp4 and say why; no other gate notices,
// because every frame still copies cleanly.
func TestSealRefusesClipAfterParameterSetChange(t *testing.T) {
	// A second camera's parameter sets at 640x480, byte-different from the
	// 1280x720 pair writeCameraSource lays down at the head of the stream.
	sps2, err := hex.DecodeString("6742c01e8c8d40501e900f08846a")
	if err != nil {
		t.Fatal(err)
	}
	pps2, err := hex.DecodeString("68ce3c80")
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Start(StartOptions{Sources: []string{"applications"}}); err != nil {
		t.Fatal(err)
	}
	session, ok := m.ActiveSession(AdHocEpisodeKey)
	if !ok {
		t.Fatal("no active session")
	}
	writeCameraSource(t, session.Directory, "cam-front", [][]byte{
		annexB(parsedIDR), annexB(parsedPSlice),
		annexB(sps2, pps2, parsedIDR), annexB(parsedPSlice),
	})
	stopped, err := m.Stop(AdHocEpisodeKey)
	if err != nil {
		t.Fatalf("a refused mux must never fail the seal: %v", err)
	}
	rel := "cameras/cam-front/" + episodeexport.PlayableFileName
	if _, ok := fileByPath(stopped.Files, rel); ok {
		t.Errorf("manifest lists %s despite the mid-episode parameter set change", rel)
	}
	if _, err := os.Stat(filepath.Join(m.root, stopped.ID, rel)); !os.IsNotExist(err) {
		t.Errorf("refused clip left on disk (stat err %v)", err)
	}
	if len(stopped.PlayableNotes) != 1 || !strings.Contains(stopped.PlayableNotes[0], "parameter sets change") {
		t.Errorf("notes %v do not name the parameter set change", stopped.PlayableNotes)
	}
}

// The mux can fail on plain I/O (a full disk mid-write follows the same error
// path: the write error surfaces, the .tmp is removed, and the seal goes on).
// Provoke an open failure with an unwritable source directory and hold the
// seal to its contract: the episode completes, the note says why, and nothing
// half-written is listed or left behind.
func TestSealSurvivesMuxWriteFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, directory permissions do not deny writes")
	}
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Start(StartOptions{Sources: []string{"applications"}}); err != nil {
		t.Fatal(err)
	}
	session, ok := m.ActiveSession(AdHocEpisodeKey)
	if !ok {
		t.Fatal("no active session")
	}
	writeCameraSource(t, session.Directory, "cam-front", [][]byte{
		annexB(parsedIDR), annexB(parsedPSlice),
	})
	sourceDir := filepath.Join(session.Directory, "cameras", "cam-front")
	if err := os.Chmod(sourceDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sourceDir, 0o755) })

	stopped, err := m.Stop(AdHocEpisodeKey)
	if err != nil {
		t.Fatalf("a failed mux must never fail the seal: %v", err)
	}
	// The seal renamed the episode directory; restore write permission at its
	// final location so the test's temporary tree can be removed.
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(m.root, stopped.ID, "cameras", "cam-front"), 0o755) })
	if stopped.State != "complete" {
		t.Fatalf("state %q, want complete", stopped.State)
	}
	rel := "cameras/cam-front/" + episodeexport.PlayableFileName
	if _, ok := fileByPath(stopped.Files, rel); ok {
		t.Errorf("manifest lists %s despite the mux failure", rel)
	}
	if len(stopped.PlayableNotes) != 1 || !strings.Contains(stopped.PlayableNotes[0], rel+" not written") {
		t.Errorf("notes %v do not name the unwritten clip", stopped.PlayableNotes)
	}
	for _, f := range stopped.Files {
		if strings.HasSuffix(f.Path, ".tmp") {
			t.Errorf("manifest lists temporary file %s", f.Path)
		}
	}
	// The raw capture sealed untouched and verifiable.
	if _, failures, err := m.Inspect(stopped.ID, true); err != nil || len(failures) != 0 {
		t.Fatalf("verification: err=%v failures=%v", err, failures)
	}
}

func TestSealRefusesClipWithoutRandomAccessFrame(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Start(StartOptions{Sources: []string{"applications"}}); err != nil {
		t.Fatal(err)
	}
	session, ok := m.ActiveSession(AdHocEpisodeKey)
	if !ok {
		t.Fatal("no active session")
	}
	// Parameter sets but never an IDR: nothing a decoder can start on.
	writeCameraSource(t, session.Directory, "cam-front", [][]byte{
		annexB(parsedPSlice), annexB(parsedPSlice),
	})
	stopped, err := m.Stop(AdHocEpisodeKey)
	if err != nil {
		t.Fatal(err)
	}
	rel := "cameras/cam-front/" + episodeexport.PlayableFileName
	if _, ok := fileByPath(stopped.Files, rel); ok {
		t.Errorf("manifest lists %s despite it having no sync sample", rel)
	}
	if len(stopped.PlayableNotes) != 1 || !strings.Contains(stopped.PlayableNotes[0], "random-access") {
		t.Errorf("notes %v do not name the missing random-access frame", stopped.PlayableNotes)
	}
}

func TestRecoverySealsPlayableClipToo(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	started, err := m.Start(StartOptions{Sources: []string{"applications"}})
	if err != nil {
		t.Fatal(err)
	}
	writeCameraSource(t, filepath.Join(root, started.ID+".partial"), "cam-front", [][]byte{
		annexB(parsedIDR), annexB(parsedPSlice),
	})
	// A new manager over the same root recovers the abandoned partial.
	m2, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	mf, failures, err := m2.Inspect(started.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if mf.State != "interrupted" {
		t.Fatalf("state %q, want interrupted", mf.State)
	}
	if len(failures) != 0 {
		t.Fatalf("verification failures: %v", failures)
	}
	rel := "cameras/cam-front/" + episodeexport.PlayableFileName
	entry, ok := fileByPath(mf.Files, rel)
	if !ok {
		t.Fatalf("recovered manifest does not list %s", rel)
	}
	if entry.Role != FileRoleDerived {
		t.Errorf("recovered clip role %q, want %q", entry.Role, FileRoleDerived)
	}
	if len(mf.PlayableNotes) != 0 {
		t.Errorf("clean recovery mux left notes: %v", mf.PlayableNotes)
	}
}

// A crash after the mux but before the manifest is written leaves a
// playable.mp4 (and possibly its .tmp) in the partial directory whose frames
// may postdate the index tail recovery truncates. Recovery must remux from
// the truncated index, never reuse the stale clip, and clean up the .tmp.
func TestRecoveryReplacesStaleClipAndTemporary(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	started, err := m.Start(StartOptions{Sources: []string{"applications"}})
	if err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(root, started.ID+".partial")
	writeCameraSource(t, partial, "cam-front", [][]byte{
		annexB(parsedIDR), annexB(parsedPSlice),
	})
	sourceDir := filepath.Join(partial, "cameras", "cam-front")
	// The crash left a clip muxed from a longer index than the one recovery
	// will keep, plus the temporary file of a mux cut off mid-write. Neither
	// may survive as-is: the clip must be rebuilt from the truncated index,
	// the temporary removed and never listed.
	stale := []byte("stale clip muxed before the crash truncated the index")
	if err := os.WriteFile(filepath.Join(sourceDir, episodeexport.PlayableFileName), stale, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, episodeexport.PlayableFileName+".tmp"), []byte("half a mux"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A partial trailing index line, the shape a crash leaves.
	idx, err := os.OpenFile(filepath.Join(sourceDir, "index.jsonl"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.WriteString(`{"canonical_episode_na`); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	m2, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	mf, failures, err := m2.Inspect(started.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 0 {
		t.Fatalf("verification failures: %v", failures)
	}
	rel := "cameras/cam-front/" + episodeexport.PlayableFileName
	entry, ok := fileByPath(mf.Files, rel)
	if !ok {
		t.Fatalf("recovered manifest does not list %s", rel)
	}
	onDisk, err := os.ReadFile(filepath.Join(root, started.ID, rel))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) == string(stale) {
		t.Error("recovery reused the stale pre-crash clip instead of remuxing")
	}
	if entry.Size == int64(len(stale)) {
		t.Errorf("listed clip size %d matches the stale clip", entry.Size)
	}
	if _, err := os.Stat(filepath.Join(root, started.ID, rel+".tmp")); !os.IsNotExist(err) {
		t.Errorf("stray mux temporary survived recovery (stat err %v)", err)
	}
	for _, f := range mf.Files {
		if strings.HasSuffix(f.Path, ".tmp") {
			t.Errorf("recovered manifest lists temporary file %s", f.Path)
		}
	}
}

func TestIsDerivedPlayable(t *testing.T) {
	for path, want := range map[string]bool{
		"cameras/cam-front/playable.mp4":        true,
		"cameras/cam-front/segment-000001.h264": false,
		"playable.mp4":                          false,
		"cameras/playable.mp4":                  false,
		"audio/mic/playable.mp4":                false,
		"cameras/cam/deeper/playable.mp4":       false,
	} {
		if got := isDerivedPlayable(path); got != want {
			t.Errorf("isDerivedPlayable(%q) = %v, want %v", path, got, want)
		}
	}
}

// The seal must not publish a clip whose index timestamps run backwards: the
// remux preserves capture order, so a clip built from an inverted index has
// timing nobody can vouch for.
func TestSealRefusesClipWithInvertedTimestamps(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Start(StartOptions{Sources: []string{"applications"}}); err != nil {
		t.Fatal(err)
	}
	session, ok := m.ActiveSession(AdHocEpisodeKey)
	if !ok {
		t.Fatal("no active session")
	}
	writeCameraSource(t, session.Directory, "cam-front", [][]byte{
		annexB(parsedIDR), annexB(parsedPSlice), annexB(parsedPSlice),
	})
	invertLastTwoIndexTimestamps(t, filepath.Join(session.Directory, "cameras", "cam-front", "index.jsonl"))

	stopped, err := m.Stop(AdHocEpisodeKey)
	if err != nil {
		t.Fatal(err)
	}
	rel := "cameras/cam-front/" + episodeexport.PlayableFileName
	if _, ok := fileByPath(stopped.Files, rel); ok {
		t.Errorf("manifest lists %s despite its index timestamps inverting", rel)
	}
	if len(stopped.PlayableNotes) != 1 || !strings.Contains(stopped.PlayableNotes[0], "earlier than the entry before them") {
		t.Errorf("notes %v do not name the inversion", stopped.PlayableNotes)
	}
	if _, statErr := os.Stat(filepath.Join(session.Directory, "cameras", "cam-front", episodeexport.PlayableFileName)); !os.IsNotExist(statErr) {
		t.Errorf("a refused clip was left on disk: %v", statErr)
	}
}

// invertLastTwoIndexTimestamps swaps the canonical timestamps of the last two
// index entries, leaving the entries themselves in capture order.
func invertLastTwoIndexTimestamps(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 2 {
		t.Fatalf("index holds %d entries, need at least two to invert", len(lines))
	}
	var a, b map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-2]), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &b); err != nil {
		t.Fatal(err)
	}
	a["canonical_episode_nanos"], b["canonical_episode_nanos"] = b["canonical_episode_nanos"], a["canonical_episode_nanos"]
	for i, rec := range []map[string]any{a, b} {
		encoded, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		lines[len(lines)-2+i] = string(encoded)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A panic anywhere in the remux must become an ordinary mux error: the seal
// writes the episode itself and must never be brought down by a derived
// artifact that can be rebuilt from the raw capture at any time.
func TestConvertPlayableClipTurnsAPanicIntoAnError(t *testing.T) {
	dir := t.TempDir()
	// A source directory with no index at all: ConvertSourceInPlace returns an
	// ordinary error, which proves the recover wrapper passes errors through.
	if _, err := convertPlayableClip(dir, filepath.Join(dir, "cameras", "cam")); err == nil {
		t.Fatal("expected an error for a source with no index")
	}
	original := convertClip
	t.Cleanup(func() { convertClip = original })
	convertClip = func(string, string) (episodeexport.ClipResult, error) {
		panic("synthetic mux panic")
	}
	_, err := convertPlayableClip(dir, filepath.Join(dir, "cameras", "cam"))
	if err == nil {
		t.Fatal("a panic inside the remux escaped instead of becoming a mux error")
	}
	if !strings.Contains(err.Error(), "synthetic mux panic") {
		t.Fatalf("error does not name the panic: %v", err)
	}
}
