package framesource

// The RealSense provider: what the agent knows about RealSense cameras without
// opening one, and how it degrades when the capture helper is absent.
//
// The important case is the degraded one. A RealSense presents four to six
// /dev/video* nodes, none of which the agent can turn into a calibrated frame:
// the depth node is Z16, which GStreamer's v4l2 element cannot capture at all,
// and even if it could, colour and depth are separate nodes with separate
// clocks and no extrinsics between them (see the note above rawPixelFormats in
// go/internal/agent/services/video_raw_tap.go). So a RealSense without the
// helper is a camera the agent can SEE and cannot USE — and it says so, by
// name, rather than reporting that this device has no depth camera.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.uber.org/zap"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// KindRealSense is the capture technology tag CalibratedSource.kind carries.
const KindRealSense = "realsense"

// unavailableRealSenseName is the source id used for RealSense hardware that is
// attached but not capturable. Individual cameras cannot be named without the
// helper — their serials come from librealsense — so the whole class gets one
// entry rather than several made-up ones.
const unavailableRealSenseName = "realsense"

// v4l2NameDir holds one file per /dev/videoN containing the driver's name for
// it. A var so the detector can be pointed at a fixture.
var v4l2NameDir = "/sys/class/video4linux"

// realSenseNameMarkers are what an Intel RealSense calls itself in
// /sys/class/video4linux/videoN/name. Matched case-insensitively.
var realSenseNameMarkers = []string{"realsense"}

// DetectRealSense lists the distinct model names of the RealSense cameras
// attached to this host. It reads sysfs only: it never opens a node, so it is
// safe to call while another process is streaming from one.
var DetectRealSense = func() []string {
	entries, err := os.ReadDir(v4l2NameDir)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "video") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(v4l2NameDir, e.Name(), "name"))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(raw))
		lower := strings.ToLower(name)
		for _, marker := range realSenseNameMarkers {
			if strings.Contains(lower, marker) {
				seen[name] = true
				break
			}
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// RealSenseProvider enumerates RealSense cameras through the capture helper,
// and reports them as present-but-unavailable when there is no helper to reach
// them with.
type RealSenseProvider struct {
	Logger *zap.Logger
	// Detect lists attached RealSense model names. Injected in tests.
	Detect func() []string
	// Launcher starts the helper. When nil, FindLauncher resolves one at each
	// call so a helper installed while the agent runs is picked up without a
	// restart.
	Launcher Launcher
	// FindLauncher resolves a launcher, or reports why there is none. Injected
	// in tests; defaults to looking the helper up on disk.
	FindLauncher func() (Launcher, error)
}

// NewRealSenseProvider builds the provider the agent uses in production.
func NewRealSenseProvider(logger *zap.Logger) *RealSenseProvider {
	return &RealSenseProvider{
		Logger: logger,
		Detect: DetectRealSense,
		FindLauncher: func() (Launcher, error) {
			path, err := FindHelper(HelperName)
			if err != nil {
				return nil, err
			}
			return ExecLauncher{Path: path, Logger: logger}, nil
		},
	}
}

func (p *RealSenseProvider) detect() []string {
	if p.Detect != nil {
		return p.Detect()
	}
	return DetectRealSense()
}

func (p *RealSenseProvider) launcher() (Launcher, error) {
	if p.Launcher != nil {
		return p.Launcher, nil
	}
	if p.FindLauncher != nil {
		return p.FindLauncher()
	}
	return nil, fmt.Errorf("%w: no helper resolver configured", ErrHelperNotInstalled)
}

// Sources enumerates what this provider can offer right now.
func (p *RealSenseProvider) Sources(ctx context.Context) ([]Source, error) {
	attached := p.detect()
	launcher, err := p.launcher()
	if err != nil {
		if len(attached) == 0 {
			// No RealSense and no helper is not a problem to report: this is
			// simply a device without one.
			return nil, nil
		}
		return []Source{NewUnavailableSource(
			unavailableRealSenseName, KindRealSense,
			strings.Join(attached, ", "),
			helperMissingReason(attached, err),
		)}, nil
	}

	descs, err := describeThroughHelper(ctx, launcher)
	if err != nil {
		if len(attached) == 0 {
			return nil, nil
		}
		return []Source{NewUnavailableSource(
			unavailableRealSenseName, KindRealSense,
			strings.Join(attached, ", "),
			fmt.Sprintf("%s is installed at %s but could not enumerate this device: %v",
				HelperName, launcher.Binary(), err),
		)}, nil
	}
	if len(descs) == 0 && len(attached) > 0 {
		return []Source{NewUnavailableSource(
			unavailableRealSenseName, KindRealSense,
			strings.Join(attached, ", "),
			fmt.Sprintf("the kernel reports %s attached but %s enumerated no device; "+
				"check that the agent can open the camera's USB node",
				strings.Join(attached, ", "), HelperName),
		)}, nil
	}
	out := make([]Source, 0, len(descs))
	for _, d := range descs {
		out = append(out, NewHelperSource(d, launcher, p.Logger))
	}
	return out, nil
}

// helperMissingReason is the sentence an operator gets for a RealSense with no
// helper. It names the hardware, says why the ordinary camera path cannot serve
// it, and says what to install — because the alternative answer, silence, is
// what produced the field failure this service was written for.
func helperMissingReason(attached []string, err error) string {
	return fmt.Sprintf(
		"%s is attached but the %s capture helper is not installed, so the agent cannot produce calibrated frames from it "+
			"(its depth node is Z16, which the shared camera pipeline cannot capture, and its colour and depth nodes carry no alignment between them). %v",
		strings.Join(attached, ", "), HelperName, err)
}

// describeThroughHelper runs the helper's enumeration mode and collects every
// source descriptor it reports.
func describeThroughHelper(ctx context.Context, launcher Launcher) ([]*agentpbv2.CalibratedSource, error) {
	run, err := launcher.Start(ctx, []string{"describe"})
	if err != nil {
		return nil, err
	}
	defer run.Stop()

	var out []*agentpbv2.CalibratedSource
	for {
		src, err := ReadSource(run.Records)
		if err != nil {
			if isCleanEnd(err) {
				break
			}
			return nil, err
		}
		out = append(out, src)
	}
	if err := run.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}
