// Package worldview runs the agent-owned world view perception worker. The
// worker proposes and measures candidate objects (box, CIELAB palette,
// silhouette, and metric size from a paired depth frame); matching, fusion and
// tracking happen in the agent. Users never install Python or provide code.
package worldview

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/inference"
)

// Supported reports whether the shared managed runtime exists for this platform.
func Supported() bool { return inference.Supported() }

// Proposers the worker implements.
const ProposerContours = "contours"

type Config struct {
	Proposer        string            `json:"proposer"`
	Rate            *float64          `json:"rate"`
	EveryFrames     *int              `json:"every_frames"`
	MaxProposals    int               `json:"max_proposals"`
	MinAreaFraction float64           `json:"min_area_fraction"`
	Depth           *DepthConfig      `json:"depth"`
	Pairs           map[string]string `json:"pairs"`
}

type DepthConfig struct {
	ScaleM     float64    `json:"scale_m"`
	Intrinsics Intrinsics `json:"intrinsics"`
}

// Intrinsics are pinhole parameters in pixels of the decoded RGB frame, to
// which the worker scales a paired depth frame.
type Intrinsics struct {
	Fx float64 `json:"fx"`
	Fy float64 `json:"fy"`
	Cx float64 `json:"cx"`
	Cy float64 `json:"cy"`
}

func finite(values ...float64) bool {
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// Validate rejects configurations the worker would refuse, before any
// process is launched.
func (c Config) Validate() error {
	if c.Proposer != ProposerContours {
		return fmt.Errorf("world view proposer %q is unsupported", c.Proposer)
	}
	if (c.Rate == nil) == (c.EveryFrames == nil) {
		return errors.New("world view needs exactly one of rate and every_frames")
	}
	if c.Rate != nil && (!finite(*c.Rate) || *c.Rate <= 0 || *c.Rate > 30) {
		return errors.New("world view rate must be in (0, 30] frames per second per camera")
	}
	if c.EveryFrames != nil && (*c.EveryFrames < 1 || *c.EveryFrames > 300) {
		return errors.New("world view every_frames must be in [1, 300]")
	}
	if c.MaxProposals < 1 || c.MaxProposals > 500 {
		return errors.New("world view max_proposals must be in [1, 500]")
	}
	if !finite(c.MinAreaFraction) || c.MinAreaFraction < 0 || c.MinAreaFraction >= 1 {
		return errors.New("world view min_area_fraction must be in [0, 1)")
	}
	if d := c.Depth; d != nil {
		in := d.Intrinsics
		if !finite(d.ScaleM, in.Fx, in.Fy, in.Cx, in.Cy) || d.ScaleM <= 0 || in.Fx <= 0 || in.Fy <= 0 {
			return errors.New("world view depth needs a positive scale_m and positive focal lengths")
		}
	}
	for rgb, depth := range c.Pairs {
		if rgb == "" || depth == "" {
			return errors.New("world view pairs need nonempty RGB and depth source IDs")
		}
	}
	return nil
}

// Input is one encoded RGB sample or one raw depth frame. Depth frames use
// encoding z16: little-endian uint16, row-major, Width by Height.
type Input struct {
	Kind           string `json:"kind"`
	SourceID       string `json:"source_id"`
	Generation     uint64 `json:"generation"`
	SampleID       uint64 `json:"sample_id"`
	BootNanos      int64  `json:"boot_nanos"`
	Encoding       string `json:"encoding,omitempty"`
	Width          int    `json:"width,omitempty"`
	Height         int    `json:"height,omitempty"`
	Initialization []byte `json:"initialization,omitempty"`
	Payload        []byte `json:"payload,omitempty"`
	DroppedBefore  uint64 `json:"dropped_before,omitempty"`
	End            bool   `json:"end,omitempty"`
}

const (
	KindRGB   = "rgb"
	KindDepth = "depth"
)

type PaletteWire struct {
	Lab   [3]float64 `json:"lab"`
	Share float64    `json:"share"`
}

type SilhouetteWire struct {
	Primitive string  `json:"primitive"`
	Aspect    float64 `json:"aspect"`
}

type MetricWire struct {
	WidthM     float64 `json:"width_m"`
	HeightM    float64 `json:"height_m"`
	DistanceM  float64 `json:"distance_m"`
	BearingDeg float64 `json:"bearing_deg"`
}

// ProposalWire is one candidate object. Box is x, y, width, height in pixels
// of the decoded frame.
type ProposalWire struct {
	Box        [4]float64     `json:"box"`
	AreaPx     int            `json:"area_px"`
	Palette    []PaletteWire  `json:"palette"`
	Silhouette SilhouetteWire `json:"silhouette"`
	Metric     *MetricWire    `json:"metric"`
}

// Result is one worker line: "proposals", "source_error" or "error".
// SampleID and BootNanos are those of the latest input sample the decoder had
// read when the frame was produced.
type Result struct {
	DroppedResults uint64         `json:"dropped_results,omitempty"`
	Type           string         `json:"type"`
	SourceID       string         `json:"source_id"`
	Generation     uint64         `json:"generation"`
	SampleID       uint64         `json:"sample_id"`
	BootNanos      int64          `json:"boot_nanos"`
	FrameW         int            `json:"frame_w,omitempty"`
	FrameH         int            `json:"frame_h,omitempty"`
	AchievedFPS    float64        `json:"achieved_fps,omitempty"`
	DepthPaired    bool           `json:"depth_paired,omitempty"`
	Proposals      []ProposalWire `json:"proposals,omitempty"`
	Error          string         `json:"error,omitempty"`
}

type Session interface {
	Send(Input) error
	Results() <-chan Result
	Close() error
}

type Factory interface {
	Start(context.Context, Config) (Session, error)
}

// ManagedFactory runs the embedded worker in a locked runtime prepared by the
// shared inference installer under Root. Launch builds the unstarted worker
// command for a prepared runtime; nil runs worker.py with inference.Command.
type ManagedFactory struct {
	Root   string
	Launch func(uv, dir string) *exec.Cmd

	mu sync.Mutex
	// prepare replaces runtime installation in tests that launch a fake worker.
	prepare func(context.Context) (uv, dir string, err error)
}

const readyTimeout = 15 * time.Minute

func (f *ManagedFactory) prepareRuntime(ctx context.Context) (string, string, error) {
	if f.prepare != nil {
		return f.prepare(ctx)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !Supported() {
		return "", "", errors.New("world view is unsupported on this platform")
	}
	installer := &inference.ManagedFactory{Root: f.Root}
	return installer.Prepare(ctx, assets, assetFiles)
}

func (f *ManagedFactory) Start(ctx context.Context, config Config) (Session, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	uv, dir, err := f.prepareRuntime(ctx)
	if err != nil {
		return nil, err
	}
	launch := f.Launch
	if launch == nil {
		launch = func(uv, dir string) *exec.Cmd { return inference.Command(uv, dir, "worker.py") }
	}
	childCtx, cancel := context.WithCancel(ctx)
	command := inference.BindContext(childCtx, launch(uv, dir))
	if err := os.MkdirAll(filepath.Join(f.Root, "home"), 0700); err != nil {
		cancel()
		return nil, fmt.Errorf("creating world view runtime home: %w", err)
	}
	command.Env = inference.RuntimeEnvironment(f.Root)
	inference.ConfigureProcess(command)
	stderr := &inference.TailBuffer{}
	command.Stderr = stderr
	stdin, err := command.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		stdin.Close()
		cancel()
		return nil, err
	}
	if err := command.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		cancel()
		return nil, err
	}
	session := &processSession{stdin: stdin, cancel: cancel, results: make(chan Result, 16), done: make(chan struct{})}
	ready := make(chan error, 1)
	go func() {
		defer close(session.done)
		defer close(session.results)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		initialized := false
		var dropped uint64
		for scanner.Scan() {
			var result Result
			if err := json.Unmarshal(scanner.Bytes(), &result); err != nil {
				cancel()
				break
			}
			if !initialized {
				if result.Type != "ready" {
					cancel()
					break
				}
				initialized = true
				ready <- nil
				continue
			}
			result.DroppedResults = dropped
			select {
			case session.results <- result:
			case <-childCtx.Done():
			default:
				// Perception is best effort: discard the oldest pending result
				// rather than deadlocking camera teardown behind a full stdout pipe.
				select {
				case <-session.results:
					dropped++
					result.DroppedResults = dropped
				default:
				}
				select {
				case session.results <- result:
				default:
				}
			}
		}
		cancel()
		waitErr := command.Wait()
		if !initialized {
			ready <- fmt.Errorf("starting world view worker: %v: %s", waitErr, stderr.String())
		}
	}()
	if err := json.NewEncoder(stdin).Encode(config); err != nil {
		session.Close()
		return nil, err
	}
	timer := time.NewTimer(readyTimeout)
	defer timer.Stop()
	select {
	case err := <-ready:
		if err != nil {
			session.Close()
			return nil, err
		}
		return session, nil
	case <-ctx.Done():
		session.Close()
		return nil, ctx.Err()
	case <-timer.C:
		session.Close()
		return nil, errors.New("world view runtime did not become ready within 15 minutes")
	}
}

type processSession struct {
	mu      sync.Mutex
	stdin   io.WriteCloser
	cancel  context.CancelFunc
	results chan Result
	done    chan struct{}
}

func (s *processSession) Send(input Input) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.NewEncoder(s.stdin).Encode(input)
}
func (s *processSession) Results() <-chan Result { return s.results }
func (s *processSession) Close() error           { s.cancel(); <-s.done; return s.stdin.Close() }
