// Package camerapose adapts Wendy's markerless localization solver to retained
// camera receipts. A pose is an inference in a scan-local frame, not geography.
package camerapose

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/stationarycamera"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

var ErrBusy = errors.New("camera position estimation is already running")
var ErrDisabled = errors.New("camera position estimation is not configured")

type Config struct {
	Helper, SiteDir, WorkDir string
	CalibrationProfilePath   string
	CalibrationBinding       *CalibrationBinding
	// Timeout bounds the complete job, including both frames and fallback solves.
	Timeout   time.Duration
	Now       func() time.Time
	pairDelay time.Duration
	solve     func(context.Context, string, string, string, *Result) (Result, error)
}
type Source interface {
	State() stationarycamera.State
	Evidence(string) ([]byte, error)
}
type Site struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ModelRevision string `json:"model_revision"`
	ModelSHA256   string `json:"model_sha256"`
}
type Lens struct {
	Quality    string                  `json:"quality"`
	FX         float64                 `json:"fx"`
	FY         float64                 `json:"fy"`
	CX         float64                 `json:"cx"`
	CY         float64                 `json:"cy"`
	Distortion [5]float64              `json:"distortion"`
	Profile    *CalibrationProfileInfo `json:"profile,omitempty"`
}
type Pose struct {
	ImageWidth      int         `json:"image_width"`
	ImageHeight     int         `json:"image_height"`
	WorldFromCamera [16]float64 `json:"world_from_camera"`
	Position        [3]float64  `json:"position"`
	Lens            Lens        `json:"lens"`
	Matches         int         `json:"matches"`
	Inliers         int         `json:"inliers"`
	RMSPixels       float64     `json:"rms_pixels"`
	ObservedAt      time.Time   `json:"observed_at"`
	ValidUntil      time.Time   `json:"valid_until"`
}
type Evidence struct {
	ReceiptID     string       `json:"receipt_id"`
	SHA256        string       `json:"sha256"`
	SourceSession string       `json:"source_session"`
	Sequence      uint64       `json:"sequence"`
	ReceivedAt    time.Time    `json:"received_at"`
	Origin        world.Origin `json:"origin"`
}
type State struct {
	Schema                string                  `json:"schema"`
	Status                string                  `json:"status"`
	Detail                string                  `json:"detail"`
	Fresh                 bool                    `json:"fresh"`
	HistorySaved          bool                    `json:"history_saved"`
	Provenance            string                  `json:"provenance"`
	InputOrigin           world.Origin            `json:"input_origin,omitempty"`
	SourceSession         string                  `json:"source_session,omitempty"`
	Site                  Site                    `json:"site"`
	CoordinateFrame       string                  `json:"coordinate_frame"`
	Units                 string                  `json:"units"`
	GeographicAnchorKnown bool                    `json:"geographic_anchor_known"`
	UncertaintyKnown      bool                    `json:"uncertainty_known"`
	Calibration           *CalibrationProfileInfo `json:"calibration,omitempty"`
	Pose                  *Pose                   `json:"pose,omitempty"`
	Evidence              []Evidence              `json:"evidence"`
}

// Result is the bounded command bridge response from the existing Wendy solver.
type Result struct {
	Schema          string      `json:"schema"`
	Success         bool        `json:"success"`
	Reason          int         `json:"reason"`
	Matches         int         `json:"matches"`
	Inliers         int         `json:"inliers"`
	FittedLens      int         `json:"fitted_lens"`
	FX              float64     `json:"fx"`
	FY              float64     `json:"fy"`
	CX              float64     `json:"cx"`
	CY              float64     `json:"cy"`
	Distortion      [5]float64  `json:"distortion"`
	RMS             float64     `json:"rms"`
	WorldFromCamera [16]float64 `json:"world_from_camera"`
}
type reference struct {
	site       Site
	landmarks  []byte
	mapDigest  [32]byte
	signatures [3]string
}
type Controller struct {
	mu          sync.Mutex
	cfg         Config
	source      Source
	ref         reference
	calibration *calibrationReference
	state       State
	cancel      context.CancelFunc
	done        chan struct{}
	closed      bool
}

// New loads and validates a map bound to the exact USDZ model. Omitted optional
// configuration produces an explicit disabled state rather than a fake pose.
func New(cfg Config, source Source) (*Controller, error) {
	if source == nil {
		return nil, errors.New("camera pose requires a source")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 90 * time.Second
	}
	if cfg.Timeout < time.Second || cfg.Timeout > 3*time.Minute {
		return nil, errors.New("invalid localization timeout")
	}
	if cfg.pairDelay == 0 {
		cfg.pairDelay = 800 * time.Millisecond
	}
	c := &Controller{cfg: cfg, source: source, state: State{Schema: "wendy.camera-pose.state.v1", Status: "disabled", Detail: "Choose a scanned site with visual references to estimate camera position.", Provenance: "inferred", Units: "m", Evidence: []Evidence{}}}
	calibration, err := loadCalibration(cfg.CalibrationProfilePath, cfg.CalibrationBinding)
	if err != nil {
		return nil, err
	}
	c.calibration = calibration
	// Own the binding instead of retaining caller-mutable configuration.
	if calibration != nil {
		binding := calibration.binding
		c.cfg.CalibrationBinding = &binding
		c.state.Calibration = cloneCalibrationInfo(&calibration.info)
	}
	if cfg.Helper == "" || cfg.SiteDir == "" {
		return c, nil
	}
	helper, err := exec.LookPath(cfg.Helper)
	if err != nil {
		return nil, errors.New("localization helper is unavailable")
	}
	c.cfg.Helper = helper
	if cfg.WorkDir == "" {
		return nil, errors.New("private localization work directory is required")
	}
	if err = os.MkdirAll(cfg.WorkDir, 0700); err != nil {
		return nil, err
	}
	st, err := os.Lstat(cfg.WorkDir)
	if err != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return nil, errors.New("localization work directory must be private and not a symlink")
	}
	ref, err := loadReference(cfg.SiteDir)
	if err != nil {
		return nil, err
	}
	c.ref = ref
	c.state.Site = ref.site
	c.state.CoordinateFrame = "wendy-site:" + ref.site.ID + ":" + ref.site.ModelRevision + ":scan-local-y-up"
	c.state.Status = "idle"
	c.state.Detail = "Ready to match two camera images against this scan's visual references."
	return c, nil
}

// Start begins a bounded asynchronous job. Snapshot remains safe while it runs.
func (c *Controller) Start(parent context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("camera position estimator is closed")
	}
	if c.state.Status == "disabled" {
		return ErrDisabled
	}
	if c.cancel != nil {
		return ErrBusy
	}
	if err := parent.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, c.cfg.Timeout)
	c.cancel = cancel
	c.done = make(chan struct{})
	c.state.Status = "estimating"
	c.state.Detail = "Matching two fresh images to the scanned room."
	c.state.Fresh = false
	c.state.Pose = nil
	c.state.Evidence = []Evidence{}
	c.state.InputOrigin = ""
	c.state.SourceSession = ""
	go func() {
		s := c.estimate(ctx)
		c.mu.Lock()
		defer c.mu.Unlock()
		if !c.closed {
			if s.Fresh && s.Pose != nil {
				s.HistorySaved = c.saveHistory(s)
			}
			c.state = s
		}
		cancel()
		c.cancel = nil
		close(c.done)
	}()
	return nil
}

// Cancel joins an in-flight solve without closing the reusable controller.
func (c *Controller) Cancel() {
	c.mu.Lock()
	cancel, done := c.cancel, c.done
	c.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// saveHistory retains the last accepted calibration as evidence only. New
// controllers deliberately never load it as current: a fresh source session
// must be verified again after every restart.
func (c *Controller) saveHistory(s State) bool {
	s.HistorySaved = true
	if c.cfg.WorkDir == "" {
		return false
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return false
	}
	f, err := os.CreateTemp(c.cfg.WorkDir, ".calibration-*")
	if err != nil {
		return false
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		if os.Rename(name, filepath.Join(c.cfg.WorkDir, "last-calibration.json")) != nil {
			return false
		}
		dir, err := os.Open(c.cfg.WorkDir)
		if err != nil {
			return false
		}
		defer dir.Close()
		return dir.Sync() == nil
	}
	return false
}
func (c *Controller) Close() {
	c.mu.Lock()
	c.closed = true
	cancel, done := c.cancel, c.done
	c.state.Fresh = false
	c.state.Pose = nil
	c.state.Status = "unavailable"
	c.state.Detail = "Position estimator stopped."
	c.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}
func (c *Controller) Snapshot() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.Pose != nil {
		s := c.source.State()
		if !freshReceipt(s, c.cfg.Now()) || s.Latest.SourceSession != c.state.SourceSession || s.Latest.Origin != c.state.InputOrigin || s.Latest.Width != c.state.Pose.ImageWidth || s.Latest.Height != c.state.Pose.ImageHeight || s.Latest.SourceCodec != "mjpeg" || !c.cfg.Now().Before(c.state.Pose.ValidUntil) || !c.referencesUnchanged() || !c.calibrationUnchanged() {
			c.state.Status = "unavailable"
			c.state.Detail = "Camera, scan or calibration changed, or the pose expired. Estimate its position again."
			c.state.Fresh = false
			c.state.Pose = nil
		}
	}
	out := c.state
	out.Evidence = append([]Evidence{}, out.Evidence...)
	out.Calibration = cloneCalibrationInfo(out.Calibration)
	if out.Pose != nil {
		p := *out.Pose
		p.Lens.Profile = cloneCalibrationInfo(p.Lens.Profile)
		out.Pose = &p
	}
	return out
}
func (c *Controller) referencesUnchanged() bool {
	for i, name := range []string{"site.json", "visual-map.json", "model.usdz"} {
		if signature(filepath.Join(c.cfg.SiteDir, name)) != c.ref.signatures[i] {
			return false
		}
	}
	return true
}
func freshReceipt(s stationarycamera.State, now time.Time) bool {
	r := s.Latest
	return s.Fresh && s.Connection == "streaming" && r != nil && r.SourceSession != "" && r.ID != "" && (r.Origin == world.Measured || r.Origin == world.Simulated) && !now.Before(r.ReceivedAt) && now.Sub(r.ReceivedAt) < stationarycamera.FreshFor && now.Before(r.RetainedUntil)
}
func evidence(r stationarycamera.Receipt) Evidence {
	return Evidence{r.ID, r.SHA256, r.SourceSession, r.Sequence, r.ReceivedAt, r.Origin}
}
func (c *Controller) estimate(ctx context.Context) State {
	c.mu.Lock()
	out := c.state
	c.mu.Unlock()
	fail := func(status, detail string) State {
		out.Status = status
		out.Detail = detail
		out.Fresh = false
		out.Pose = nil
		return out
	}
	if !c.referencesUnchanged() {
		return fail("unavailable", "The scanned site changed. Reopen the camera with the current scan.")
	}
	if !c.calibrationUnchanged() {
		return fail("unavailable", "The camera calibration changed. Reopen the camera with the current profile.")
	}
	s := c.source.State()
	if !freshReceipt(s, c.cfg.Now()) {
		return fail("unavailable", "A fresh camera image is needed to estimate position.")
	}
	first := *s.Latest
	if !c.calibrationMatchesReceipt(first) {
		return fail("unavailable", "The calibration does not match this image size or its measured/simulated origin.")
	}
	out.InputOrigin = first.Origin
	out.SourceSession = first.SourceSession
	out.Evidence = []Evidence{evidence(first)}
	firstBytes, err := c.readImage(first)
	if err != nil {
		return fail("unavailable", "The original camera image is unavailable or failed verification.")
	}
	var second stationarycamera.Receipt
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	waiting := true
	for waiting {
		select {
		case <-ctx.Done():
			return fail("unavailable", "Position estimation was cancelled or timed out.")
		case <-deadline.C:
			return fail("unavailable", "A second fresh camera image is needed to verify position.")
		case <-poll.C:
			next := c.source.State()
			if !freshReceipt(next, c.cfg.Now()) || next.Latest.SourceSession != first.SourceSession || next.Latest.Origin != first.Origin {
				return fail("unavailable", "The camera connection changed during position estimation.")
			}
			r := *next.Latest
			if r.Width != first.Width || r.Height != first.Height || r.SourceCodec != first.SourceCodec {
				return fail("unavailable", "The camera image format changed during position estimation.")
			}
			if r.Sequence > first.Sequence && r.ID != first.ID && r.ReceivedAt.Sub(first.ReceivedAt) >= c.cfg.pairDelay {
				second = r
				waiting = false
			}
		}
	}
	secondBytes, err := c.readImage(second)
	if err != nil {
		return fail("unavailable", "The second original image failed verification.")
	}
	out.Evidence = append(out.Evidence, evidence(second))
	dir, err := os.MkdirTemp(c.cfg.WorkDir, "pose-")
	if err != nil {
		return fail("unavailable", "Private position estimation storage is unavailable.")
	}
	defer os.RemoveAll(dir)
	landmarks := filepath.Join(dir, "landmarks.bin")
	if os.WriteFile(landmarks, c.ref.landmarks, 0600) != nil {
		return fail("unavailable", "Private position estimation storage is unavailable.")
	}
	paths := []string{filepath.Join(dir, "first.pgm"), filepath.Join(dir, "second.pgm")}
	for i, data := range [][]byte{firstBytes, secondBytes} {
		if err = writePGM(paths[i], data); err != nil {
			return fail("unavailable", "A camera image could not be decoded.")
		}
	}
	solve := c.cfg.solve
	if solve == nil {
		solve = c.solve
	}
	var result Result
	mode, quality := "measured", "scene_fit"
	if c.calibration != nil {
		mode, quality = c.calibrationMode()
		lens := c.calibration.lens
		result, err = solve(ctx, landmarks, paths[0], mode, &lens)
		if err == nil && result.Success && (result.FittedLens != 0 || !sameProfileLens(result, lens)) {
			return fail("no_match", "The position estimator did not preserve the selected calibration.")
		}
		if quality == "model_calibration" {
			result.FittedLens = 2
		}
	} else {
		result, err = solve(ctx, landmarks, paths[0], "joint", nil)
		if err == nil && !result.Success {
			result, err = solve(ctx, landmarks, paths[0], "estimated", nil)
		}
		if result.FittedLens == 2 {
			mode, quality = "assumed", "approximate"
		} else if result.FittedLens == 0 {
			quality = "measured"
		}
	}
	if err != nil {
		return fail("unavailable", "The position estimator could not process this image.")
	}
	if !result.Success {
		return fail("no_match", failureDetail(result))
	}
	if !validResult(result, first.Width, first.Height) {
		return fail("no_match", "The estimated camera geometry failed validation. No position was saved.")
	}
	verified, err := solve(ctx, landmarks, paths[1], mode, &result)
	if err != nil {
		return fail("unavailable", "The second image could not be checked.")
	}
	if c.calibration != nil && verified.Success && verified.FittedLens != 0 {
		return fail("no_match", "The second image did not preserve the selected calibration.")
	}
	// The native assumed-lens verification reports fitted_lens=0 because it
	// held the supplied lens fixed. It does not turn that assumption into a measurement.
	if quality == "approximate" || quality == "model_calibration" {
		verified.FittedLens = 2
	}
	if c.calibration != nil && verified.Success && !sameProfileLens(verified, c.calibration.lens) {
		return fail("no_match", "The second image did not preserve the selected calibration.")
	}
	if !verified.Success || !validResult(verified, second.Width, second.Height) {
		return fail("no_match", "The second image did not confirm the estimated position.")
	}
	if !consistent(result, verified) {
		return fail("no_match", "The estimated position changed between images. No position was saved.")
	}
	current := c.source.State()
	if ctx.Err() != nil || !freshReceipt(current, c.cfg.Now()) || current.Latest.SourceSession != first.SourceSession || current.Latest.Origin != first.Origin || current.Latest.Width != first.Width || current.Latest.Height != first.Height || !c.referencesUnchanged() || !c.calibrationUnchanged() {
		return fail("unavailable", "The camera or scanned site changed before position estimation finished.")
	}
	now := c.cfg.Now()
	until := now.Add(7 * 24 * time.Hour)
	// The receipt provenance and source-session dependency remain in State.
	out.Pose = &Pose{ImageWidth: first.Width, ImageHeight: first.Height, WorldFromCamera: verified.WorldFromCamera, Position: [3]float64{verified.WorldFromCamera[12], verified.WorldFromCamera[13], verified.WorldFromCamera[14]}, Lens: Lens{Quality: quality, FX: result.FX, FY: result.FY, CX: result.CX, CY: result.CY, Distortion: result.Distortion, Profile: cloneCalibrationInfo(out.Calibration)}, Matches: min(result.Matches, verified.Matches), Inliers: min(result.Inliers, verified.Inliers), RMSPixels: max(result.RMS, verified.RMS), ObservedAt: now.UTC(), ValidUntil: until.UTC()}
	out.Status = "located"
	out.Detail = "Position estimated in the scan's metric coordinate frame. Accuracy is not independently measured."
	out.Fresh = true
	return out
}
func (c *Controller) readImage(r stationarycamera.Receipt) ([]byte, error) {
	if r.Width < 32 || r.Height < 32 || r.Width > 8192 || r.Height > 8192 || int64(r.Width)*int64(r.Height) > 32_000_000 || r.ContentType != "image/jpeg" || r.Representation != "original_jpeg" || r.SourceCodec != "mjpeg" {
		return nil, errors.New("invalid image receipt")
	}
	b, err := c.source.Evidence(r.SHA256)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(b)
	if len(b) > stationarycamera.MaxJPEGBytes || int64(len(b)) != r.Size || hex.EncodeToString(digest[:]) != r.SHA256 {
		return nil, errors.New("invalid original image")
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(b))
	if err != nil || cfg.Width != r.Width || cfg.Height != r.Height {
		return nil, errors.New("invalid image dimensions")
	}
	return b, nil
}
func writePGM(path string, data []byte) error {
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return err
	}
	b := img.Bounds()
	var out bytes.Buffer
	fmt.Fprintf(&out, "P5\n%d %d\n255\n", b.Dx(), b.Dy())
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			out.WriteByte(color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y)
		}
	}
	return os.WriteFile(path, out.Bytes(), 0600)
}
func failureDetail(r Result) string {
	switch r.Reason {
	case 1, 2:
		return fmt.Sprintf("Found %d possible scene matches; %d agree on the camera geometry. Point the camera at textured surfaces included in the scan.", r.Matches, r.Inliers)
	case 3:
		return "Scene matches cover too little of the image to estimate position."
	case 7:
		return "The matched scene is too flat to resolve this camera geometry."
	case 8:
		return "Several camera positions or orientations fit this view. No position was saved. Include scanned features at different depths, or calibrate the camera lens."
	default:
		return "The camera view did not produce a stable match to this scan. No position was saved."
	}
}
func validResult(r Result, w, h int) bool {
	if r.Schema != "wendy.site-localization.result.v1" || !r.Success || r.FittedLens < 0 || r.FittedLens > 2 || r.Matches < r.Inliers || r.Matches > 12000 || r.Inliers < 8 || !finite(r.RMS) || r.RMS < 0 {
		return false
	}
	if r.FittedLens == 1 && r.Inliers < 16 || r.FittedLens == 0 && r.Inliers < 12 {
		return false
	}
	for _, v := range []float64{r.FX, r.FY, r.CX, r.CY} {
		if !finite(v) {
			return false
		}
	}
	if r.FX <= 0 || r.FY <= 0 || r.CX < 0 || r.CY < 0 || r.CX > float64(w) || r.CY > float64(h) {
		return false
	}
	for _, v := range r.Distortion {
		if !finite(v) {
			return false
		}
	}
	t := r.WorldFromCamera
	for _, v := range t {
		if !finite(v) {
			return false
		}
	}
	if math.Abs(t[3])+math.Abs(t[7])+math.Abs(t[11])+math.Abs(t[15]-1) > 1e-6 {
		return false
	}
	for i := 0; i < 3; i++ {
		if math.Abs(t[12+i]) > 1000 {
			return false
		}
		for j := 0; j < 3; j++ {
			dot := 0.
			for k := 0; k < 3; k++ {
				dot += t[i*4+k] * t[j*4+k]
			}
			want := 0.
			if i == j {
				want = 1
			}
			if math.Abs(dot-want) > 1e-4 {
				return false
			}
		}
	}
	det := t[0]*(t[5]*t[10]-t[9]*t[6]) - t[4]*(t[1]*t[10]-t[9]*t[2]) + t[8]*(t[1]*t[6]-t[5]*t[2])
	return math.Abs(det-1) < 1e-4
}
func consistent(a, b Result) bool {
	d := 0.
	for i := 12; i < 15; i++ {
		d += math.Pow(a.WorldFromCamera[i]-b.WorldFromCamera[i], 2)
	}
	trace := 0.
	for i := 0; i < 3; i++ {
		for k := 0; k < 3; k++ {
			trace += a.WorldFromCamera[i*4+k] * b.WorldFromCamera[i*4+k]
		}
	}
	angle := math.Acos(max(-1, min(1, (trace-1)/2)))
	distanceMax, angleMax := .08, 3*math.Pi/180
	if a.FittedLens == 2 {
		distanceMax, angleMax = .30, 10*math.Pi/180
	}
	return math.Sqrt(d) <= distanceMax && angle <= angleMax
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 64<<10 {
		return 0, errors.New("localization output exceeds bound")
	}
	return b.Buffer.Write(p)
}
func (c *Controller) solve(ctx context.Context, landmarks, image, mode string, lens *Result) (Result, error) {
	args := []string{"--landmarks", landmarks, "--image", image, "--mode", mode}
	if lens != nil {
		values := []float64{lens.FX, lens.FY, lens.CX, lens.CY}
		values = append(values, lens.Distortion[:]...)
		parts := make([]string, len(values))
		for i, v := range values {
			parts[i] = strconv.FormatFloat(v, 'g', 17, 64)
		}
		args = append(args, "--lens", strings.Join(parts, ","))
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.cfg.Helper, args...)
	var out boundedBuffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return Result{}, errors.New("localization command failed")
	}
	var result Result
	var payload json.RawMessage
	d := json.NewDecoder(bytes.NewReader(out.Bytes()))
	if err := d.Decode(&payload); err != nil {
		return result, errors.New("invalid localization response")
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		return result, errors.New("invalid localization response")
	}
	var shapes struct {
		Distortion []float64 `json:"distortion"`
		Transform  []float64 `json:"world_from_camera"`
	}
	if err := json.Unmarshal(payload, &shapes); err != nil || (result.Success && (len(shapes.Distortion) != 5 || len(shapes.Transform) != 16)) {
		return Result{}, errors.New("invalid localization result dimensions")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return result, errors.New("extra localization output")
	}
	if result.Schema != "wendy.site-localization.result.v1" || result.Matches < 0 || result.Matches > 12000 || result.Inliers < 0 || result.Inliers > result.Matches {
		return result, errors.New("invalid localization response")
	}
	return result, nil
}

func signature(path string) string {
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano())
}
func readBounded(path string, limit int64) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > limit {
		return nil, errors.New("invalid reference file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("reference file exceeds bound")
	}
	return data, nil
}
func loadReference(dir string) (reference, error) {
	var ref reference
	var before [3]string
	for i, name := range []string{"site.json", "visual-map.json", "model.usdz"} {
		before[i] = signature(filepath.Join(dir, name))
		if before[i] == "" {
			return ref, errors.New("reference files are unavailable")
		}
	}
	siteBytes, err := readBounded(filepath.Join(dir, "site.json"), 2<<20)
	if err != nil {
		return ref, errors.New("site metadata is unavailable")
	}
	var site struct{ ID, Name, ModelRevision string }
	if json.Unmarshal(siteBytes, &site) != nil || site.ID == "" || site.ModelRevision == "" || len(site.ID) > 128 || len(site.ModelRevision) > 128 || len(site.Name) > 256 {
		return ref, errors.New("invalid site metadata")
	}
	mapBytes, err := readBounded(filepath.Join(dir, "visual-map.json"), 12<<20)
	if err != nil {
		return ref, errors.New("visual references are unavailable")
	}
	var m struct {
		Version                    int
		ModelRevision, ModelSHA256 string
		CapturedAt                 float64
		FrameCount                 int
		Landmarks                  []byte
	}
	if json.Unmarshal(mapBytes, &m) != nil || m.Version != 1 || m.ModelRevision != site.ModelRevision || m.FrameCount < 1 || m.FrameCount > 200 || !finite(m.CapturedAt) || len(m.Landmarks)%(131*4) != 0 || len(m.Landmarks)/(131*4) < 120 || len(m.Landmarks)/(131*4) > 12000 {
		return ref, errors.New("invalid visual reference map")
	}
	for i := 0; i < len(m.Landmarks)/4; i++ {
		v := float64(math.Float32frombits(binary.LittleEndian.Uint32(m.Landmarks[i*4:])))
		if !finite(v) || i%131 < 3 && math.Abs(v) > 1000 || i%131 >= 3 && (v < 0 || v > 512) {
			return ref, errors.New("invalid visual landmark")
		}
	}
	modelPath := filepath.Join(dir, "model.usdz")
	st, err := os.Lstat(modelPath)
	if err != nil || !st.Mode().IsRegular() || st.Size() < 1 || st.Size() > 1<<30 {
		return ref, errors.New("invalid site model")
	}
	f, err := os.Open(modelPath)
	if err != nil {
		return ref, errors.New("site model is unavailable")
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(f, (1<<30)+1))
	f.Close()
	if err != nil || n != st.Size() || hex.EncodeToString(hash.Sum(nil)) != strings.ToLower(m.ModelSHA256) {
		return ref, errors.New("visual references do not match the site model")
	}
	ref.site = Site{site.ID, site.Name, site.ModelRevision, strings.ToLower(m.ModelSHA256)}
	ref.landmarks = m.Landmarks
	ref.mapDigest = sha256.Sum256(mapBytes)
	for i, name := range []string{"site.json", "visual-map.json", "model.usdz"} {
		ref.signatures[i] = signature(filepath.Join(dir, name))
		if ref.signatures[i] == "" || ref.signatures[i] != before[i] {
			return ref, errors.New("reference files changed")
		}
	}
	return ref, nil
}
