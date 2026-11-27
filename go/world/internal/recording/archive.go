// Package recording stores bounded sensor histories and their exact media bytes.
// It does not independently verify historical origin signatures.
package recording

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/replay"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const Schema = "wendy.recording.v1"
const MaxRecords = 1024
const MaxManifestBytes = 8 << 20
const MaxTotalBytes = 64 << 20
const Integrity = "replica-capture-no-historical-signature-proof"

type Manifest struct {
	SchemaVersion string            `json:"schema_version"`
	Integrity     string            `json:"integrity"`
	Tenant        string            `json:"tenant"`
	Site          string            `json:"site"`
	RunID         string            `json:"run_id"`
	Frame         world.FrameRef    `json:"frame"`
	StartedAt     time.Time         `json:"started_at"`
	EndedAt       time.Time         `json:"ended_at"`
	RecordedAt    time.Time         `json:"recorded_at"`
	CompletedAt   time.Time         `json:"completed_at"`
	Records       []world.Component `json:"records"`
}
type Bundle struct {
	Manifest Manifest
	Root     []byte
	SHA256   string
	Blobs    map[string][]byte
}

func Digest(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func identity(c world.Component) string {
	b, _ := json.Marshal(struct {
		Key     world.Key
		Version world.Version
	}{c.Key, c.Version})
	return string(b)
}
func sourceID(c world.Component) string { b, _ := json.Marshal(c.Key.Source); return string(b) }
func captureStart(c world.Component, s sensing.Sample) time.Time {
	t := c.Timing.SampleTime
	if s.Audio != nil {
		t = t.Add(-time.Duration(s.Audio.DurationNS))
	}
	return t
}

func ValidateOriginal(c world.Component) (sensing.Sample, error) {
	s, err := replay.ValidateOriginal(c)
	if err != nil {
		return s, err
	}
	if c.TTL > 24*time.Hour || !c.Timing.ErrorKnown || c.Timing.ErrorBound >= c.TTL || c.Timing.AgeHint >= c.TTL {
		return s, errors.New("original record has unbounded or unknown freshness")
	}
	if s.Audio != nil && c.Timing.AgeHint+time.Duration(s.Audio.DurationNS) >= c.TTL {
		return s, errors.New("recorded audio was already stale at reception")
	}
	return s, nil
}

// DecodeOriginal preserves required zero-valued metadata before JSON decoding
// could turn an absent uncertainty or simulation seed into a false known zero.
func DecodeOriginal(raw []byte) (world.Component, error) {
	c, err := replay.DecodeOriginal(raw)
	if err != nil {
		return c, err
	}
	_, err = ValidateOriginal(c)
	return c, err
}

func decodeManifest(raw []byte) (Manifest, error) {
	var m Manifest
	if e := authority.DecodeStrictLimit(raw, &m, MaxManifestBytes); e != nil {
		return m, e
	}
	var fields map[string]json.RawMessage
	if e := json.Unmarshal(raw, &fields); e != nil {
		return m, e
	}
	for _, key := range []string{"schema_version", "integrity", "tenant", "site", "run_id", "frame", "started_at", "ended_at", "recorded_at", "completed_at", "records"} {
		b, ok := fields[key]
		if !ok || bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
			return m, fmt.Errorf("missing or null recording field %s", key)
		}
	}
	var records []json.RawMessage
	if e := json.Unmarshal(fields["records"], &records); e != nil {
		return m, e
	}
	for i, raw := range records {
		c, e := DecodeOriginal(raw)
		if e != nil {
			return m, e
		}
		m.Records[i] = c
	}
	return m, nil
}

func validateManifest(m *Manifest, derive bool) (map[string]sensing.EvidenceRef, error) {
	if m.SchemaVersion != Schema || m.Integrity != Integrity || m.Tenant == "" || m.Site == "" || m.RunID == "" || m.Frame.Validate() != nil || len(m.Records) == 0 || len(m.Records) > MaxRecords || m.RecordedAt.IsZero() || m.CompletedAt.Before(m.RecordedAt) || m.CompletedAt.Sub(m.RecordedAt) > time.Hour {
		return nil, errors.New("invalid recording scope, count or polling window")
	}
	refs := map[string]sensing.EvidenceRef{}
	seen := map[string]bool{}
	sources := map[string]string{}
	var earliest, latest time.Time
	packageHash, scenario := "", ""
	var seed uint64
	for _, c := range m.Records {
		s, e := ValidateOriginal(c)
		if e != nil {
			return nil, e
		}
		if c.Key.Tenant != m.Tenant || c.Key.Site != m.Site || c.Frame != m.Frame || s.RunID != m.RunID {
			return nil, errors.New("recording contains another scope")
		}
		key := identity(c)
		if seen[key] {
			return nil, errors.New("duplicate recording source/version")
		}
		seen[key] = true
		src := sourceID(c)
		if prev, ok := sources[s.SensorID]; ok && prev != src {
			return nil, errors.New("ambiguous source for recorded sensor")
		}
		sources[s.SensorID] = src
		if packageHash == "" {
			packageHash, scenario, seed = s.PackageSHA256, s.ScenarioVersion, s.Seed
		} else if packageHash != s.PackageSHA256 || scenario != s.ScenarioVersion || seed != s.Seed {
			return nil, errors.New("mixed recording simulation lineage")
		}
		start := captureStart(c, s)
		if !m.RecordedAt.Before(start.Add(c.TTL - c.Timing.ErrorBound)) {
			return nil, errors.New("recording polling began after original observation expiry")
		}
		if s.Camera != nil && !m.RecordedAt.Before(c.Timing.SampleTime.Add(time.Duration(s.Camera.CalibrationValidForNS)-c.Timing.ErrorBound)) {
			return nil, errors.New("recording polling began after calibration expiry")
		}
		if earliest.IsZero() || start.Before(earliest) {
			earliest = start
		}
		if latest.IsZero() || c.Timing.SampleTime.After(latest) {
			latest = c.Timing.SampleTime
		}
		if s.Evidence != nil {
			ref := *s.Evidence
			if old, ok := refs[ref.SHA256]; ok && old != ref {
				return nil, errors.New("conflicting media metadata")
			}
			refs[ref.SHA256] = ref
		}
	}
	if latest.Sub(earliest) > 24*time.Hour {
		return nil, errors.New("recording capture span too large")
	}
	if latest.After(m.CompletedAt) {
		return nil, errors.New("recording contains a capture after polling completed")
	}
	if derive {
		m.StartedAt = earliest
		m.EndedAt = latest
	} else if !m.StartedAt.Equal(earliest) || !m.EndedAt.Equal(latest) {
		return nil, errors.New("recording capture bounds differ from its records")
	}
	// Canonical ordering is part of the format and makes one captured history stable.
	less := func(a, b world.Component) bool {
		if !a.Timing.SampleTime.Equal(b.Timing.SampleTime) {
			return a.Timing.SampleTime.Before(b.Timing.SampleTime)
		}
		if sourceID(a) != sourceID(b) {
			return sourceID(a) < sourceID(b)
		}
		return a.Version.Compare(b.Version) < 0
	}
	if derive {
		sort.Slice(m.Records, func(i, j int) bool { return less(m.Records[i], m.Records[j]) })
	} else if !sort.SliceIsSorted(m.Records, func(i, j int) bool { return less(m.Records[i], m.Records[j]) }) {
		return nil, errors.New("recording records are not ordered")
	}
	previous := map[string]world.Component{}
	for _, c := range m.Records {
		src := sourceID(c)
		if old, ok := previous[src]; ok && c.Version.Compare(old.Version) <= 0 {
			return nil, errors.New("source versions regress along capture timeline")
		}
		previous[src] = c
	}
	return refs, nil
}

// Build freezes caller-owned records and media. Capture bounds are derived from
// the original samples, including the complete audio interval.
func Build(m Manifest, blobs map[string][]byte) (Bundle, error) {
	if m.SchemaVersion == "" {
		m.SchemaVersion = Schema
	}
	if m.Integrity == "" {
		m.Integrity = Integrity
	}
	if len(m.Records) == 0 || len(m.Records) > MaxRecords {
		return Bundle{}, errors.New("recording count outside bound")
	}
	// Check individual encodings before allocating the whole history encoding.
	header := m
	header.Records = nil
	base, e := json.Marshal(header)
	if e != nil {
		return Bundle{}, e
	}
	metadataBytes := len(base)
	for _, c := range m.Records {
		encoded, err := json.Marshal(c)
		if err != nil {
			return Bundle{}, err
		}
		if len(encoded) > replay.MaxEnvelopeBytes {
			return Bundle{}, errors.New("recorded component too large")
		}
		metadataBytes += len(encoded) + 1
		if metadataBytes > MaxManifestBytes {
			return Bundle{}, errors.New("recording manifest too large")
		}
	}
	raw, e := json.Marshal(m)
	if e != nil {
		return Bundle{}, e
	}
	if len(raw) > MaxManifestBytes {
		return Bundle{}, errors.New("recording manifest too large")
	}
	var copyM Manifest
	if e = authority.DecodeStrictLimit(raw, &copyM, MaxManifestBytes); e != nil {
		return Bundle{}, e
	}
	if _, e = validateManifest(&copyM, true); e != nil {
		return Bundle{}, e
	}
	raw, e = json.Marshal(copyM)
	if e != nil {
		return Bundle{}, e
	}
	return Verify(raw, blobs)
}

// Verify validates exact manifest bytes and all required blobs. Extra blobs are
// refused, and copies detach the result from the caller's mutable inputs.
func Verify(raw []byte, blobs map[string][]byte) (Bundle, error) {
	m, e := decodeManifest(raw)
	if e != nil {
		return Bundle{}, e
	}
	refs, e := validateManifest(&m, false)
	if e != nil {
		return Bundle{}, e
	}
	if len(refs) != len(blobs) {
		return Bundle{}, errors.New("recording media inventory mismatch")
	}
	copied := make(map[string][]byte, len(blobs))
	total := int64(len(raw))
	for hash, ref := range refs {
		data, ok := blobs[hash]
		if !ok || int64(len(data)) != ref.Size || Digest(data) != hash {
			return Bundle{}, errors.New("recording media digest or size mismatch")
		}
		total += int64(len(data))
		if total > MaxTotalBytes {
			return Bundle{}, errors.New("recording exceeds byte quota")
		}
		copied[hash] = append([]byte(nil), data...)
	}
	return Bundle{Manifest: m, Root: append([]byte(nil), raw...), SHA256: Digest(raw), Blobs: copied}, nil
}

func readFile(root *os.Root, name string, max int64) ([]byte, error) {
	st, e := root.Lstat(name)
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("recording file must be regular")
	}
	f, e := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	st, e = f.Stat()
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() || st.Size() > max {
		return nil, errors.New("recording file type or size invalid")
	}
	b, e := io.ReadAll(io.LimitReader(f, max+1))
	if e != nil {
		return nil, e
	}
	if int64(len(b)) > max {
		return nil, errors.New("recording file too large")
	}
	return b, nil
}
func directoryNames(root *os.Root) ([]string, error) {
	f, e := root.Open(".")
	if e != nil {
		return nil, e
	}
	defer f.Close()
	names, e := f.Readdirnames(MaxRecords + 3)
	if e != nil && e != io.EOF {
		return nil, e
	}
	return names, nil
}

// Load takes a directory containing recording.json and digest-named blobs.
// Every path is descriptor-relative; symlink and nonregular entries are refused.
func Load(directory string) (Bundle, error) {
	root, e := os.OpenRoot(directory)
	if e != nil {
		return Bundle{}, e
	}
	defer root.Close()
	names, e := directoryNames(root)
	if e != nil {
		return Bundle{}, e
	}
	if len(names) != 2 {
		return Bundle{}, errors.New("recording directory must contain only manifest and blobs")
	}
	for _, n := range names {
		if n != "recording.json" && n != "blobs" {
			return Bundle{}, errors.New("unexpected recording entry")
		}
	}
	raw, e := readFile(root, "recording.json", MaxManifestBytes)
	if e != nil {
		return Bundle{}, e
	}
	m, e := decodeManifest(raw)
	if e != nil {
		return Bundle{}, e
	}
	refs, e := validateManifest(&m, false)
	if e != nil {
		return Bundle{}, e
	}
	st, e := root.Lstat("blobs")
	if e != nil {
		return Bundle{}, e
	}
	if !st.IsDir() {
		return Bundle{}, errors.New("recording blobs must be a directory")
	}
	blobRoot, e := root.OpenRoot("blobs")
	if e != nil {
		return Bundle{}, e
	}
	defer blobRoot.Close()
	names, e = directoryNames(blobRoot)
	if e != nil {
		return Bundle{}, e
	}
	if len(names) != len(refs) {
		return Bundle{}, errors.New("unexpected recording blobs")
	}
	blobs := map[string][]byte{}
	total := int64(len(raw))
	for _, name := range names {
		ref, ok := refs[name]
		if !ok {
			return Bundle{}, errors.New("unreferenced recording blob")
		}
		total += ref.Size
		if total > MaxTotalBytes {
			return Bundle{}, errors.New("recording exceeds byte quota")
		}
		data, e := readFile(blobRoot, name, ref.Size)
		if e != nil {
			return Bundle{}, fmt.Errorf("blob %s: %w", name, e)
		}
		blobs[name] = data
	}
	return Verify(raw, blobs)
}

// SameRecord ignores only the relay-age hint when comparing duplicate versions.
// A recorder retains the first received hint and does not renew capture time.
func SameRecord(a, b world.Component) bool {
	a.Timing.AgeHint = 0
	b.Timing.AgeHint = 0
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
