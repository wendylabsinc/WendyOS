package recording

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func fixture(t *testing.T) (Manifest, map[string][]byte) {
	t.Helper()
	at := time.Unix(1700000000, 0).UTC()
	blob := []byte("retained-media-fixture")
	sample := sensing.Sample{SchemaVersion: sensing.Schema, ID: "run/audio-1/1", SiteID: "office", Frame: world.FrameRef{ID: "floor", Revision: "v1"}, SensorID: "audio-1", Kind: "audio", Sequence: 1, SampleNS: int64(time.Second), RunID: "run", ScenarioVersion: "v1", Seed: 1, PackageSHA256: strings.Repeat("a", 64), SensorModel: "audio", PositionM: []float64{0, 0, 0}, UncertaintyM: .1, CalibrationRevision: "v1", Audio: &sensing.Audio{SampleRateHz: 8000, Channels: 1, DurationNS: int64(200 * time.Millisecond)}, Evidence: &sensing.EvidenceRef{SHA256: Digest(blob), Size: int64(len(blob)), ContentType: "audio/wav"}}
	conv, e := sensing.New(sensing.Config{Tenant: "tenant", Site: "office", RunID: "run", Frame: sample.Frame, Source: world.Source{Device: "capture-node", App: "sensor", Service: "audio-1", Stream: "samples"}, Epoch: 3, Anchor: at, TTL: 5 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	data, _ := json.Marshal(sample)
	c, e := conv.Convert(data)
	if e != nil {
		t.Fatal(e)
	}
	c.Timing.AgeHint = 50 * time.Millisecond
	return Manifest{Tenant: "tenant", Site: "office", RunID: "run", Frame: sample.Frame, RecordedAt: at.Add(time.Second), CompletedAt: at.Add(2 * time.Second), Records: []world.Component{c}}, map[string][]byte{Digest(blob): blob}
}
func TestArchiveRoundTripPreservesCaptureAndFrozenBytes(t *testing.T) {
	m, blobs := fixture(t)
	b, e := Build(m, blobs)
	if e != nil {
		t.Fatal(e)
	}
	if !b.Manifest.StartedAt.Equal(m.Records[0].Timing.SampleTime.Add(-200*time.Millisecond)) || !b.Manifest.EndedAt.Equal(m.Records[0].Timing.SampleTime) {
		t.Fatal("audio capture bounds lost")
	}
	path := filepath.Join(t.TempDir(), "archive")
	if e = Write(b, path); e != nil {
		t.Fatal(e)
	}
	restored, e := Load(path)
	if e != nil {
		t.Fatal(e)
	}
	if restored.SHA256 != b.SHA256 || string(restored.Root) != string(b.Root) || !SameRecord(restored.Manifest.Records[0], m.Records[0]) || restored.Manifest.Records[0].Timing.AgeHint != 50*time.Millisecond {
		t.Fatal("roundtrip changed capture")
	}
	for name := range blobs {
		blobs[name][0] = 'x'
		if b.Blobs[name][0] == 'x' {
			t.Fatal("blob not copied")
		}
	}
	m.Records[0].Payload[0] = 'x'
	if b.Manifest.Records[0].Payload[0] == 'x' {
		t.Fatal("payload not copied")
	}
	if e = Write(b, path); e == nil {
		t.Fatal("overwrote installed recording")
	}
}
func TestMalformedArchivesFailClosed(t *testing.T) {
	for _, which := range []string{"duplicate", "scope", "clock", "lineage", "unknown-error", "ttl", "missing-blob", "corrupt-blob", "extra-blob"} {
		t.Run(which, func(t *testing.T) {
			m, blobs := fixture(t)
			c := &m.Records[0]
			switch which {
			case "duplicate":
				m.Records = append(m.Records, *c)
			case "scope":
				m.Site = "other"
			case "clock":
				c.Timing.ClockDomain = "physical"
			case "lineage":
				c.Provenance.Origin = world.Measured
			case "unknown-error":
				c.Timing.ErrorKnown = false
			case "ttl":
				c.TTL = 25 * time.Hour
			case "missing-blob":
				clear(blobs)
			case "corrupt-blob":
				for k := range blobs {
					blobs[k][0] = 'x'
				}
			case "extra-blob":
				blobs[Digest([]byte("extra"))] = []byte("extra")
			}
			if _, e := Build(m, blobs); e == nil {
				t.Fatal("accepted malformed archive")
			}
		})
	}
	m, blobs := fixture(t)
	b, e := Build(m, blobs)
	if e != nil {
		t.Fatal(e)
	}
	for _, raw := range [][]byte{append(b.Root, []byte(" {}")...), []byte(strings.Replace(string(b.Root), `"integrity":`, `"unknown":1,"integrity":`, 1)), []byte(strings.Replace(string(b.Root), `"tenant":"tenant"`, `"tenant":"tenant","tenant":"tenant"`, 1)), []byte(strings.Replace(string(b.Root), `"started_at":"2023-11-14T22:13:20.8Z"`, `"started_at":"2023-11-14T22:13:20Z"`, 1))} {
		if _, e = Verify(raw, b.Blobs); e == nil {
			t.Fatal("accepted malformed manifest")
		}
	}
}
func TestLoadRejectsUnexpectedEntriesLinksAndFIFO(t *testing.T) {
	for _, which := range []string{"extra-root", "extra-blob", "manifest-link", "blob-link", "blob-fifo", "blob-directory", "blobs-link"} {
		t.Run(which, func(t *testing.T) {
			m, blobs := fixture(t)
			b, e := Build(m, blobs)
			if e != nil {
				t.Fatal(e)
			}
			root := t.TempDir()
			path := filepath.Join(root, "archive")
			if e = Write(b, path); e != nil {
				t.Fatal(e)
			}
			hash := ""
			for h := range blobs {
				hash = h
			}
			blob := filepath.Join(path, "blobs", hash)
			switch which {
			case "extra-root":
				e = os.WriteFile(filepath.Join(path, "extra"), nil, 0600)
			case "extra-blob":
				e = os.WriteFile(filepath.Join(path, "blobs", "extra"), nil, 0600)
			case "manifest-link":
				e = os.Rename(filepath.Join(path, "recording.json"), filepath.Join(root, "root.json"))
				if e == nil {
					e = os.Symlink(filepath.Join(root, "root.json"), filepath.Join(path, "recording.json"))
				}
			case "blob-link":
				e = os.Rename(blob, filepath.Join(root, "blob"))
				if e == nil {
					e = os.Symlink(filepath.Join(root, "blob"), blob)
				}
			case "blob-fifo":
				e = os.Remove(blob)
				if e == nil {
					e = syscall.Mkfifo(blob, 0600)
				}
			case "blob-directory":
				e = os.Remove(blob)
				if e == nil {
					e = os.Mkdir(blob, 0700)
				}
			case "blobs-link":
				e = os.Rename(filepath.Join(path, "blobs"), filepath.Join(root, "moved"))
				if e == nil {
					e = os.Symlink(filepath.Join(root, "moved"), filepath.Join(path, "blobs"))
				}
			}
			if e != nil {
				t.Fatal(e)
			}
			if _, e = Load(path); e == nil {
				t.Fatal("accepted unexpected file topology")
			}
		})
	}
}
func TestConcurrentInstallNeverOverwrites(t *testing.T) {
	m, blobs := fixture(t)
	b, e := Build(m, blobs)
	if e != nil {
		t.Fatal(e)
	}
	dest := filepath.Join(t.TempDir(), "archive")
	results := make(chan error, 2)
	go func() { results <- Write(b, dest) }()
	results <- Write(b, dest)
	success := 0
	for range 2 {
		e := <-results
		if e == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatal(success)
	}
	if _, e = Load(dest); e != nil {
		t.Fatal(e)
	}
}
func TestDestinationAppearingAtRenameIsPreserved(t *testing.T) {
	m, blobs := fixture(t)
	b, e := Build(m, blobs)
	if e != nil {
		t.Fatal(e)
	}
	dest := filepath.Join(t.TempDir(), "archive")
	e = write(b, dest, func(fd int, from, to string) error {
		if err := os.Mkdir(dest, 0700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dest, "marker"), []byte("existing"), 0600); err != nil {
			return err
		}
		return renameNoReplace(fd, from, to)
	})
	if e == nil {
		t.Fatal("replaced raced destination")
	}
	raw, e := os.ReadFile(filepath.Join(dest, "marker"))
	if e != nil || string(raw) != "existing" {
		t.Fatal("destination lost", e)
	}
}
func TestCaptureOrderAndDuplicateHints(t *testing.T) {
	m, blobs := fixture(t)
	c := m.Records[0]
	c.Version.Sequence = 2
	c.Timing.SampleTime = c.Timing.SampleTime.Add(time.Second)
	s, e := sensing.Decode(c.Payload)
	if e != nil {
		t.Fatal(e)
	}
	s.Sequence = 2
	s.ID = "run/audio-1/2"
	s.SampleNS += int64(time.Second)
	c.Payload, _ = json.Marshal(s)
	m.Records = []world.Component{c, m.Records[0]}
	b, e := Build(m, blobs)
	if e != nil {
		t.Fatal(e)
	}
	if b.Manifest.Records[0].Version.Sequence != 1 {
		t.Fatal("not sorted")
	}
	other := m.Records[0]
	other.Timing.AgeHint++
	if !SameRecord(m.Records[0], other) {
		t.Fatal("relay hint became conflict")
	}
	other.Timing.SampleTime = other.Timing.SampleTime.Add(time.Millisecond)
	if SameRecord(m.Records[0], other) {
		t.Fatal("capture mutation accepted")
	}
}

func TestMissingOrNullOriginalMetadataIsRejected(t *testing.T) {
	m, blobs := fixture(t)
	b, err := Build(m, blobs)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"error_bound_ns":0`, `"age_hint_ns":50000000`, `"error_known":true`, `"tombstone":false`, `"seed":1`} {
		for _, mode := range []string{"omit", "null"} {
			t.Run(field+mode, func(t *testing.T) {
				modified := string(b.Root)
				if mode == "omit" {
					modified = strings.Replace(modified, field+",", "", 1)
					if modified == string(b.Root) {
						modified = strings.Replace(modified, ","+field, "", 1)
					}
				} else {
					key := strings.SplitN(field, ":", 2)[0]
					modified = strings.Replace(modified, field, key+":null", 1)
				}
				if modified == string(b.Root) {
					t.Fatal("fixture did not change")
				}
				if _, err := Verify([]byte(modified), b.Blobs); err == nil {
					t.Fatal("accepted missing/null original metadata")
				}
			})
		}
	}
}

func TestCaptureMustHaveBeenPossibleDuringPolling(t *testing.T) {
	for _, which := range []string{"completed_before_capture", "poll_after_expiry", "audio_hint_expired"} {
		t.Run(which, func(t *testing.T) {
			m, blobs := fixture(t)
			switch which {
			case "completed_before_capture":
				m.RecordedAt = m.RecordedAt.Add(-time.Second)
				m.CompletedAt = m.Records[0].Timing.SampleTime.Add(-time.Millisecond)
			case "poll_after_expiry":
				m.RecordedAt = m.RecordedAt.Add(10 * time.Second)
				m.CompletedAt = m.RecordedAt.Add(time.Second)
			case "audio_hint_expired":
				m.Records[0].Timing.AgeHint = m.Records[0].TTL - 100*time.Millisecond
			}
			if _, err := Build(m, blobs); err == nil {
				t.Fatal("accepted impossible polling history")
			}
		})
	}
}
