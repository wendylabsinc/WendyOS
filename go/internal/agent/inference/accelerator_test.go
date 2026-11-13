package inference

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ulikunitz/xz"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

var orin = Hardware{GPUVendor: "nvidia", ComputeBackends: []string{"cuda"}, CUDAVersion: "13.2", GPUArch: "sm_87"}

func yoloInference(accelerator string) data.CampaignInference {
	return data.CampaignInference{Backend: "yolo_onnx", ModelFile: "onnx/yolo11n.onnx", Accelerator: accelerator}
}

// withBinding installs a bindings row for this platform's libnvinfer 10.16.2.
func withBinding(t *testing.T, binding tensorRTBinding) {
	t.Helper()
	key := runtime.GOOS + "/" + runtime.GOARCH + "/10.16.2"
	previous, had := tensorRTBindings[key]
	tensorRTBindings[key] = binding
	t.Cleanup(func() {
		if had {
			tensorRTBindings[key] = previous
		} else {
			delete(tensorRTBindings, key)
		}
	})
}

func TestPlanAccelerator(t *testing.T) {
	withBinding(t, tensorRTBinding{version: "10.16.2.10", cudaMajor: "13"})
	pi := Hardware{GPUVendor: "broadcom"}
	x86 := Hardware{GPUVendor: "intel"}
	for _, test := range []struct {
		name       string
		config     data.CampaignInference
		hardware   Hardware
		library    string
		candidates []string
		degraded   bool
		reason     string
		binding    bool
	}{
		{"orin auto", yoloInference(""), orin, "10.16.2", []string{"tensorrt", "cpu"}, false, "", true},
		{"orin explicit auto", yoloInference("auto"), orin, "10.16.2", []string{"tensorrt", "cpu"}, false, "", true},
		{"orin tensorrt", yoloInference("tensorrt"), orin, "10.16.2", []string{"tensorrt"}, false, "", true},
		{"orin cpu", yoloInference("cpu"), orin, "10.16.2", []string{"cpu"}, false, "the campaign asks for the cpu", false},
		{"transformers on orin", data.CampaignInference{}, orin, "10.16.2", []string{"cpu"}, false, "only backend yolo_onnx", false},
		{"raspberry pi", yoloInference(""), pi, "", []string{"cpu"}, false, "GPU vendor broadcom", false},
		{"x86 without a GPU", yoloInference(""), x86, "", []string{"cpu"}, false, "no supported accelerator", false},
		{"no GPU at all", yoloInference(""), Hardware{}, "", []string{"cpu"}, false, "GPU vendor none", false},
		{"nvidia without cuda nodes", yoloInference(""), Hardware{GPUVendor: "nvidia"}, "10.16.2", []string{"cpu"}, false, "NVIDIA GPU with CUDA", false},
		{"x86 dGPU without TensorRT", yoloInference(""), Hardware{GPUVendor: "nvidia", ComputeBackends: []string{"cuda"}}, "", []string{"cpu"}, false, "no TensorRT libraries", false},
		{"unknown TensorRT", yoloInference(""), orin, "10.17.0", []string{"cpu"}, true, "update the agent", false},
		{"other CUDA major", yoloInference(""), Hardware{GPUVendor: "nvidia", ComputeBackends: []string{"cuda"}, CUDAVersion: "12.6"}, "10.16.2", []string{"cpu"}, true, "need CUDA 13", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, binding := planAccelerator(test.config, test.hardware, test.library)
			if !reflect.DeepEqual(plan.Candidates, test.candidates) || plan.Degraded != test.degraded || (binding != nil) != test.binding {
				t.Fatalf("plan %+v, binding %v", plan, binding)
			}
			if !strings.Contains(plan.Reason, test.reason) {
				t.Fatalf("reason %q lacks %q", plan.Reason, test.reason)
			}
			if plan.Requested != test.config.EffectiveAccelerator() {
				t.Fatalf("requested %q", plan.Requested)
			}
		})
	}
}

func TestHostTensorRTVersion(t *testing.T) {
	dir := t.TempDir()
	previous := tensorRTLibraryDirs
	tensorRTLibraryDirs = []string{filepath.Join(dir, "missing"), dir}
	t.Cleanup(func() { tensorRTLibraryDirs = previous })
	touch := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	touch("libnvinfer.so.10.16.2")
	if v := hostTensorRTVersion(); v != "" {
		t.Fatalf("TensorRT without its parser and plugins reported as %q", v)
	}
	touch("libnvonnxparser.so.10")
	touch("libnvinfer_plugin.so.10")
	if v := hostTensorRTVersion(); v != "10.16.2" {
		t.Fatalf("version %q", v)
	}
}

// fakeBindingsDeb builds a Debian package shaped like NVIDIA's: an ar archive
// with an xz-compressed data tarball holding the Python package and other files.
func fakeBindingsDeb(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var tarball bytes.Buffer
	compressed, err := xz.NewWriter(&tarball)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(compressed)
	write := func(name, content string) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: "./usr/lib/python3.12/dist-packages/tensorrt/", Mode: 0755, Typeflag: tar.TypeDir}); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		write("./usr/lib/python3.12/dist-packages/"+name, content)
	}
	write("./usr/lib/python3.12/dist-packages/tensorrt-10.16.2.10.dist-info/RECORD", "not extracted")
	write("./usr/share/doc/python3-libnvinfer/copyright", "not extracted")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	var deb bytes.Buffer
	deb.WriteString("!<arch>\n")
	member := func(name string, content []byte) {
		fmt.Fprintf(&deb, "%-16s%-12s%-6s%-6s%-8s%-10d`\n", name, "0", "0", "0", "100644", len(content))
		deb.Write(content)
		if len(content)%2 == 1 {
			deb.WriteByte('\n')
		}
	}
	member("debian-binary", []byte("2.0\n"))
	member("control.tar.xz/", []byte("control"))
	member("data.tar.xz/", tarball.Bytes())
	return deb.Bytes()
}

func digest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func TestTensorRTBindingsInstallExactlyThePinnedFiles(t *testing.T) {
	contents := map[string]string{"tensorrt/__init__.py": "from .tensorrt import *\n", "tensorrt/tensorrt.so": "\x7fELF bindings", "tensorrt/plugin/__init__.py": "plugins\n"}
	pins := map[string]string{}
	for name, content := range contents {
		pins[name] = digest(content)
	}
	deb := fakeBindingsDeb(t, contents)
	sum := sha256.Sum256(deb)
	for _, test := range []struct {
		name     string
		checksum string
		pins     map[string]string
		want     string
	}{
		{"valid", hex.EncodeToString(sum[:]), pins, ""},
		{"package checksum mismatch", strings.Repeat("0", 64), pins, "checksum mismatch"},
		{"tampered member", hex.EncodeToString(sum[:]), map[string]string{"tensorrt/__init__.py": digest("something else")}, "does not match its pinned checksum"},
		{"missing member", hex.EncodeToString(sum[:]), map[string]string{"tensorrt/missing.py": digest("")}, "lacks tensorrt/missing.py"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(deb) }))
			defer server.Close()
			destination := filepath.Join(t.TempDir(), "accel", "tensorrt-10.16.2.10")
			err := installTensorRTBindings(context.Background(), server.URL, test.checksum, test.pins, destination)
			if test.want != "" {
				if err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("err %v, want %q", err, test.want)
				}
				if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
					t.Fatal("rejected bindings were installed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := validTensorRTBindings(destination, pins); err != nil {
				t.Fatal(err)
			}
			var installed []string
			_ = filepath.Walk(destination, func(path string, info os.FileInfo, _ error) error {
				if !info.IsDir() {
					relative, _ := filepath.Rel(destination, path)
					installed = append(installed, filepath.ToSlash(relative))
				}
				return nil
			})
			if len(installed) != len(pins) {
				t.Fatalf("installed %v, pinned %v", installed, pins)
			}
			// A cached copy that changes is refused on the next start.
			if err := os.WriteFile(filepath.Join(destination, "tensorrt", "__init__.py"), []byte("import os\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if validTensorRTBindings(destination, pins) == nil {
				t.Fatal("modified cached bindings accepted")
			}
		})
	}
}

func TestTensorRTBindingsPreferTheImageThenTheCacheAndBackOff(t *testing.T) {
	contents := map[string]string{"tensorrt/__init__.py": "bindings\n"}
	deb := fakeBindingsDeb(t, contents)
	sum := sha256.Sum256(deb)
	var requests atomic.Int32
	fail := atomic.Bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		if fail.Load() {
			http.Error(w, "offline", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(deb)
	}))
	defer server.Close()
	binding := tensorRTBinding{version: "10.16.2.10", url: server.URL, sha: hex.EncodeToString(sum[:]), files: map[string]string{"tensorrt/__init__.py": digest("bindings\n")}}
	image := t.TempDir()
	previous := imageTensorRTBindingDirs
	imageTensorRTBindingDirs = []string{image}
	t.Cleanup(func() { imageTensorRTBindingDirs = previous })
	root := t.TempDir()

	// Offline, nothing cached: one attempt, then no retry before the backoff.
	fail.Store(true)
	factory := &ManagedFactory{}
	for range 3 {
		if _, err := factory.tensorRTBindings(context.Background(), root, binding); err == nil || !strings.Contains(err.Error(), "HTTP 503") {
			t.Fatalf("offline fetch: %v", err)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("%d requests while offline, want 1", requests.Load())
	}

	fail.Store(false)
	factory = &ManagedFactory{}
	dir, err := factory.tensorRTBindings(context.Background(), root, binding)
	if err != nil || dir != filepath.Join(root, "accel", "tensorrt-10.16.2.10") {
		t.Fatalf("dir %q, err %v", dir, err)
	}
	// Cached: no download.
	if dir, err = factory.tensorRTBindings(context.Background(), root, binding); err != nil || requests.Load() != 2 {
		t.Fatalf("cached bindings downloaded again: %d requests, %v", requests.Load(), err)
	}
	// An image that ships the same bindings wins, with no download.
	if err := os.MkdirAll(filepath.Join(image, "tensorrt"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(image, "tensorrt", "__init__.py"), []byte("bindings\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if dir, err = factory.tensorRTBindings(context.Background(), root, binding); err != nil || dir != image || requests.Load() != 2 {
		t.Fatalf("image bindings not preferred: %q, %v", dir, err)
	}
}

func TestAcceleratorPlanFallsBackLoudlyWithoutBindings(t *testing.T) {
	withBinding(t, tensorRTBinding{version: "10.16.2.10", cudaMajor: "13", url: "http://127.0.0.1:1/unreachable.deb", sha: strings.Repeat("0", 64), files: map[string]string{"tensorrt/__init__.py": digest("x")}})
	dir := t.TempDir()
	for _, name := range []string{"libnvinfer.so.10.16.2", "libnvonnxparser.so.10", "libnvinfer_plugin.so.10"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	previous, previousImage := tensorRTLibraryDirs, imageTensorRTBindingDirs
	tensorRTLibraryDirs, imageTensorRTBindingDirs = []string{dir}, nil
	t.Cleanup(func() { tensorRTLibraryDirs, imageTensorRTBindingDirs = previous, previousImage })
	factory := &ManagedFactory{Hardware: func() Hardware { return orin }}

	plan, err := factory.acceleratorPlan(context.Background(), t.TempDir(), yoloInference(""))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Candidates, []string{"cpu"}) || !plan.Degraded || !strings.Contains(plan.Reason, "could not be fetched") || plan.TensorRT != nil {
		t.Fatalf("plan %+v", plan)
	}
	// A campaign that requires TensorRT does not start on the CPU.
	if _, err := factory.acceleratorPlan(context.Background(), t.TempDir(), yoloInference("tensorrt")); err == nil || !strings.Contains(err.Error(), "accelerator tensorrt is unavailable") {
		t.Fatalf("strict campaign started: %v", err)
	}
	// The cpu accelerator never probes the hardware.
	probed := false
	factory.Hardware = func() Hardware { probed = true; return orin }
	if plan, err := factory.acceleratorPlan(context.Background(), t.TempDir(), yoloInference("cpu")); err != nil || plan.Degraded || probed {
		t.Fatalf("cpu plan %+v, probed %v, %v", plan, probed, err)
	}
}

// withTensorRTHost makes this host look like a Jetson with TensorRT 10.16.2
// whose image ships the bindings, and returns the bindings directory.
func withTensorRTHost(t *testing.T) string {
	t.Helper()
	contents := map[string]string{"tensorrt/__init__.py": "bindings\n"}
	image := t.TempDir()
	if err := os.MkdirAll(filepath.Join(image, "tensorrt"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(image, "tensorrt", "__init__.py"), []byte(contents["tensorrt/__init__.py"]), 0600); err != nil {
		t.Fatal(err)
	}
	withBinding(t, tensorRTBinding{version: "10.16.2.10", cudaMajor: "13", files: map[string]string{"tensorrt/__init__.py": digest(contents["tensorrt/__init__.py"])}})
	libraries := t.TempDir()
	for _, name := range []string{"libnvinfer.so.10.16.2", "libnvonnxparser.so.10", "libnvinfer_plugin.so.10"} {
		if err := os.WriteFile(filepath.Join(libraries, name), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	previous, previousImage := tensorRTLibraryDirs, imageTensorRTBindingDirs
	tensorRTLibraryDirs, imageTensorRTBindingDirs = []string{libraries}, []string{image}
	t.Cleanup(func() { tensorRTLibraryDirs, imageTensorRTBindingDirs = previous, previousImage })
	return image
}

func TestAcceleratorPlanForTensorRT(t *testing.T) {
	image := withTensorRTHost(t)
	root := t.TempDir()
	plan, err := (&ManagedFactory{Hardware: func() Hardware { return orin }}).acceleratorPlan(context.Background(), root, yoloInference(""))
	if err != nil {
		t.Fatal(err)
	}
	want := &TensorRTPlan{Library: "10.16.2", Bindings: image, EngineDir: filepath.Join(root, "engines"), CUDARuntime: "libcudart.so.13",
		WorkspaceMiB: 1024, BuildMinAvailableMiB: 3072, GPUBudgetMiB: 1536, BuildTimeoutSeconds: 1800}
	if !reflect.DeepEqual(plan.TensorRT, want) || !reflect.DeepEqual(plan.Candidates, []string{"tensorrt", "cpu"}) {
		t.Fatalf("plan %+v, tensorrt %+v", plan, plan.TensorRT)
	}
	// The config line carries the campaign and the plan side by side.
	line, err := json.Marshal(workerConfig{yoloInference(""), plan})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(line, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["backend"] != "yolo_onnx" || decoded["accelerator_plan"].(map[string]any)["tensorrt"].(map[string]any)["engine_dir"] != filepath.Join(root, "engines") {
		t.Fatalf("config line %s", line)
	}
}

func TestPinnedTensorRTBindings(t *testing.T) {
	for key, binding := range tensorRTBindings {
		if !strings.HasPrefix(binding.url, "https://repo.download.nvidia.com/") || len(binding.sha) != 64 {
			t.Fatalf("%s: unpinned source %q", key, binding.url)
		}
		for _, required := range []string{"tensorrt/__init__.py", "tensorrt/tensorrt.so"} {
			if len(binding.files[required]) != 64 {
				t.Fatalf("%s: %s is not pinned", key, required)
			}
		}
	}
}

func TestAcceleratorResultsDecode(t *testing.T) {
	var result Result
	line := `{"type":"accelerator","accelerator":{"requested":"auto","active":"tensorrt","engine":{"state":"ready","cached":true}},"stats":{"fps":2,"infer_ms_p50":5.5,"infer_ms_p95":7,"frames_scored":10}}`
	if err := json.Unmarshal([]byte(line), &result); err != nil {
		t.Fatal(err)
	}
	if result.Accelerator.Active != "tensorrt" || !result.Accelerator.Engine.Cached || result.Stats.InferMsP50 != 5.5 {
		t.Fatalf("%+v", result)
	}
	if !strings.Contains(strings.Join(runtimeEnvironment(t.TempDir()), "\n"), "CUDA_MODULE_LOADING=LAZY") {
		t.Fatal("worker environment lacks lazy CUDA module loading")
	}
}

// A TensorRT worker that keeps dying unasked gets no TensorRT for a while: an
// auto campaign detects on the CPU, a tensorrt campaign reports the reason.
// Before, the worker was restarted every 8 s and detected nothing.
func TestCrashingTensorRTWorkerFallsBackToTheCPU(t *testing.T) {
	withTensorRTHost(t)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	previous := timeNow
	timeNow = func() time.Time { return now }
	t.Cleanup(func() { timeNow = previous })
	factory := &ManagedFactory{Hardware: func() Hardware { return orin }}
	root := t.TempDir()
	plan := func(accelerator string) (AcceleratorPlan, error) {
		return factory.acceleratorPlan(context.Background(), root, yoloInference(accelerator))
	}
	// Real processes, wired the way Start wires a worker offered TensorRT.
	key := tensorRTCrashKey(yoloInference(""))
	crash := func(stderr string) {
		t.Helper()
		session, err := shWorker(t, context.Background(), `echo '{"type":"ready"}'; echo '`+stderr+`' >&2; exit 1`,
			func(reason string) { factory.workerCrashed(key, reason) })
		if err != nil {
			t.Fatal(err)
		}
		drain(t, session)
		session.Close()
	}

	crash("first")
	now = now.Add(time.Minute)
	crash("second")
	if p, err := plan(""); err != nil || p.TensorRT == nil {
		t.Fatalf("two exits took TensorRT away: %+v, %v", p, err)
	}
	now = now.Add(time.Minute)
	crash("Segmentation fault in libnvinfer")

	auto, err := plan("")
	if err != nil {
		t.Fatal(err)
	}
	reason := "the TensorRT worker exited 3 times within 10 minutes, so TensorRT is off for this model until 2026-10-27T13:02:00Z; " +
		"last exit: exit status 1; stderr: Segmentation fault in libnvinfer"
	if !reflect.DeepEqual(auto.Candidates, []string{"cpu"}) || !auto.Degraded || auto.Reason != reason || auto.TensorRT != nil {
		t.Fatalf("auto plan %+v", auto)
	}
	if _, err := plan("tensorrt"); err == nil || err.Error() != "accelerator tensorrt is unavailable: "+reason {
		t.Fatalf("strict plan error %v", err)
	}
	// The same at every start during the hold, so it is logged once.
	now = now.Add(30 * time.Minute)
	if again, _ := plan(""); again.Reason != reason {
		t.Fatalf("reason changed to %q", again.Reason)
	}
	// Another model is not affected.
	other := yoloInference("")
	other.Revision = strings.Repeat("b", 40)
	if p, err := factory.acceleratorPlan(context.Background(), root, other); err != nil || p.TensorRT == nil {
		t.Fatalf("another model lost TensorRT: %+v, %v", p, err)
	}
	// After the hold TensorRT is tried again, and exits count afresh.
	now = now.Add(31 * time.Minute)
	if p, err := plan(""); err != nil || p.TensorRT == nil || p.Degraded {
		t.Fatalf("TensorRT was not offered after the hold: %+v, %v", p, err)
	}
	crash("once more")
	if p, _ := plan(""); p.TensorRT == nil {
		t.Fatal("one exit after the hold took TensorRT away")
	}
}

func TestTensorRTExitsOutsideTheWindowDoNotAddUp(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	previous := timeNow
	timeNow = func() time.Time { return now }
	t.Cleanup(func() { timeNow = previous })
	factory := &ManagedFactory{}
	for range 5 {
		factory.workerCrashed("model", "exit status 1")
		now = now.Add(6 * time.Minute)
	}
	if held := factory.tensorRTHeldOff("model"); held != "" {
		t.Fatalf("exits 6 minutes apart were held: %s", held)
	}
}
