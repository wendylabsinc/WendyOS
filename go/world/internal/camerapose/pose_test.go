package camerapose

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/stationarycamera"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type testSource struct {
	mu      sync.Mutex
	data    []byte
	session string
	fresh   bool
	seq     uint64
	width   int
	now     func() time.Time
}

func (s *testSource) State() stationarycamera.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	n := s.now()
	width := s.width
	if width == 0 {
		width = 64
	}
	h := sha256.Sum256(s.data)
	return stationarycamera.State{Connection: "streaming", Fresh: s.fresh, Latest: &stationarycamera.Receipt{ID: time.Unix(int64(s.seq), 0).Format(time.RFC3339), SHA256: hex.EncodeToString(h[:]), Width: width, Height: 48, Size: int64(len(s.data)), ContentType: "image/jpeg", Representation: "original_jpeg", SourceCodec: "mjpeg", ReceivedAt: n, RetainedUntil: n.Add(time.Hour), SourceSession: s.session, Sequence: s.seq, Origin: world.Simulated}}
}
func (s *testSource) Evidence(string) ([]byte, error) { return append([]byte{}, s.data...), nil }
func fixture(t *testing.T) (Config, *testSource) {
	t.Helper()
	dir := t.TempDir()
	model := []byte("synthetic test model identity")
	h := sha256.Sum256(model)
	write := func(name string, v any) {
		b, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(dir, name), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	write("site.json", map[string]any{"id": "test-site", "name": "Simulated room", "modelRevision": "revision-one"})
	write("visual-map.json", map[string]any{"version": 1, "modelRevision": "revision-one", "modelSHA256": hex.EncodeToString(h[:]), "capturedAt": 800000000, "frameCount": 4, "landmarks": make([]byte, 120*131*4)})
	if err := os.WriteFile(filepath.Join(dir, "model.usdz"), model, 0600); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewGray(image.Rect(0, 0, 64, 48)), nil); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	if err := os.Chmod(work, 0700); err != nil {
		t.Fatal(err)
	}
	return Config{Helper: "/usr/bin/true", SiteDir: dir, WorkDir: work, pairDelay: time.Millisecond}, &testSource{data: b.Bytes(), session: "session-one", fresh: true, now: time.Now}
}
func result() Result {
	return Result{Schema: "wendy.site-localization.result.v1", Success: true, Matches: 30, Inliers: 20, FittedLens: 1, FX: 50, FY: 50, CX: 32, CY: 24, RMS: .2, WorldFromCamera: [16]float64{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 1, 2, 3, 1}}
}
func await(t *testing.T, c *Controller) State {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s := c.Snapshot()
		if s.Status != "estimating" {
			return s
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("pose job did not finish")
	return State{}
}
func TestReferenceMapBoundToExactModelAndRevision(t *testing.T) {
	for _, name := range []string{"model", "revision", "landmark"} {
		t.Run(name, func(t *testing.T) {
			cfg, s := fixture(t)
			if name == "model" {
				os.WriteFile(filepath.Join(cfg.SiteDir, "model.usdz"), []byte("different bytes"), 0600)
			} else {
				path := filepath.Join(cfg.SiteDir, "visual-map.json")
				b, _ := os.ReadFile(path)
				var m map[string]any
				json.Unmarshal(b, &m)
				if name == "revision" {
					m["modelRevision"] = "other"
				} else {
					m["landmarks"] = []byte{0, 0, 0, 0}
				}
				b, _ = json.Marshal(m)
				os.WriteFile(path, b, 0600)
			}
			if _, err := New(cfg, s); err == nil {
				t.Fatal("unbound map accepted")
			}
		})
	}
}
func TestPoseKeepsEvidenceAndInvalidatesAfterDisconnect(t *testing.T) {
	cfg, s := fixture(t)
	var modes []string
	cfg.solve = func(_ context.Context, landmarks, img, mode string, lens *Result) (Result, error) {
		modes = append(modes, mode)
		if b, e := os.ReadFile(img); e != nil || !bytes.HasPrefix(b, []byte("P5\n64 48\n255\n")) {
			t.Error("missing grayscale image")
		}
		r := result()
		if lens != nil {
			r.FittedLens = 0
		}
		return r, nil
	}
	c, err := New(cfg, s)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := await(t, c)
	if out.Status != "located" || !out.Fresh || out.Pose == nil || out.Pose.Lens.Quality != "scene_fit" || out.InputOrigin != world.Simulated || len(out.Evidence) != 2 || out.Provenance != "inferred" {
		t.Fatalf("bad inference: %+v", out)
	}
	if len(modes) != 2 || modes[0] != "joint" || modes[1] != "measured" {
		t.Fatal(modes)
	}
	if entries, _ := os.ReadDir(cfg.WorkDir); len(entries) != 1 || entries[0].Name() != "last-calibration.json" {
		t.Fatal("temporary image files retained")
	}
	s.mu.Lock()
	s.fresh = false
	s.mu.Unlock()
	out = c.Snapshot()
	if out.Pose != nil || out.Fresh || out.Status != "unavailable" {
		t.Fatal("disconnected pose stayed valid")
	}
	s.mu.Lock()
	s.fresh = true
	s.mu.Unlock()
	if c.Snapshot().Pose != nil {
		t.Fatal("reconnect revived an old pose")
	}
}
func TestAssumedLensVerificationCannotUpgradeEstimate(t *testing.T) {
	cfg, s := fixture(t)
	var modes []string
	cfg.solve = func(_ context.Context, _, _, mode string, _ *Result) (Result, error) {
		modes = append(modes, mode)
		r := result()
		if mode == "joint" {
			r.Success = false
			r.Reason = 7
		}
		if mode == "estimated" {
			r.FittedLens = 2
			r.Inliers = 8
		}
		if mode == "assumed" {
			r.FittedLens = 0
			r.Inliers = 8
		}
		return r, nil
	}
	c, e := New(cfg, s)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	c.Start(context.Background())
	out := await(t, c)
	if out.Status != "located" || out.Pose.Lens.Quality != "approximate" || out.Pose.Inliers != 8 || len(modes) != 3 || modes[2] != "assumed" {
		t.Fatalf("assumed lens changed status: %+v %v", out, modes)
	}
}
func TestTwoFrameDisagreementNeverPublishesPose(t *testing.T) {
	for _, approx := range []bool{false, true} {
		cfg, s := fixture(t)
		calls := 0
		cfg.solve = func(_ context.Context, _, _, _ string, _ *Result) (Result, error) {
			calls++
			r := result()
			if approx {
				r.FittedLens = 2
			}
			if calls == 2 {
				r.WorldFromCamera[12] += .5
			}
			return r, nil
		}
		c, e := New(cfg, s)
		if e != nil {
			t.Fatal(e)
		}
		c.Start(context.Background())
		out := await(t, c)
		c.Close()
		if out.Status != "no_match" || out.Pose != nil || out.Fresh {
			t.Fatalf("unstable pose published %+v", out)
		}
	}
}
func TestSessionChangeDuringSolveRejectsResult(t *testing.T) {
	cfg, s := fixture(t)
	cfg.solve = func(_ context.Context, _, _, _ string, _ *Result) (Result, error) {
		s.mu.Lock()
		s.session = "replacement-session"
		s.mu.Unlock()
		return result(), nil
	}
	c, e := New(cfg, s)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	c.Start(context.Background())
	out := await(t, c)
	if out.Status != "unavailable" || out.Pose != nil {
		t.Fatal("old session pose published")
	}
}
func TestRejectedFitHasNoTransform(t *testing.T) {
	cfg, s := fixture(t)
	cfg.solve = func(_ context.Context, _, _, _ string, _ *Result) (Result, error) {
		r := result()
		r.Success = false
		r.Reason = 8
		return r, nil
	}
	c, e := New(cfg, s)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	c.Start(context.Background())
	out := await(t, c)
	b, _ := json.Marshal(out)
	if out.Status != "no_match" || bytes.Contains(b, []byte("world_from_camera")) {
		t.Fatal("rejected fit exposed a transform")
	}
}
func TestExpiryAndMapChangeInvalidatePose(t *testing.T) {
	for _, name := range []string{"expiry", "map"} {
		t.Run(name, func(t *testing.T) {
			cfg, s := fixture(t)
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			now := func() time.Time { return time.Unix(0, clock.Add(int64(time.Microsecond))) }
			cfg.Now = now
			s.now = now
			cfg.pairDelay = time.Microsecond
			cfg.solve = func(context.Context, string, string, string, *Result) (Result, error) { return result(), nil }
			c, e := New(cfg, s)
			if e != nil {
				t.Fatal(e)
			}
			defer c.Close()
			c.Start(context.Background())
			out := await(t, c)
			if out.Pose == nil {
				t.Fatalf("expected pose: %+v", out)
			}
			if name == "expiry" {
				clock.Add(int64(8 * 24 * time.Hour))
			} else {
				os.WriteFile(filepath.Join(cfg.SiteDir, "site.json"), []byte("changed"), 0600)
			}
			if c.Snapshot().Pose != nil {
				t.Fatal("expired/map-changed pose remained available")
			}
		})
	}
}
func TestCloseCancelsJobAndStartDoesNotOverlap(t *testing.T) {
	cfg, s := fixture(t)
	entered := make(chan struct{})
	cfg.solve = func(ctx context.Context, _, _, _ string, _ *Result) (Result, error) {
		close(entered)
		<-ctx.Done()
		return Result{}, ctx.Err()
	}
	c, e := New(cfg, s)
	if e != nil {
		t.Fatal(e)
	}
	c.Start(context.Background())
	<-entered
	if c.Start(context.Background()) != ErrBusy {
		t.Fatal("overlapping job accepted")
	}
	c.Close()
	if c.Snapshot().Pose != nil || c.Snapshot().Status != "unavailable" {
		t.Fatal("closed estimator published pose")
	}
}

func TestMalformedGeometryIsRejected(t *testing.T) {
	for name, change := range map[string]func(*Result){
		"reflection":         func(r *Result) { r.WorldFromCamera[0] = -1 },
		"projective":         func(r *Result) { r.WorldFromCamera[3] = .2 },
		"nonorthogonal":      func(r *Result) { r.WorldFromCamera[4] = .4 },
		"unbounded position": func(r *Result) { r.WorldFromCamera[12] = 1001 },
		"missing lens":       func(r *Result) { r.FX = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			r := result()
			change(&r)
			if validResult(r, 64, 48) {
				t.Fatal("invalid geometry accepted")
			}
		})
	}
}
func TestDisabledControllerNeverStarts(t *testing.T) {
	_, s := fixture(t)
	c, e := New(Config{}, s)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	if c.Start(context.Background()) != ErrDisabled || c.Snapshot().Pose != nil {
		t.Fatal("unconfigured estimator started")
	}
}

func TestAcceptedCalibrationHistoryDoesNotRestoreCurrentPose(t *testing.T) {
	cfg, source := fixture(t)
	cfg.solve = func(context.Context, string, string, string, *Result) (Result, error) { return result(), nil }
	c, err := New(cfg, source)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !await(t, c).Fresh {
		t.Fatal("fixture pose missing")
	}
	c.Close()
	data, err := os.ReadFile(filepath.Join(cfg.WorkDir, "last-calibration.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved State
	if json.Unmarshal(data, &saved) != nil || saved.Pose == nil || len(saved.Evidence) != 2 {
		t.Fatal("accepted calibration evidence missing")
	}
	next, err := New(cfg, source)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if state := next.Snapshot(); state.Fresh || state.Pose != nil || state.Status != "idle" {
		t.Fatal("restart trusted old calibration")
	}
}

func TestPoseInvalidatedByCameraFormatChange(t *testing.T) {
	cfg, source := fixture(t)
	cfg.solve = func(context.Context, string, string, string, *Result) (Result, error) { return result(), nil }
	c, err := New(cfg, source)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !await(t, c).Fresh {
		t.Fatal("fixture pose unavailable")
	}
	source.mu.Lock()
	source.width = 128
	source.mu.Unlock()
	if state := c.Snapshot(); state.Fresh || state.Pose != nil {
		t.Fatal("new image resolution retained stale lens")
	}
}

func TestCalibrationHistoryFailureRemainsExplicit(t *testing.T) {
	cfg, source := fixture(t)
	cfg.solve = func(context.Context, string, string, string, *Result) (Result, error) { return result(), nil }
	if err := os.Mkdir(filepath.Join(cfg.WorkDir, "last-calibration.json"), 0700); err != nil {
		t.Fatal(err)
	}
	c, err := New(cfg, source)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := await(t, c)
	if !state.Fresh || state.Pose == nil || state.HistorySaved {
		t.Fatal("history write failure was hidden or invalidated geometric fit")
	}
}
