package calib

import (
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/spatialsim"
)

var probeRef = spatialsim.DefaultReferenceConfig()

func probeRun(t *testing.T, seed uint64, until time.Duration, landmarks bool, cfg Config) (*spatialsim.Site, *AutoCalibrator) {
	opt := spatialsim.Options{}
	if os.Getenv("PROBE_NOCLOCK") != "" {
		sc := spatialsim.DefaultConfig()
		sc.ClockOffsetMax = 0
		opt.Config = &sc
	}
	if os.Getenv("PROBE_QUIET") != "" {
		opt.NoDisturbances = true
	}
	if os.Getenv("PROBE_NOTARGET") != "" {
		opt.TargetShows = []spatialsim.TargetShow{}
	}
	mk := spatialsim.Office
	switch os.Getenv("PROBE_SCEN") {
	case "single":
		mk = spatialsim.SingleRoom
	case "corridor":
		mk = spatialsim.Corridor
	case "two":
		mk = spatialsim.TwoRooms
	}
	site, err := mk(seed, opt)
	if err != nil {
		t.Fatal(err)
	}
	rc := probeRef
	switch os.Getenv("PROBE_REF") {
	case "exact":
		rc = spatialsim.ReferenceConfig{}
	case "noyaw":
		rc.PoseYawSigma = 0
	case "nopos":
		rc.PoseSigmaM = 0
	case "white":
		rc.PoseCorrelation = 0
	}
	ref, err := site.Reference(rc)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := strconv.Atoi(os.Getenv("PROBE_T")); err == nil {
		cfg.PoseErrorCorrelation = time.Duration(v) * time.Second
	}
	if v, err := strconv.Atoi(os.Getenv("PROBE_W")); err == nil {
		cfg.PoseErrorWindow = time.Duration(v) * time.Second
	}
	if v, err := strconv.ParseFloat(os.Getenv("PROBE_ELEV"), 64); err == nil {
		cfg.ElevationSigmaM = v
	}
	a, err := NewAutoCalibrator(cfg, site.Target(), site.Fiducials())
	if err != nil {
		t.Fatal(err)
	}
	if landmarks {
		if err := a.SetLandmarks(ref.Landmarks()); err != nil {
			t.Fatal(err)
		}
	}
	var cams, robots []spatial.DeviceID
	for _, m := range site.Manifests() {
		if m.Modality == spatial.KindCamera && m.Attachment.Mode == spatial.AttachFixed {
			cams = append(cams, m.Device)
		}
	}
	for _, f := range site.Fiducials() {
		if f.Carrier != "" {
			robots = append(robots, f.Carrier)
		}
	}
	const posePeriod = 50 * time.Millisecond
	next := time.Duration(0)
	for m := range site.Stream(site.At(0), site.At(until), spatialsim.Filter{Devices: cams, Kinds: []spatial.SensorKind{spatial.KindCamera}}) {
		for ; site.At(next).Before(m.When().Add(200 * time.Millisecond)); next += posePeriod {
			for _, r := range robots {
				if os.Getenv("PROBE_NOPOSE") != "" {
					break
				}
				if p, ok := ref.DevicePose(r, site.At(next)); ok {
					if err := a.IngestPose(p, site.At(next)); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
		if err := a.Ingest(m); err != nil {
			t.Fatal(err)
		}
	}
	return site, a
}

func TestProbe(t *testing.T) {
	for _, until := range probeUntils() {
		for _, lm := range probeLM() {
			var zpp, zpos, zrot, zf, zdt float64
			perCam := map[spatial.DeviceID][7]float64{}
			defer func() {
				for _, d := range []spatial.DeviceID{"cam-lobby", "cam-corridor-w", "cam-corridor-e", "cam-kitchen", "cam-lab", "cam-storage", "cam-yard", "cam-upper", "cam-late"} {
					pc := perCam[d]
					t.Logf("PERCAM %-15s z2 pp %.2f pos %.2f rot %.2f fx %.2f | max pp err %.2f mean pp sigma %.2f", d, pc[0]/pc[4], pc[1]/pc[4], pc[2]/pc[4], pc[3]/pc[4], pc[5], pc[6]/pc[4])
				}
			}()
			n := 0
			for _, seed := range probeSeeds() {
				site, a := probeRun(t, seed, until, lm, Config{})
				tr := site.Truth()
				for _, c := range a.Cameras() {
					s0 := time.Now()
					st, _ := a.Status(c.Device, c.Sensor)
					el := time.Since(s0)
					in, pose, _ := tr.Camera(c.Device, site.At(200*time.Second))
					off, _ := tr.ClockOffset(c.Device, c.Sensor)
					e := st.Estimate
					if os.Getenv("PROBE_ASPECT") != "" {
						cv := e.Covariance
						t.Logf("ASPECT seed %d %-15s fy/fx-1 = %+.4f%% (σfx %.2f σfy %.2f px) σcx %.2f σcy %.2f | errcx %+.2f errcy %+.2f", seed, c.Device, (e.Intrinsics.FY/e.Intrinsics.FX-1)*100, math.Sqrt(cv.At(0, 0)), math.Sqrt(cv.At(1, 1)), math.Sqrt(cv.At(2, 2)), math.Sqrt(cv.At(3, 3)), e.Intrinsics.CX-in.CX, e.Intrinsics.CY-in.CY)
					}
					pc := perCam[c.Device]
					pc[0] += sq(principalError(e.Intrinsics, in) / e.PrincipalSigmaPx)
					pc[1] += sq(e.CameraToSite.T.Dist(pose.T) / e.TranslationSigmaM)
					pc[2] += sq(geom.RotAngle(e.CameraToSite.R, pose.R) / e.RotationSigmaRad)
					pc[3] += sq((e.Intrinsics.FX - in.FX) / in.FX / e.FocalSigma)
					pc[4]++
					pc[5] = math.Max(pc[5], principalError(e.Intrinsics, in))
					pc[6] += e.PrincipalSigmaPx
					perCam[c.Device] = pc
					zpp += sq(principalError(e.Intrinsics, in) / e.PrincipalSigmaPx)
					zpos += sq(e.CameraToSite.T.Dist(pose.T) / e.TranslationSigmaM)
					zrot += sq(geom.RotAngle(e.CameraToSite.R, pose.R) / e.RotationSigmaRad)
					zf += sq((e.Intrinsics.FX - in.FX) / in.FX / e.FocalSigma)
					zdt += sq(float64(e.TimeOffset-off) / float64(e.TimeOffsetSigma))
					n++
					t.Logf("%v lm=%v seed %d %-15s %-10s views %3d cov %.2f passes %2d lms %d out %d/%d | f %.3f%% (σ %.3f) pp %.2f (σ %.2f) pos %.3f (σ %.3f) rot %.3f° (σ %.3f) dt err %v (σ %v) rmse %.2f max %.1f | %v %s",
						until, lm, seed, c.Device, st.State, st.TargetViews, st.ImageCoverage, st.MarkerPasses, st.Landmarks, st.Outliers, st.MarkerPoints, focalError(e.Intrinsics, in)*100, e.FocalSigma*100, principalError(e.Intrinsics, in), e.PrincipalSigmaPx,
						e.CameraToSite.T.Dist(pose.T), e.TranslationSigmaM, geom.RotAngle(e.CameraToSite.R, pose.R)/deg, e.RotationSigmaRad/deg,
						e.TimeOffset-off, e.TimeOffsetSigma, e.Residuals.RMSE, e.Residuals.Max, el, st.Detail)
				}
			}
			t.Logf("%v lm=%v normalised squared error (want 1): pp %.2f pos %.2f rot %.2f fx %.2f dt %.2f", until, lm, zpp/float64(n), zpos/float64(n), zrot/float64(n), zf/float64(n), zdt/float64(n))
		}
	}
}

func sq(x float64) float64 { return x * x }

func probeUntils() []time.Duration {
	var out []time.Duration
	for _, f := range strings.Split(os.Getenv("PROBE_UNTIL"), ",") {
		if v, err := strconv.Atoi(f); err == nil {
			out = append(out, time.Duration(v)*time.Second)
		}
	}
	if out == nil {
		out = []time.Duration{240 * time.Second}
	}
	return out
}

func probeLM() []bool {
	switch os.Getenv("PROBE_LM") {
	case "1":
		return []bool{true}
	case "both":
		return []bool{false, true}
	}
	return []bool{false}
}

func probeSeeds() []uint64 {
	var out []uint64
	for _, f := range strings.Split(os.Getenv("PROBE_SEEDS"), ",") {
		if v, err := strconv.ParseUint(f, 10, 64); err == nil {
			out = append(out, v)
		}
	}
	if out == nil {
		out = []uint64{1}
	}
	return out
}
