// world-camera connects one stationary Wendy Lite camera to a private local
// World receipt and evidence store. It has no robot or mission interface.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/camerapose"
	"github.com/wendylabsinc/wendy/go/world/internal/camerastream"
	"github.com/wendylabsinc/wendy/go/world/internal/stationarycamera"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type cameraStore interface {
	State() stationarycamera.State
	Evidence(string) ([]byte, error)
}

type captureControl struct {
	mu     sync.Mutex
	parent context.Context
	cancel context.CancelFunc
	done   chan struct{}
	store  *stationarycamera.Store
	config camerastream.Config
	origin world.Origin
}

func (c *captureControl) resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil || c.parent.Err() != nil {
		return
	}
	ctx, cancel := context.WithCancel(c.parent)
	c.cancel, c.done = cancel, make(chan struct{})
	done := c.done
	origin := c.origin
	if origin == "" {
		origin = world.Measured
	}
	go func() {
		defer close(done)
		err := camerastream.Run(ctx, c.config, func(data []byte, session string, seq uint64) error {
			return c.store.AcceptJPEG(data, session, seq, origin)
		}, c.store.SetConnection)
		if err != nil && ctx.Err() == nil {
			c.store.SetConnection("error", "Camera capture stopped. Pause and resume to retry.")
		}
	}()
}

func (c *captureControl) pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		c.cancel()
		<-c.done
		c.cancel = nil
	}
	c.store.SetConnection("stopped", "Camera feed paused. Saved evidence remains available.")
}

type poseEndpoint struct {
	controller *camerapose.Controller
	context    context.Context
}

func handler(s cameraStore, token string, control func(string) error, pose ...poseEndpoint) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if len(pose) == 1 && r.URL.Path == "/v1/pose" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(pose[0].controller.Snapshot())
			return
		}
		if len(pose) == 1 && r.URL.Path == "/v1/pose/estimate" && r.Method == http.MethodPost {
			if err := pose[0].controller.Start(pose[0].context); err != nil {
				status := http.StatusServiceUnavailable
				if errors.Is(err, camerapose.ErrBusy) {
					status = http.StatusConflict
				}
				http.Error(w, "Position estimation is unavailable or already running", status)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, "{}\n")
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/state" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(s.State())
			return
		}
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/evidence/") {
			digest := strings.TrimPrefix(r.URL.Path, "/v1/evidence/")
			decoded, err := hex.DecodeString(digest)
			if err != nil || len(decoded) != 32 || strings.ToLower(digest) != digest {
				http.NotFound(w, r)
				return
			}
			data, err := s.Evidence(digest)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "image/jpeg")
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			_, _ = w.Write(data)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/v1/control" {
			var body struct {
				Action string `json:"action"`
			}
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&body); err != nil {
				http.Error(w, "invalid action", 400)
				return
			}
			var extra any
			if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) || (body.Action != "pause" && body.Action != "resume") {
				http.Error(w, "invalid action", 400)
				return
			}
			if err := control(body.Action); err != nil {
				http.Error(w, "capture unavailable", 503)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "{}\n")
			return
		}
		http.NotFound(w, r)
	})
}

func run(args []string) error {
	f := flag.NewFlagSet("world-camera", flag.ContinueOnError)
	device := f.String("device", "", "Wendy Lite selector from wendy discover")
	id := f.Uint("camera-id", 0, "SensorLink camera channel from camera list")
	dir := f.String("state-dir", "", "absolute private directory for receipts, evidence and viewer profile")
	wendy := f.String("wendy", "wendy", "Wendy CLI executable")
	siteDir := f.String("site-dir", "", "existing Wendy site directory containing model and visual map")
	poseHelper := f.String("pose-helper", "", "native Wendy site-localization helper executable")
	calibrationProfile := f.String("calibration-profile", "", "absolute path to a measured camera intrinsics profile")
	calibrationBindingPath := f.String("calibration-binding", "", "explicit camera hardware and capture-mode binding JSON")
	cameraConfigPath := f.String("camera-config", "", "private JSON array of camera configurations")
	emulatedSource := f.String("emulated-source", "", "absolute executable emitting original MJPEG from emulated firmware; requires --simulation-run")
	simulationRun := f.String("simulation-run", "", "explicit run ID for a live emulated camera; marks all receipts simulated")
	duration := f.Duration("duration", time.Hour, "maximum capture session duration")
	if err := f.Parse(args); err != nil {
		return err
	}
	if (*calibrationProfile == "") != (*calibrationBindingPath == "") || (*cameraConfigPath != "" && (*calibrationProfile != "" || *calibrationBindingPath != "")) {
		return errors.New("use both --calibration-profile and --calibration-binding for one camera, or configure calibration per camera with --camera-config")
	}
	var calibrationBinding *camerapose.CalibrationBinding
	if *calibrationProfile != "" {
		if !filepath.IsAbs(*calibrationProfile) {
			return errors.New("calibration profile path must be absolute")
		}
		var err error
		calibrationBinding, err = readCalibrationBinding(*calibrationBindingPath)
		if err != nil {
			return err
		}
	}
	idSet := false
	simulationSet := false
	f.Visit(func(v *flag.Flag) {
		if v.Name == "simulation-run" {
			simulationSet = true
		}
		if v.Name == "camera-id" {
			idSet = true
		}
	})
	if simulationSet && (strings.TrimSpace(*simulationRun) == "" || len(*simulationRun) > 256 || strings.ContainsAny(*simulationRun, "\x00\r\n")) {
		return errors.New("--simulation-run requires a nonempty run ID of at most 256 bytes without control line breaks")
	}
	if *emulatedSource != "" && (*simulationRun == "" || !filepath.IsAbs(*emulatedSource) || *cameraConfigPath != "") {
		return errors.New("--emulated-source requires an absolute path and --simulation-run; configure emulated_source per camera with --camera-config")
	}
	if f.NArg() != 0 || (*cameraConfigPath == "" && (!idSet || !strings.HasPrefix(*device, "wendy-lite:") || len(*device) <= len("wendy-lite:"))) || !filepath.IsAbs(*dir) || *duration < time.Second || *duration > 24*time.Hour || uint64(*id) > uint64(^uint32(0)) {
		return errors.New("provide --device wendy-lite:HOST --camera-id ID --state-dir /private/path; duration must be 1s to 24h")
	}
	if err := os.MkdirAll(*dir, 0700); err != nil {
		return err
	}
	if st, err := os.Lstat(*dir); err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0077 != 0 {
		return errors.New("state directory must be private (mode 0700) and not a symlink")
	}
	configs := []cameraConfig{{ID: "camera-1", Name: "Stationary camera", Device: *device, CameraID: uint32(*id), EmulatedSource: *emulatedSource, CalibrationProfile: *calibrationProfile, CalibrationBinding: calibrationBinding}}
	if *cameraConfigPath != "" {
		var err error
		configs, err = readCameras(*cameraConfigPath)
		if err != nil {
			return err
		}
	}
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(signals, *duration)
	defer cancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	var secret [32]byte
	if _, err = rand.Read(secret[:]); err != nil {
		return err
	}
	token := hex.EncodeToString(secret[:])
	twin, err := newTwin(ctx, configs, *dir, *wendy, *siteDir, *poseHelper, token, *cameraConfigPath == "", *simulationRun)
	if err != nil {
		return err
	}
	defer twin.close()

	profile, err := json.MarshalIndent(map[string]any{"url": "http://" + listener.Addr().String(), "token": token, "pid": os.Getpid()}, "", "  ")
	if err != nil {
		return err
	}
	profilePath := filepath.Join(*dir, "viewer-profile.json")
	// The store lock excludes another running server. Replace old scoped profiles
	// atomically so a viewer never sees a partially written token.
	tmp, err := os.CreateTemp(*dir, ".profile-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(profile); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, profilePath); err != nil {
		return err
	}
	server := &http.Server{Handler: twin, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 15 * time.Second, MaxHeaderBytes: 8192}
	errorsCh := make(chan error, 1)
	go func() { errorsCh <- server.Serve(listener) }()
	twin.start()
	fmt.Printf("Stationary camera service ready. Viewer profile: %s\n", profilePath)
	select {
	case <-ctx.Done():
	case err = <-errorsCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	return server.Shutdown(shutdownCtx)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
