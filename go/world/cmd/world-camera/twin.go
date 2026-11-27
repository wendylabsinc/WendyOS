package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/camerapose"
	"github.com/wendylabsinc/wendy/go/world/internal/camerastream"
	"github.com/wendylabsinc/wendy/go/world/internal/cameratwin"
	"github.com/wendylabsinc/wendy/go/world/internal/stationarycamera"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type cameraConfig struct {
	ID                 string                         `json:"id"`
	Name               string                         `json:"name"`
	Device             string                         `json:"device"`
	CameraID           uint32                         `json:"camera_id"`
	EmulatedSource     string                         `json:"emulated_source,omitempty"`
	Identity           string                         `json:"identity,omitempty"`
	CalibrationProfile string                         `json:"calibration_profile,omitempty"`
	CalibrationBinding *camerapose.CalibrationBinding `json:"calibration_binding,omitempty"`
}
type twinCamera struct {
	config    cameraConfig
	store     *stationarycamera.Store
	pose      *camerapose.Controller
	capture   *captureControl
	mu        sync.Mutex
	automatic bool
	next      time.Time
	failures  int
	pending   bool
}
type twinSite struct {
	camerapose.Site
	MetresPerModelUnit float64 `json:"metres_per_model_unit"`
}
type twinService struct {
	cameras      []*twinCamera
	site         twinSite
	model        []byte
	observations *cameratwin.Store
	ctx          context.Context
	token        string
	cancel       context.CancelFunc
	done         chan struct{}
}
type twinCameraState struct {
	ID                string                 `json:"id"`
	Name              string                 `json:"name"`
	IdentityStatus    string                 `json:"identity_status"`
	State             stationarycamera.State `json:"state"`
	Pose              camerapose.State       `json:"pose"`
	Automatic         bool                   `json:"automatic_alignment"`
	PublicationStatus string                 `json:"publication_status"`
}
type twinState struct {
	Schema         string                   `json:"schema"`
	Site           twinSite                 `json:"site"`
	ModelAvailable bool                     `json:"model_available"`
	Cameras        []twinCameraState        `json:"cameras"`
	Observations   []cameratwin.Observation `json:"observations"`
}

var cameraKey = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func readCameras(path string) ([]cameraConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var configs []cameraConfig
	d := json.NewDecoder(io.LimitReader(f, 64<<10))
	d.DisallowUnknownFields()
	if err = d.Decode(&configs); err != nil {
		return nil, err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, errors.New("camera config has trailing data")
	}
	if err = validateCameras(configs); err != nil {
		return nil, err
	}
	return configs, nil
}
func validateCameras(configs []cameraConfig) error {
	if len(configs) == 0 || len(configs) > 16 {
		return errors.New("configure 1 to 16 cameras")
	}
	ids := map[string]bool{}
	sources := map[string]bool{}
	for _, c := range configs {
		if c.EmulatedSource != "" && (!filepath.IsAbs(c.EmulatedSource) || strings.ContainsAny(c.EmulatedSource, "\x00\r\n")) {
			return errors.New("emulated camera source must be an absolute executable path")
		}
		if (c.CalibrationProfile == "") != (c.CalibrationBinding == nil) || (c.CalibrationProfile != "" && !filepath.IsAbs(c.CalibrationProfile)) {
			return errors.New("camera calibration requires an absolute profile path and an explicit camera binding")
		}
		source := fmt.Sprintf("%s:%d", c.Device, c.CameraID)
		if !cameraKey.MatchString(c.ID) || ids[c.ID] || sources[source] || c.Name == "" || len(c.Name) > 256 || !strings.HasPrefix(c.Device, "wendy-lite:") || len(c.Device) <= 11 || len(c.Device) > 512 || strings.ContainsAny(c.Device+c.Name+c.Identity, "\x00\r\n") || len(c.Identity) > 256 {
			return errors.New("invalid, duplicate or unsupported camera configuration")
		}
		ids[c.ID] = true
		sources[source] = true
	}
	return nil
}
func loadTwinSite(dir string) (twinSite, []byte, error) {
	if dir == "" {
		return twinSite{}, nil, nil
	}
	var raw struct {
		ID, Name, ModelRevision string
		MetresPerModelUnit      *float64 `json:"metresPerModelUnit"`
	}
	b, err := readRegular(filepath.Join(dir, "site.json"), 1<<20)
	if err != nil {
		return twinSite{}, nil, err
	}
	if json.Unmarshal(b, &raw) != nil || raw.ID == "" || raw.ModelRevision == "" || len(raw.ID) > 128 || len(raw.ModelRevision) > 128 || len(raw.Name) > 256 {
		return twinSite{}, nil, errors.New("invalid site metadata")
	}
	scale := 1.0
	if raw.MetresPerModelUnit != nil {
		scale = *raw.MetresPerModelUnit
	}
	if math.IsNaN(scale) || math.IsInf(scale, 0) || scale <= 0 || scale > 1000 {
		return twinSite{}, nil, errors.New("invalid site scale")
	}
	model, err := readRegular(filepath.Join(dir, "model.usdz"), 256<<20)
	if err != nil {
		return twinSite{}, nil, err
	}
	hash := sha256.Sum256(model)
	return twinSite{Site: camerapose.Site{ID: raw.ID, Name: raw.Name, ModelRevision: raw.ModelRevision, ModelSHA256: hex.EncodeToString(hash[:])}, MetresPerModelUnit: scale}, model, nil
}
func readRegular(path string, limit int64) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return nil, errors.New("site input must be a bounded regular file")
	}
	return os.ReadFile(path)
}
func newTwin(ctx context.Context, configs []cameraConfig, dir, wendy, siteDir, helper, token string, legacy bool, simulationRuns ...string) (*twinService, error) {
	if len(simulationRuns) > 1 {
		return nil, errors.New("only one simulation run can bind a camera service")
	}
	simulationRun := ""
	origin := world.Measured
	if len(simulationRuns) == 1 && simulationRuns[0] != "" {
		simulationRun, origin = simulationRuns[0], world.Simulated
	}
	if err := validateCameras(configs); err != nil {
		return nil, err
	}
	for _, cfg := range configs {
		if cfg.EmulatedSource != "" && simulationRun == "" {
			return nil, errors.New("emulated camera source requires --simulation-run")
		}
	}
	site, model, err := loadTwinSite(siteDir)
	if err != nil {
		return nil, err
	}
	child, cancel := context.WithCancel(ctx)
	t := &twinService{ctx: child, cancel: cancel, token: token, site: site, model: model}
	ok := false
	defer func() {
		if !ok {
			t.close()
		}
	}()
	for _, cfg := range configs {
		cameraDir := filepath.Join(dir, "cameras", cfg.ID)
		if legacy {
			cameraDir = dir
		}
		s, err := stationarycamera.Open(stationarycamera.Config{Dir: cameraDir, Tenant: "local-camera-demo", Site: "stationary-camera", Device: cfg.Device, Camera: fmt.Sprint(cfg.CameraID), SimulationRun: simulationRun})
		if err != nil {
			return nil, err
		}
		poseSite := siteDir
		if _, err := os.Stat(filepath.Join(siteDir, "visual-map.json")); err != nil {
			poseSite = ""
		}
		p, err := camerapose.New(camerapose.Config{Helper: helper, SiteDir: poseSite, WorkDir: filepath.Join(cameraDir, "localization"), CalibrationProfilePath: cfg.CalibrationProfile, CalibrationBinding: cfg.CalibrationBinding}, s)
		if err != nil {
			s.Close()
			return nil, err
		}
		c := &twinCamera{config: cfg, store: s, pose: p, automatic: poseSite != "" && helper != "", capture: &captureControl{parent: child, store: s, origin: origin, config: camerastream.Config{WendyBinary: wendy, EmulatedSource: cfg.EmulatedSource, SimulationRun: simulationRun, Device: cfg.Device, CameraID: cfg.CameraID}}}
		t.cameras = append(t.cameras, c)
	}
	if site.ID != "" {
		t.observations, err = cameratwin.Open(filepath.Join(dir, "twin"), site.Site, func(id string) (cameratwin.Camera, bool) {
			c := t.camera(id)
			if c == nil {
				return cameratwin.Camera{}, false
			}
			return cameratwin.Camera{State: c.store.State(), Pose: c.pose.Snapshot()}, true
		}, nil)
		if err != nil {
			return nil, err
		}
	}
	ok = true
	return t, nil
}
func (t *twinService) camera(id string) *twinCamera {
	if id == "" && len(t.cameras) > 0 {
		return t.cameras[0]
	}
	for _, c := range t.cameras {
		if c.config.ID == id {
			return c
		}
	}
	return nil
}
func (t *twinService) close() {
	if t.cancel != nil {
		t.cancel()
	}
	if t.done != nil {
		<-t.done
	}
	for _, c := range t.cameras {
		c.capture.pause()
		c.pose.Close()
		c.store.Close()
	}
	if t.observations != nil {
		t.observations.Close()
	}
}
func (t *twinService) start() {
	for _, c := range t.cameras {
		c.capture.resume()
	}
	t.done = make(chan struct{})
	go func() { defer close(t.done); t.automaticLoop() }()
}
func (t *twinService) automaticLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-t.ctx.Done():
			return
		case now := <-ticker.C:
			busy := false
			for _, c := range t.cameras {
				state := c.pose.Snapshot()
				if t.observations != nil {
					// Retry failed persistence on the next tick. The HTTP snapshot
					// reports publication_status independently of a valid local fit.
					_ = t.observations.PublishPose(c.config.ID)
				}
				c.mu.Lock()
				if state.Status == "estimating" {
					busy = true
					c.pending = true
				} else if c.pending {
					c.pending = false
					if state.Fresh {
						c.failures = 0
						c.next = now.Add(time.Minute)
					} else {
						c.failures++
						c.next = now.Add(retryDelay(c.failures))
					}
				}
				c.mu.Unlock()
			}
			if busy {
				continue
			}
			for _, c := range t.cameras {
				c.mu.Lock()
				if !c.automatic || now.Before(c.next) || !c.store.State().Fresh {
					c.mu.Unlock()
					continue
				}
				err := c.pose.Start(t.ctx)
				if err == nil {
					c.pending = true
				} else {
					c.failures++
					c.next = now.Add(retryDelay(c.failures))
				}
				c.mu.Unlock()
				break
			}
		}
	}
}
func retryDelay(failures int) time.Duration {
	return time.Duration(min(300, 30<<(min(max(failures-1, 0), 4)))) * time.Second
}
func (t *twinService) snapshot() twinState {
	out := twinState{Schema: "wendy.camera-twin.state.v1", Site: t.site, ModelAvailable: len(t.model) > 0, Cameras: []twinCameraState{}, Observations: []cameratwin.Observation{}}
	for _, c := range t.cameras {
		c.mu.Lock()
		automatic := c.automatic
		c.mu.Unlock()
		pose := c.pose.Snapshot()
		status := "disabled"
		if t.observations != nil {
			if err := t.observations.PublishPose(c.config.ID); err != nil {
				status = "failed"
			} else if pose.Fresh && pose.Pose != nil {
				status = "published"
			} else {
				status = "no_current_pose"
			}
		}
		out.Cameras = append(out.Cameras, twinCameraState{c.config.ID, c.config.Name, "configured", c.store.State(), pose, automatic, status})
	}
	if t.observations != nil {
		out.Observations = t.observations.Snapshot().Observations
	}
	return out
}
func (t *twinService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+t.token)) != 1 {
		http.Error(w, "unauthorized", 401)
		return
	}
	write := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
	if r.Method == "GET" && r.URL.Path == "/v1/twin" {
		write(t.snapshot())
		return
	}
	if r.Method == "GET" && r.URL.Path == "/v1/site/model" {
		if len(t.model) == 0 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.usdz+zip")
		w.Header().Set("X-Content-SHA256", t.site.ModelSHA256)
		w.Header().Set("Content-Length", fmt.Sprint(len(t.model)))
		_, _ = w.Write(t.model)
		return
	}
	if r.URL.Path == "/v1/twin/observations" && t.observations != nil {
		if r.Method == "GET" {
			write(t.observations.Snapshot())
			return
		}
		if r.Method == "POST" {
			var b cameratwin.Batch
			if decodeBody(w, r, &b, 128<<10) != nil {
				http.Error(w, "invalid observations", 400)
				return
			}
			if err := t.observations.Publish(b); err != nil {
				http.Error(w, err.Error(), 409)
				return
			}
			write(t.observations.Snapshot())
			return
		}
	}
	c := t.camera(r.URL.Query().Get("camera"))
	if c == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method == "POST" && r.URL.Path == "/v1/pose/automatic" {
		var b struct {
			Enabled *bool `json:"enabled"`
		}
		if decodeBody(w, r, &b, 1024) != nil || b.Enabled == nil {
			http.Error(w, "enabled boolean required", 400)
			return
		}
		c.mu.Lock()
		c.automatic = *b.Enabled
		c.next = time.Time{}
		c.mu.Unlock()
		if !*b.Enabled {
			c.pose.Cancel()
		}
		write(map[string]bool{"enabled": *b.Enabled})
		return
	}
	handler(c.store, t.token, func(action string) error {
		if action == "pause" {
			c.capture.pause()
			c.pose.Cancel()
		} else {
			c.capture.resume()
			c.mu.Lock()
			c.next = time.Time{}
			c.mu.Unlock()
		}
		return nil
	}, poseEndpoint{controller: c.pose, context: t.ctx}).ServeHTTP(w, r)
}
func decodeBody(w http.ResponseWriter, r *http.Request, out any, limit int64) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("trailing input")
	}
	return nil
}
