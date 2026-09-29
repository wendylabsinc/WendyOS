package worldview

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/inference"
)

const fakePython = "/usr/bin/python3"

// fakeWorker speaks the worker protocol without uv or OpenCV. It records the
// config line, answers ready, and echoes one proposals line per input until an
// end line. In hold mode it also starts a grandchild and never exits by itself.
const fakeWorker = `import json, os, subprocess, sys
directory, mode = sys.argv[1], sys.argv[2]
config = sys.stdin.readline()
with open(os.path.join(directory, "config.json"), "w") as f:
    f.write(config)
if mode == "hold":
    child = subprocess.Popen(["/bin/sleep", "60"], stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    with open(os.path.join(directory, "pids.tmp"), "w") as f:
        f.write("%d %d" % (os.getpid(), child.pid))
    os.rename(os.path.join(directory, "pids.tmp"), os.path.join(directory, "pids"))
print(json.dumps({"type": "ready"}), flush=True)
for line in sys.stdin:
    item = json.loads(line)
    if item.get("end"):
        break
    print(json.dumps({"type": "proposals", "source_id": item["source_id"], "generation": item["generation"],
        "sample_id": item["sample_id"], "boot_nanos": item["boot_nanos"], "frame_w": item.get("width", 0),
        "frame_h": item.get("height", 0), "achieved_fps": 4.0, "depth_paired": item["kind"] == "depth",
        "proposals": [{"box": [1, 2, 3, 4], "area_px": 12, "palette": [{"lab": [53, 80, 67], "share": 1.0}],
                       "silhouette": {"primitive": "rect", "aspect": 0.5}, "metric": None}]}), flush=True)
`

func fakeFactory(t *testing.T, mode string) (*ManagedFactory, string) {
	t.Helper()
	if _, err := os.Stat(fakePython); err != nil {
		t.Skip("requires " + fakePython)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "fake_worker.py")
	if err := os.WriteFile(script, []byte(fakeWorker), 0600); err != nil {
		t.Fatal(err)
	}
	factory := &ManagedFactory{
		Root:    filepath.Join(dir, "root"),
		Launch:  func(_, dir string) *exec.Cmd { return exec.Command(fakePython, "-u", script, dir, mode) },
		prepare: func(context.Context) (string, string, error) { return "unused-uv", dir, nil },
	}
	return factory, dir
}

func rate(v float64) *float64 { return &v }
func every(v int) *int        { return &v }

func testConfig() Config {
	return Config{Proposer: ProposerContours, Rate: rate(4), MaxProposals: 50, MinAreaFraction: 0.002,
		Depth: &DepthConfig{ScaleM: 0.001, Intrinsics: Intrinsics{Fx: 320, Fy: 320, Cx: 160, Cy: 120}},
		Pairs: map[string]string{"v4l2:/dev/video0": "depth:0"}}
}

func startFake(t *testing.T, mode string) (Session, string) {
	t.Helper()
	factory, dir := fakeFactory(t, mode)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	session, err := factory.Start(ctx, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, dir
}

func TestFakeWorkerConfigRoundTripAndOneResultPerInput(t *testing.T) {
	session, dir := startFake(t, "echo")
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got Config
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, testConfig()) {
		t.Fatalf("config did not round-trip:\n got %+v\nwant %+v", got, testConfig())
	}
	if !strings.Contains(string(raw), `"every_frames":null`) {
		t.Fatalf("unset every_frames must be sent as null: %s", raw)
	}
	inputs := []Input{
		{Kind: KindRGB, SourceID: "v4l2:/dev/video0", Generation: 3, SampleID: 7, BootNanos: 1000, Encoding: "h264", Payload: []byte{0, 0, 0, 1}},
		{Kind: KindDepth, SourceID: "depth:0", Generation: 4, SampleID: 8, BootNanos: 2000, Encoding: "z16", Width: 2, Height: 1, Payload: []byte{1, 0, 2, 0}},
		{Kind: KindRGB, SourceID: "v4l2:/dev/video0", Generation: 3, SampleID: 9, BootNanos: 3000, Encoding: "h264", Payload: []byte{0, 0, 0, 1}},
	}
	for _, input := range inputs {
		if err := session.Send(input); err != nil {
			t.Fatal(err)
		}
		select {
		case result := <-session.Results():
			if result.Type != "proposals" || result.SourceID != input.SourceID || result.Generation != input.Generation ||
				result.SampleID != input.SampleID || result.BootNanos != input.BootNanos || result.DroppedResults != 0 {
				t.Fatalf("result %+v does not answer input %+v", result, input)
			}
			if result.DepthPaired != (input.Kind == KindDepth) || result.FrameW != input.Width || len(result.Proposals) != 1 {
				t.Fatalf("result fields not decoded: %+v", result)
			}
			p := result.Proposals[0]
			if p.Box != [4]float64{1, 2, 3, 4} || p.AreaPx != 12 || p.Palette[0].Lab != [3]float64{53, 80, 67} ||
				p.Silhouette.Primitive != "rect" || p.Silhouette.Aspect != 0.5 || p.Metric != nil {
				t.Fatalf("proposal not decoded: %+v", p)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("no result for input")
		}
	}
	if err := session.Send(Input{Kind: KindRGB, SourceID: "v4l2:/dev/video0", Generation: 3, End: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case result, ok := <-session.Results():
		if ok {
			t.Fatalf("unexpected result after end: %+v", result)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("fake worker did not exit on end")
	}
}

func TestFakeWorkerSlowConsumerDropsOldest(t *testing.T) {
	session, _ := startFake(t, "echo")
	const sent = 40
	for i := uint64(1); i <= sent; i++ {
		if err := session.Send(Input{Kind: KindRGB, SourceID: "cam", Generation: 1, SampleID: i, Encoding: "h264", Payload: []byte{1}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := session.Send(Input{Kind: KindRGB, SourceID: "cam", Generation: 1, End: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.(*processSession).done:
	case <-time.After(10 * time.Second):
		t.Fatal("fake worker did not exit")
	}
	var got []Result
	for result := range session.Results() {
		got = append(got, result)
	}
	capacity := cap(session.(*processSession).results)
	if len(got) != capacity {
		t.Fatalf("got %d buffered results, want %d", len(got), capacity)
	}
	last := got[len(got)-1]
	if last.SampleID != sent || last.DroppedResults != sent-uint64(capacity) {
		t.Fatalf("last result sample %d dropped %d; want %d and %d", last.SampleID, last.DroppedResults, sent, sent-capacity)
	}
	if first := got[0]; first.SampleID != sent-uint64(capacity)+1 {
		t.Fatalf("oldest kept sample %d; want %d", first.SampleID, sent-capacity+1)
	}
}

func TestConfigValidation(t *testing.T) {
	valid := testConfig()
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	everyFrames := testConfig()
	everyFrames.Rate, everyFrames.EveryFrames = nil, every(4)
	if err := everyFrames.Validate(); err != nil {
		t.Fatalf("every_frames config rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"both rate settings":    func(c *Config) { c.EveryFrames = every(4) },
		"neither rate setting":  func(c *Config) { c.Rate = nil },
		"rate zero":             func(c *Config) { c.Rate = rate(0) },
		"rate above 30":         func(c *Config) { c.Rate = rate(30.5) },
		"every_frames zero":     func(c *Config) { c.Rate, c.EveryFrames = nil, every(0) },
		"every_frames over 300": func(c *Config) { c.Rate, c.EveryFrames = nil, every(301) },
		"unknown proposer":      func(c *Config) { c.Proposer = "yolo" },
		"no proposals":          func(c *Config) { c.MaxProposals = 0 },
		"area fraction one":     func(c *Config) { c.MinAreaFraction = 1 },
		"zero depth scale":      func(c *Config) { c.Depth.ScaleM = 0 },
		"zero focal length":     func(c *Config) { c.Depth.Intrinsics.Fx = 0 },
		"empty pair":            func(c *Config) { c.Pairs = map[string]string{"cam": ""} },
	} {
		config := testConfig()
		mutate(&config)
		if err := config.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
		// Validation runs before any runtime work, so no process is launched.
		factory := &ManagedFactory{Root: t.TempDir(), prepare: func(context.Context) (string, string, error) {
			t.Fatalf("%s: runtime prepared for an invalid config", name)
			return "", "", nil
		}}
		if _, err := factory.Start(context.Background(), config); err == nil {
			t.Errorf("%s: started", name)
		}
	}
}

func TestDefaultLaunchUsesSharedInferenceCommand(t *testing.T) {
	command := inference.Command("/data/uv-0.10.9", "/data/runtime-abc", "worker.py")
	want := []string{"/data/uv-0.10.9", "run", "--project", "/data/runtime-abc", "--frozen", "--no-dev", "--no-build",
		"--managed-python", "--python", "3.12", "python", "-u", "/data/runtime-abc/worker.py"}
	if !reflect.DeepEqual(command.Args, want) || command.Dir != "/data/runtime-abc" {
		t.Fatalf("command %v in %q", command.Args, command.Dir)
	}
	for _, name := range assetFiles {
		if _, err := assets.ReadFile(name); err != nil {
			t.Fatalf("embedded asset %s: %v", name, err)
		}
	}
}

func TestWorkerStartupFailureReportsStderr(t *testing.T) {
	factory, _ := fakeFactory(t, "echo")
	factory.Launch = func(_, _ string) *exec.Cmd {
		return exec.Command(fakePython, "-c", "import sys; sys.stdin.readline(); sys.stderr.write('no opencv here'); sys.exit(3)")
	}
	_, err := factory.Start(context.Background(), testConfig())
	if err == nil || !strings.Contains(err.Error(), "no opencv here") {
		t.Fatalf("startup failure lost the worker's stderr: %v", err)
	}
}

// Opt-in integration: the exact embedded runtime and worker, no agent or camera.
func TestManagedRuntimeSmoke(t *testing.T) {
	video := os.Getenv("WENDY_WORLDVIEW_SMOKE_H264")
	if video == "" {
		t.Skip("set WENDY_WORLDVIEW_SMOKE_H264 to an H.264 Annex B fixture with a distinct object; downloads the runtime")
	}
	if !Supported() {
		t.Skip("unsupported runtime platform")
	}
	root := os.Getenv("WENDY_WORLDVIEW_SMOKE_CACHE")
	if root == "" {
		root = t.TempDir()
	}
	payload, err := os.ReadFile(video)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) == 0 {
		t.Fatal("empty video fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	session, err := (&ManagedFactory{Root: root}).Start(ctx, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	started := time.Now()
	feedErr := make(chan error, 1)
	go func() {
		depth := make([]byte, 160*120*2)
		for i := 0; i < len(depth); i += 2 {
			depth[i], depth[i+1] = 0xe8, 0x03 // 1000 little endian
		}
		var sample uint64
		for ctx.Err() == nil {
			for pos := 0; pos < len(payload); pos += 4096 {
				sample++
				now := time.Since(started).Nanoseconds()
				inputs := []Input{{Kind: KindRGB, SourceID: "v4l2:/dev/video0", Generation: 1, SampleID: sample, BootNanos: now, Encoding: "h264", Payload: payload[pos:min(pos+4096, len(payload))]}}
				if sample%5 == 0 {
					inputs = append(inputs, Input{Kind: KindDepth, SourceID: "depth:0", Generation: 1, SampleID: sample, BootNanos: now, Encoding: "z16", Width: 160, Height: 120, Payload: depth})
				}
				for _, input := range inputs {
					if err := session.Send(input); err != nil {
						feedErr <- err
						return
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Millisecond):
				}
			}
		}
	}()
	timer := time.NewTimer(90 * time.Second)
	defer timer.Stop()
	for {
		select {
		case result, ok := <-session.Results():
			if !ok {
				t.Fatal("runtime exited before proposals")
			}
			if result.Type == "source_error" || result.Type == "error" {
				t.Fatalf("worker reported %s: %s", result.Type, result.Error)
			}
			if result.Type == "proposals" && result.DepthPaired && len(result.Proposals) > 0 && result.Proposals[0].Metric != nil {
				t.Logf("managed runtime proposed %d objects in %dx%d; first %+v metric %+v", len(result.Proposals),
					result.FrameW, result.FrameH, result.Proposals[0], *result.Proposals[0].Metric)
				return
			}
		case err := <-feedErr:
			if err != io.EOF {
				t.Fatal(err)
			}
		case <-timer.C:
			t.Fatal("runtime produced no depth-paired proposals")
		}
	}
}
