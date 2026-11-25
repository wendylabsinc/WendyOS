// Package twinvision owns camera matching policy for every digital-twin client.
package twinvision

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strings"
	"time"
)

const PolicyVersion = 1

type Frame struct {
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	Codec    string  `json:"codec"`
	Sequence uint64  `json:"sequence"`
	AgeMS    float64 `json:"ageMS"`
	Gray     []byte  `json:"gray"`
}

func (f Frame) Validate() error {
	if f.Width < 32 || f.Height < 32 || f.Width > 8192 || f.Height > 8192 || f.Width*f.Height > 32000000 || len(f.Gray) != f.Width*f.Height || f.Sequence == 0 || !finite(f.AgeMS) || f.AgeMS < 0 || f.AgeMS > 1500 || (f.Codec != "H.264" && f.Codec != "JPEG") {
		return errors.New("a fresh camera frame in its original capture mode is required")
	}
	return nil
}
func (f Frame) Mode() string { return fmt.Sprintf("%s %dx%d", f.Codec, f.Width, f.Height) }

type Result struct {
	Source     int32       `json:"source"`
	Success    int32       `json:"success"`
	Reason     int32       `json:"reason"`
	Matches    int32       `json:"matches"`
	Inliers    int32       `json:"inliers"`
	FittedLens int32       `json:"fittedLens"`
	FX         float64     `json:"fx"`
	FY         float64     `json:"fy"`
	CX         float64     `json:"cx"`
	CY         float64     `json:"cy"`
	Distortion [5]float64  `json:"distortion"`
	RMS        float64     `json:"rms"`
	Transform  [16]float64 `json:"transform"`
}

func (r Result) Lens() [9]float64 {
	return [9]float64{r.FX, r.FY, r.CX, r.CY, r.Distortion[0], r.Distortion[1], r.Distortion[2], r.Distortion[3], r.Distortion[4]}
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func Landmarks(raw json.RawMessage, model []byte, revision string) ([]byte, error) {
	var m struct {
		Version, FrameCount        int
		ModelRevision, ModelSHA256 string
		Landmarks                  []byte
	}
	if e := json.Unmarshal(raw, &m); e != nil {
		return nil, e
	}
	if m.Version != 1 || m.FrameCount < 1 || m.FrameCount > 200 || !strings.EqualFold(m.ModelRevision, revision) || m.ModelSHA256 != fmt.Sprintf("%x", sha256.Sum256(model)) || len(m.Landmarks) < 120*131*4 || len(m.Landmarks) > 12000*131*4 || len(m.Landmarks)%(131*4) != 0 {
		return nil, errors.New("room visual references do not match this model revision")
	}
	for i := 0; i < len(m.Landmarks); i += 4 {
		v := float64(math.Float32frombits(binary.LittleEndian.Uint32(m.Landmarks[i:])))
		if !finite(v) || (i/4%131 < 3 && math.Abs(v) > 1000) || (i/4%131 >= 3 && (v < 0 || v > 512)) {
			return nil, errors.New("invalid room visual references")
		}
	}
	return m.Landmarks, nil
}

// Solve runs the same bounded worker and OpenCV build for all clients. Native
// crashes and cancellation cannot kill or indefinitely block the twin service.
func Solve(ctx context.Context, landmarks []byte, f Frame, lens [9]float64, provenance int32, verification bool) (Result, error) {
	var result Result
	if e := f.Validate(); e != nil {
		return result, e
	}
	var b bytes.Buffer
	mode := int32(0)
	if verification {
		mode = 1
	}
	for _, v := range []int32{1, int32(f.Width), int32(f.Height), int32(len(landmarks) / (131 * 4)), mode, provenance} {
		binary.Write(&b, binary.LittleEndian, v)
	}
	binary.Write(&b, binary.LittleEndian, lens)
	b.Write(landmarks)
	b.Write(f.Gray)
	executable := os.Getenv("WENDY_TWIN_VISION_WORKER")
	if executable == "" {
		executable = "/usr/local/bin/wendy-twin-vision"
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable)
	cmd.Stdin = &b
	output, e := cmd.Output()
	if e != nil {
		return result, fmt.Errorf("camera matching worker failed: %w", e)
	}
	if len(output) != 6*4+26*8 {
		return result, errors.New("invalid camera worker response")
	}
	e = binary.Read(bytes.NewReader(output), binary.LittleEndian, &result)
	return result, e
}

func ValidateLens(lens [9]float64, width, height int) error {
	for _, v := range lens {
		if !finite(v) {
			return errors.New("nonfinite lens")
		}
	}
	if lens[0] <= 0 || lens[1] <= 0 || lens[0] > 1e7 || lens[1] > 1e7 || lens[2] < 0 || lens[2] > float64(width) || lens[3] < 0 || lens[3] > float64(height) {
		return errors.New("invalid lens")
	}
	for _, v := range lens[4:] {
		if math.Abs(v) > 100 {
			return errors.New("invalid distortion")
		}
	}
	// A numerically fitted lens must also have invertible boundary rays.
	for edge := 0; edge < 4; edge++ {
		for step := 0; step < 8; step++ {
			t := float64(step) / 8
			x, y := 0., 0.
			w, h := float64(width), float64(height)
			switch edge {
			case 0:
				x = t * w
			case 1:
				x = w
				y = t * h
			case 2:
				x = (1 - t) * w
				y = h
			case 3:
				y = (1 - t) * h
			}
			xd, yd := (x-lens[2])/lens[0], (y-lens[3])/lens[1]
			u, v := xd, yd
			radial := func(u, v float64) float64 { r := u*u + v*v; return 1 + r*(lens[4]+r*(lens[5]+r*lens[8])) }
			for j := 0; j < 40; j++ {
				r := u*u + v*v
				k := radial(u, v)
				if !finite(k) || k <= .01 {
					return errors.New("lens boundary ray does not converge")
				}
				du := 2*lens[6]*u*v + lens[7]*(r+2*u*u)
				dv := lens[6]*(r+2*v*v) + 2*lens[7]*u*v
				u = (xd - du) / k
				v = (yd - dv) / k
			}
			r := u*u + v*v
			k := radial(u, v)
			dx := (u*k + 2*lens[6]*u*v + lens[7]*(r+2*u*u) - xd) * lens[0]
			dy := (v*k + lens[6]*(r+2*v*v) + 2*lens[7]*u*v - yd) * lens[1]
			if !finite(dx) || !finite(dy) || math.Hypot(dx, dy) >= .1 {
				return errors.New("lens boundary ray does not converge")
			}
		}
	}
	return nil
}
func Validate(r Result, width, height int) error {
	if r.Source < 0 || r.Source > 3 {
		return errors.New("invalid camera result source")
	}
	if r.Success != 1 {
		return fmt.Errorf("camera match failed: %d inliers from %d matches (reason %d)", r.Inliers, r.Matches, r.Reason)
	}
	minimum := int32(12)
	limit := 2.5
	switch r.FittedLens {
	case 0:
	case 1:
		minimum = 16
	case 2:
		minimum = 8
		limit = 5
	default:
		return errors.New("invalid lens provenance")
	}
	if r.Inliers < minimum || !finite(r.RMS) || r.RMS < 0 || r.RMS > limit*float64(max(width, height))/640 {
		return errors.New("camera matches did not pass validation")
	}
	if e := ValidateLens(r.Lens(), width, height); e != nil {
		return e
	}
	t := r.Transform
	for _, v := range t {
		if !finite(v) || math.Abs(v) > 1e6 {
			return errors.New("invalid camera transform")
		}
	}
	if math.Abs(t[3])+math.Abs(t[7])+math.Abs(t[11])+math.Abs(t[15]-1) > 1e-5 {
		return errors.New("invalid camera transform")
	}
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			dot := 0.
			for k := 0; k < 3; k++ {
				dot += t[i*4+k] * t[j*4+k]
			}
			want := 0.
			if i == j {
				want = 1
			}
			if math.Abs(dot-want) > 1e-5 {
				return errors.New("invalid camera rotation")
			}
		}
	}
	det := t[0]*(t[5]*t[10]-t[9]*t[6]) - t[4]*(t[1]*t[10]-t[9]*t[2]) + t[8]*(t[1]*t[6]-t[5]*t[2])
	if math.Abs(det-1) > 1e-5 {
		return errors.New("invalid camera rotation")
	}
	return nil
}
func Verify(a, b Result, width, height int) (Result, error) {
	if e := Validate(a, width, height); e != nil {
		return Result{}, e
	}
	// A verification fixes the first frame's lens, so a joint fit only needs
	// pose support on frame two. The first frame still requires lens support.
	if e := Validate(b, width, height); e != nil {
		return Result{}, e
	}
	sameLens := true
	for i, v := range a.Lens() {
		if math.Abs(v-b.Lens()[i]) > 1e-8*math.Max(1, math.Abs(v)) {
			sameLens = false
		}
	}
	if !sameLens || (a.FittedLens == 2) != (b.FittedLens == 2) {
		return Result{}, errors.New("verification changed the lens")
	}
	distance := 0.
	trace := 0.
	for i := 0; i < 3; i++ {
		d := a.Transform[12+i] - b.Transform[12+i]
		distance += d * d
		for j := 0; j < 3; j++ {
			trace += a.Transform[i*4+j] * b.Transform[i*4+j]
		}
	}
	angle := math.Acos(math.Max(-1, math.Min(1, (trace-1)/2)))
	dmax, amax := .08, 3.
	if a.FittedLens == 2 {
		dmax, amax = .30, 10
	}
	if math.Sqrt(distance) > dmax || angle > amax*math.Pi/180 {
		return Result{}, errors.New("camera position changed between fresh frames")
	}
	b.FittedLens = a.FittedLens
	b.Source = a.Source
	b.Inliers = min(a.Inliers, b.Inliers)
	b.RMS = math.Max(a.RMS, b.RMS)
	if err := Validate(b, width, height); err != nil {
		return Result{}, err
	}
	return b, nil
}

// Quaternion converts a verified column-major proper rotation into x,y,z,w.
func Quaternion(t [16]float64) []float64 {
	q := make([]float64, 4)
	trace := t[0] + t[5] + t[10]
	if trace > 0 {
		s := math.Sqrt(trace+1) * 2
		q[3] = s / 4
		q[0] = (t[6] - t[9]) / s
		q[1] = (t[8] - t[2]) / s
		q[2] = (t[1] - t[4]) / s
	} else {
		i := 0
		if t[5] > t[0] {
			i = 1
		}
		if t[10] > t[i*5] {
			i = 2
		}
		j, k := (i+1)%3, (i+2)%3
		s := math.Sqrt(1+t[i*5]-t[j*5]-t[k*5]) * 2
		q[i] = s / 4
		q[j] = (t[j*4+i] + t[i*4+j]) / s
		q[k] = (t[k*4+i] + t[i*4+k]) / s
		q[3] = (t[j*4+k] - t[k*4+j]) / s
	}
	return q
}
