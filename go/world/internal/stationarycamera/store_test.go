package stationarycamera

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func testJPEG(t *testing.T) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, 32, 24))
	for y := 0; y < 24; y++ {
		for x := 0; x < 32; x++ {
			im.Set(x, y, color.RGBA{uint8(x * 7), uint8(y * 9), 71, 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, im, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func fixture(t *testing.T) (*Store, *time.Time, Config) {
	t.Helper()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	cfg := Config{Dir: t.TempDir(), Tenant: "private-local", Site: "stationary-demo", Device: "wendy-camera", Camera: "wall-camera", Now: func() time.Time { return now }}
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, &now, cfg
}

func TestFreshReceiptExpiresWithoutInventingExposureTime(t *testing.T) {
	s, now, _ := fixture(t)
	if got := s.State(); got.Fresh || got.Latest != nil || got.Freshness != "waiting" {
		t.Fatalf("startup: %+v", got)
	}
	data := testJPEG(t)
	if err := s.AcceptJPEG(data, "stream-1", 1, world.Measured); err != nil {
		t.Fatal(err)
	}
	got := s.State()
	if !got.Fresh || got.Freshness != "receiving" || got.Latest.Origin != world.Measured || got.Latest.ExposureTimeKnown || got.Latest.CalibrationStatus != "unknown" || got.Latest.Width != 32 || got.Latest.Height != 24 {
		t.Fatalf("receipt: %+v", got)
	}
	retained, err := s.Evidence(got.Latest.SHA256)
	if err != nil || !bytes.Equal(retained, data) {
		t.Fatalf("evidence %v", err)
	}
	*now = now.Add(FreshFor)
	if got := s.State(); got.Fresh || got.Freshness != "stale" || got.Latest == nil {
		t.Fatalf("expiry: %+v", got)
	}
	if _, err = s.Evidence(got.Latest.SHA256); err != nil {
		t.Fatalf("receipt expiry removed retained evidence: %v", err)
	}
	records, err := s.world.Records()
	if err != nil || len(records) != 1 {
		t.Fatalf("records %v", err)
	}
	if records[0].Provenance.Synthetic() || records[0].Timing.ClockDomain == "utc" {
		t.Fatal("invented synthetic ancestry or UTC clock")
	}
}

func TestDuplicateReorderedAndRetiredStreamsCannotRefresh(t *testing.T) {
	s, now, _ := fixture(t)
	data := testJPEG(t)
	if err := s.AcceptJPEG(data, "stream-1", 3, world.Measured); err != nil {
		t.Fatal(err)
	}
	first := *s.State().Latest
	*now = now.Add(2 * time.Second)
	for _, seq := range []uint64{0, 2, 3} {
		if err := s.AcceptJPEG(data, "stream-1", seq, world.Measured); !errors.Is(err, ErrSequence) {
			t.Fatalf("seq %d: %v", seq, err)
		}
	}
	*now = now.Add(time.Second)
	if s.State().Fresh {
		t.Fatal("rejected duplicate refreshed deadline")
	}
	s.SetConnection("disconnected", "camera cable removed")
	if err := s.AcceptJPEG(data, "stream-1", 4, world.Measured); !errors.Is(err, ErrSequence) {
		t.Fatalf("failed source revived: %v", err)
	}
	s.SetConnection("connecting", "")
	if err := s.AcceptJPEG(data, "stream-2", 1, world.Measured); err != nil {
		t.Fatal(err)
	}
	got := s.State()
	if !got.Fresh || len(got.Recent) != 2 || got.Recent[1] != first || !got.Latest.RetainedUntil.Equal(first.RetainedUntil) {
		t.Fatal("receipt or immutable retention changed")
	}
	if err := s.AcceptJPEG(data, "stream-1", 5, world.Measured); !errors.Is(err, ErrSequence) {
		t.Fatalf("retired stream reentered: %v", err)
	}
}

func TestRestartRetainsEvidenceButNeverClaimsConnection(t *testing.T) {
	s, now, cfg := fixture(t)
	data := testJPEG(t)
	if err := s.AcceptJPEG(data, "before-restart", 1, world.Measured); err != nil {
		t.Fatal(err)
	}
	first := *s.State().Latest
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Second)
	reopened, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got := reopened.State()
	if got.Fresh || got.Connection != "connecting" || got.Latest == nil || *got.Latest != first {
		t.Fatalf("restart: %+v", got)
	}
	if b, err := reopened.Evidence(first.SHA256); err != nil || !bytes.Equal(data, b) {
		t.Fatalf("evidence after restart %v", err)
	}
	if err := reopened.AcceptJPEG(data, "before-restart", 2, world.Measured); !errors.Is(err, ErrSequence) {
		t.Fatalf("reused pre-crash source: %v", err)
	}
	if err := reopened.AcceptJPEG(data, "after-restart", 1, world.Measured); err != nil {
		t.Fatal(err)
	}
	if reopened.State().Latest.ID == first.ID || !reopened.State().Fresh {
		t.Fatal("new publisher epoch missing")
	}
}

func TestCorruptOrMissingImagesNeverPublish(t *testing.T) {
	s, _, cfg := fixture(t)
	data := testJPEG(t)
	for _, bad := range [][]byte{nil, {}, data[:len(data)/2], append(append([]byte{}, data...), 0)} {
		if err := s.AcceptJPEG(bad, "stream-1", 1, world.Measured); !errors.Is(err, ErrFrame) {
			t.Fatalf("bad frame %v", err)
		}
		if s.State().Latest != nil {
			t.Fatal("invalid input published")
		}
	}
	if err := s.AcceptJPEG(data, "stream-1", 1, world.Measured); err != nil {
		t.Fatal(err)
	}
	digest := s.State().Latest.SHA256
	path := filepath.Join(cfg.Dir, "evidence", digest+".blob")
	corrupt := append([]byte{}, data...)
	corrupt[len(corrupt)/2] ^= 0xff
	if err := os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Evidence(digest); err == nil {
		t.Fatal("corrupted digest served")
	}
	if _, err := s.Evidence("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); !errors.Is(err, evidence.ErrUnavailable) {
		t.Fatalf("unreferenced digest: %v", err)
	}
}

func TestEvidenceOutlivesRecentPresentationWindow(t *testing.T) {
	s, now, cfg := fixture(t)
	firstBytes := testJPEG(t)
	if err := s.AcceptJPEG(firstBytes, "stream", 1, world.Measured); err != nil {
		t.Fatal(err)
	}
	first := *s.State().Latest
	for sequence := uint64(2); sequence <= MaxRecent+3; sequence++ {
		im := image.NewGray(image.Rect(0, 0, 8, 8))
		for i := range im.Pix {
			im.Pix[i] = uint8(sequence)
		}
		var encoded bytes.Buffer
		if err := jpeg.Encode(&encoded, im, nil); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(500 * time.Millisecond)
		if err := s.AcceptJPEG(encoded.Bytes(), "stream", sequence, world.Measured); err != nil {
			t.Fatal(err)
		}
	}
	for _, receipt := range s.State().Recent {
		if receipt.ID == first.ID {
			t.Fatal("first receipt still in recent window")
		}
	}
	if got, err := s.Evidence(first.SHA256); err != nil || !bytes.Equal(got, firstBytes) {
		t.Fatalf("retained image lost after presentation rollover: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, err := reopened.Evidence(first.SHA256); err != nil || !bytes.Equal(got, firstBytes) {
		t.Fatalf("older evidence lost after reopen: %v", err)
	}
}

func TestExplicitFixtureOriginAndBoundedHistory(t *testing.T) {
	s, now, _ := fixture(t)
	data := testJPEG(t)
	if err := s.AcceptJPEG(data, "fixture", 1, world.Replayed); err == nil {
		t.Fatal("replay accepted without replay lineage")
	}
	if err := s.AcceptJPEG(data, "fixture", 1, world.Simulated); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptJPEG(data, "fixture", 2, world.Measured); err == nil {
		t.Fatal("simulation changed to measured")
	}
	for seq := uint64(2); seq < MaxRecent+12; seq++ {
		*now = now.Add(time.Millisecond)
		if err := s.AcceptJPEG(data, "fixture", seq, world.Simulated); err != nil {
			t.Fatal(err)
		}
	}
	got := s.State()
	if len(got.Recent) != MaxRecent || got.Latest.Origin != world.Simulated {
		t.Fatal("unbounded history or lost origin")
	}
	records, err := s.world.Records()
	if err != nil || len(records) != 1 || !records[0].Provenance.Synthetic() {
		t.Fatalf("fixture component %v", err)
	}
	encoded, err := json.Marshal(got)
	if err != nil || !bytes.Contains(encoded, []byte(`"exposure_time_known":false`)) || !bytes.Contains(encoded, []byte(`"calibration_status":"unknown"`)) {
		t.Fatal("unknown metadata lost")
	}
	*now = now.Add(Retention)
	if got := s.State(); got.Fresh || len(got.Recent) != 0 {
		t.Fatal("retention expiry ignored")
	}
	if _, err := s.Evidence(got.Latest.SHA256); !errors.Is(err, evidence.ErrUnavailable) {
		t.Fatalf("expired evidence %v", err)
	}
}

func TestFailureAndStopImmediatelyRemoveFreshness(t *testing.T) {
	for _, status := range []string{"disconnected", "error", "stopped", "connecting"} {
		t.Run(status, func(t *testing.T) {
			s, _, _ := fixture(t)
			if err := s.AcceptJPEG(testJPEG(t), "stream", 1, world.Measured); err != nil {
				t.Fatal(err)
			}
			s.SetConnection(status, "reason")
			if got := s.State(); got.Fresh || got.Connection != status || got.Detail != "reason" {
				t.Fatalf("%+v", got)
			}
		})
	}
}
