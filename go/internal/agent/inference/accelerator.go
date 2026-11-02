package inference

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ulikunitz/xz"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

// Hardware is the agent's own accelerator evidence, the same probe that
// `wendy device info` reports as gpuVendor, gpuCapabilities[].computeBackends,
// cudaVersion and gpuArch.
type Hardware struct {
	GPUVendor       string
	ComputeBackends []string
	CUDAVersion     string
	GPUArch         string
}

// AcceleratorPlan is what the agent offers the model process, in the config
// line. The process tries Candidates in order and reports what it actually
// runs on; that report, never this plan, is the campaign's status.
type AcceleratorPlan struct {
	Requested  string   `json:"requested"`
	Candidates []string `json:"candidates"`
	// Degraded and Reason say why the plan holds no accelerator. Degraded is
	// set only when the device has one the campaign could have used.
	Degraded bool          `json:"degraded,omitempty"`
	Reason   string        `json:"reason,omitempty"`
	TensorRT *TensorRTPlan `json:"tensorrt,omitempty"`
}

// TensorRTPlan is everything the model process needs to find, build and load
// a TensorRT engine. The limits are the agent's, so they are set in one place.
type TensorRTPlan struct {
	// Library is the host's libnvinfer version, such as 10.16.2.
	Library string `json:"library"`
	// Bindings is the directory holding the verified tensorrt Python package.
	Bindings string `json:"bindings"`
	// EngineDir is the engine cache.
	EngineDir string `json:"engine_dir"`
	// CUDARuntime is the libcudart the engine runs on, such as libcudart.so.13.
	CUDARuntime          string `json:"cuda_runtime"`
	WorkspaceMiB         int    `json:"workspace_mib"`
	BuildMinAvailableMiB int    `json:"build_min_available_mib"`
	GPUBudgetMiB         int    `json:"gpu_budget_mib"`
	BuildTimeoutSeconds  int    `json:"build_timeout_seconds"`
}

const (
	// tensorRTWorkspaceMiB caps the builder's scratch memory. It is the
	// workspace the build below was measured with.
	tensorRTWorkspaceMiB = 1024

	// tensorRTBuildMinAvailableMiB is the MemAvailable a TensorRT engine build
	// needs before it starts; with less, the campaign stays on the CPU and the
	// build is retried later. Measured on 2026-10-27 on a Jetson Orin Nano
	// 8 GB (JetPack 7.2, TensorRT 10.16.2, sm_87): building yolo11n at FP16 with
	// a 1024 MiB workspace took about 2.2 GB of extra memory (available memory
	// fell from about 6.2 GB to about 3.9 GB) over 655 s. 3 GiB is that peak
	// plus about 800 MiB of headroom, so a build never pushes another app on
	// the device into the OOM killer. The Jetson's GPU shares this memory.
	tensorRTBuildMinAvailableMiB = 3072

	// tensorRTGPUBudgetMiB is the most the accelerated path may take: the
	// TensorRT and CUDA libraries the worker imports, the CUDA context, the
	// plan and its activations. Over it the campaign falls back to the CPU,
	// reported as degraded. The worker measures it as described above
	// memory_cost in accelerator.py. On 2026-10-27 yolo11n on a Jetson Orin
	// Nano 8 GB (JetPack 7.2, TensorRT 10.16.2) took about 900 MiB of free
	// memory, and its worker's RSS was 885 MB against 238 MB for a CPU-only
	// worker. 1.5 GiB is that plus room for other apps that allocate during
	// the measurement.
	tensorRTGPUBudgetMiB = 1536

	// tensorRTBuildTimeout ends a build that hangs. The measured build took
	// 655 s; the timeout leaves room for a slower power mode.
	tensorRTBuildTimeout = 30 * time.Minute

	// tensorRTBindingsRetry is how long a failed bindings download waits
	// before it is tried again, so an offline device or a bad mirror costs
	// one request per interval rather than one per worker start.
	tensorRTBindingsRetry = 10 * time.Minute

	// A model whose TensorRT worker exits tensorRTCrashLimit times within
	// tensorRTCrashWindow, without being asked to and without reporting an
	// error of its own, gets no TensorRT for tensorRTCrashHold: an auto
	// campaign runs on the CPU, reported as degraded with the last exit's
	// stderr, and a tensorrt campaign reports that as its error. Before, a
	// worker that TensorRT crashed after every reboot was restarted every 8 s
	// and detected nothing at all. After the hold, TensorRT is tried again.
	tensorRTCrashLimit  = 3
	tensorRTCrashWindow = 10 * time.Minute
	tensorRTCrashHold   = time.Hour
)

// timeNow is behind a var for tests.
var timeNow = time.Now

// tensorRTCrashes are one model's recent unasked exits of TensorRT workers.
type tensorRTCrashes struct {
	exits []time.Time
	// last is the last exit's reason, with the tail of its stderr.
	last string
	// until is when TensorRT is offered again, after held exits.
	until time.Time
	held  int
}

// tensorRTCrashKey names what a crash is held against: the model, and so
// the engine. Labels, rate and threshold do not change the engine.
func tensorRTCrashKey(config data.CampaignInference) string {
	return config.Backend + "\x00" + config.Model + "\x00" + config.Revision + "\x00" + config.ModelFile
}

// workerCrashed records an unasked exit of a worker offered TensorRT.
func (f *ManagedFactory) workerCrashed(key, reason string) {
	now := timeNow()
	f.crashMu.Lock()
	defer f.crashMu.Unlock()
	if f.crashes == nil {
		f.crashes = map[string]*tensorRTCrashes{}
	}
	record := f.crashes[key]
	if record == nil {
		record = &tensorRTCrashes{}
		f.crashes[key] = record
	}
	recent := record.exits[:0]
	for _, at := range record.exits {
		if now.Sub(at) < tensorRTCrashWindow {
			recent = append(recent, at)
		}
	}
	record.exits, record.last = append(recent, now), reason
	if len(record.exits) >= tensorRTCrashLimit {
		record.until, record.held, record.exits = now.Add(tensorRTCrashHold), len(record.exits), nil
	}
}

// tensorRTHeldOff says why a model gets no TensorRT now, or "". The reason
// is the same at every start during the hold, so the agent logs it once.
func (f *ManagedFactory) tensorRTHeldOff(key string) string {
	f.crashMu.Lock()
	defer f.crashMu.Unlock()
	record := f.crashes[key]
	if record == nil || !timeNow().Before(record.until) {
		return ""
	}
	return fmt.Sprintf("the TensorRT worker exited %d times within %d minutes, so TensorRT is off for this model until %s; last exit: %s",
		record.held, int(tensorRTCrashWindow/time.Minute), record.until.UTC().Format(time.RFC3339), record.last)
}

// tensorRTBinding pins NVIDIA's Python bindings for one TensorRT version.
// They are a 846 KB Debian package for exactly the TensorRT in the image and
// exactly the Python minor the worker runs (3.12). The libraries themselves
// always come from the image: the TensorRT libraries wheel on PyPI is 2.5 GB.
type tensorRTBinding struct {
	version   string // the bindings' own version
	cudaMajor string // the CUDA major they are built for
	url       string
	sha       string // of the package
	// files is the package's tensorrt/ tree, path to SHA-256. A cached copy
	// is checked against these on every start: a writable sidecar checksum
	// would not authenticate it.
	files map[string]string
}

// tensorRTBindings is keyed by GOOS/GOARCH/libnvinfer version. A host whose
// TensorRT has no row runs on the CPU and says the agent needs an update.
var tensorRTBindings = map[string]tensorRTBinding{
	"linux/arm64/10.16.2": {
		version:   "10.16.2.10",
		cudaMajor: "13",
		url:       "https://repo.download.nvidia.com/jetson/common/pool/main/t/tensorrt/python3-libnvinfer_10.16.2.10-1+cuda13.2_arm64.deb",
		sha:       "da1282da12166871f731bb3f058f81b76c4ef22036668111723b0116ba2cf216",
		files: map[string]string{
			"tensorrt/__init__.py":             "52c598e996e8480532b785dfbfcdb3f1a418d3dde5b9d410cf8dceaebc0c45f2",
			"tensorrt/tensorrt.so":             "dabdbe9dbdd9f91af55693d3840c040ea59f50c39c2ca3862eee01eb23b45219",
			"tensorrt/plugin/__init__.py":      "7b0feb812aa309c90d96e8e04c903410b3e64b473b67288d96785b4b522d0b09",
			"tensorrt/plugin/_autotune.py":     "218d49d742b1be93ccddc135d19fd5a7b31e6d9c86cbe05f286b8b4b9e75b1fc",
			"tensorrt/plugin/_export.py":       "caf9fb6bfb4c9bef84c2c09a1380e999d919b84c9d09be86e1174faf750c31f6",
			"tensorrt/plugin/_lib.py":          "8dc0a2c70d34cd427b4e0207f21ba15e4c115dba730791595cb91bb48c7c3246",
			"tensorrt/plugin/_plugin_class.py": "e768d2bc4f8adb0c8973a008f4db930f3a1b7c8061130e3570e86d3afac5bafa",
			"tensorrt/plugin/_tensor.py":       "c7f0450df930dbcaf8e63aad15d4310874c907ae7aa7a8cadf729056e4feb8ef",
			"tensorrt/plugin/_top_level.py":    "0f4bc60a377d3d46feb2aa30cd0e24636987d2fe411e974553003f3ddfa3e736",
			"tensorrt/plugin/_utils.py":        "0b53221a64a01311b28b54cc9381ec36a56b984529037098f94ae7d6c751276d",
			"tensorrt/plugin/_validate.py":     "9b0ac6299360a185f15713c2eddcbe308f10213a4c5737bb12a7bfa2b33eceac",
		},
	},
}

// The package's files live here inside its data archive.
const debPythonPackages = "usr/lib/python3.12/dist-packages/"

// imageTensorRTBindingDirs is where an image may ship the same bindings, so
// that first setup downloads nothing for the GPU. A directory is used only
// when its tensorrt/ tree matches the pinned hashes. Behind a var for tests.
var imageTensorRTBindingDirs = []string{"/usr/lib/wendy-inference/python3.12"}

// tensorRTLibraryDirs are searched for the host's libnvinfer. Behind a var for
// tests.
var tensorRTLibraryDirs = []string{"/usr/lib", "/usr/lib/aarch64-linux-gnu", "/usr/lib/x86_64-linux-gnu", "/usr/lib64", "/usr/local/lib"}

var libnvinferRE = regexp.MustCompile(`^libnvinfer\.so\.(\d+)\.(\d+)\.(\d+)$`)

// hostTensorRTVersion is the libnvinfer version installed on the host, such
// as 10.16.2, or "" when TensorRT, its ONNX parser or its plugins are missing.
func hostTensorRTVersion() string {
	for _, dir := range tensorRTLibraryDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			m := libnvinferRE.FindStringSubmatch(entry.Name())
			if m == nil {
				continue
			}
			// The bindings link all three, at the same major.
			if !exists(filepath.Join(dir, "libnvonnxparser.so."+m[1])) || !exists(filepath.Join(dir, "libnvinfer_plugin.so."+m[1])) {
				continue
			}
			return m[1] + "." + m[2] + "." + m[3]
		}
	}
	return ""
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// planAccelerator chooses the engines to offer. The binding is non-nil when
// TensorRT is offered and its bindings still have to be found or fetched.
func planAccelerator(config data.CampaignInference, hardware Hardware, library string) (AcceleratorPlan, *tensorRTBinding) {
	requested := config.EffectiveAccelerator()
	plan := AcceleratorPlan{Requested: requested, Candidates: []string{data.AcceleratorCPU}}
	gpu := hardware.GPUVendor == "nvidia" && slices.Contains(hardware.ComputeBackends, "cuda")
	switch {
	case requested == data.AcceleratorCPU:
		plan.Reason = "the campaign asks for the cpu"
	case config.Backend != "yolo_onnx":
		plan.Reason = "only backend yolo_onnx can use an accelerator"
	case !gpu:
		vendor := hardware.GPUVendor
		if vendor == "" {
			vendor = "none"
		}
		plan.Reason = fmt.Sprintf("no supported accelerator: TensorRT needs an NVIDIA GPU with CUDA (GPU vendor %s, compute backends %v)", vendor, hardware.ComputeBackends)
	case library == "":
		plan.Reason = "this device has an NVIDIA GPU but no TensorRT libraries"
	default:
		binding, ok := tensorRTBindings[runtime.GOOS+"/"+runtime.GOARCH+"/"+library]
		if !ok {
			// The device could run TensorRT; this agent does not know its bindings.
			plan.Degraded = true
			plan.Reason = fmt.Sprintf("this agent knows no TensorRT Python bindings for libnvinfer %s on %s/%s; update the agent", library, runtime.GOOS, runtime.GOARCH)
			break
		}
		if major, _, _ := strings.Cut(hardware.CUDAVersion, "."); major != "" && major != binding.cudaMajor {
			plan.Degraded = true
			plan.Reason = fmt.Sprintf("the TensorRT bindings for libnvinfer %s need CUDA %s, and this device has CUDA %s", library, binding.cudaMajor, hardware.CUDAVersion)
			break
		}
		plan.Candidates = []string{data.AcceleratorTensorRT, data.AcceleratorCPU}
		if requested == data.AcceleratorTensorRT {
			plan.Candidates = []string{data.AcceleratorTensorRT}
		}
		return plan, &binding
	}
	return plan, nil
}

// acceleratorPlan probes the hardware, plans, and finds or fetches the
// TensorRT bindings. It fails only for a tensorrt campaign that cannot have
// TensorRT; an auto campaign falls back to the CPU and says why.
func (f *ManagedFactory) acceleratorPlan(ctx context.Context, root string, config data.CampaignInference) (AcceleratorPlan, error) {
	var hardware Hardware
	if f.Hardware != nil && config.EffectiveAccelerator() != data.AcceleratorCPU && config.Backend == "yolo_onnx" {
		hardware = f.Hardware()
	}
	library := ""
	if hardware.GPUVendor == "nvidia" {
		library = hostTensorRTVersion()
	}
	plan, binding := planAccelerator(config, hardware, library)
	if binding != nil {
		if held := f.tensorRTHeldOff(tensorRTCrashKey(config)); held != "" {
			plan.Candidates, plan.Degraded, plan.Reason = []string{data.AcceleratorCPU}, true, held
			binding = nil
		}
	}
	if binding != nil {
		bindings, err := f.tensorRTBindings(ctx, root, *binding)
		if err != nil {
			plan.Candidates = []string{data.AcceleratorCPU}
			plan.Degraded = true
			plan.Reason = fmt.Sprintf("the TensorRT Python bindings %s are not on this device and could not be fetched (%v)", binding.version, err)
		} else {
			cudaRuntime := "libcudart.so"
			if major, _, _ := strings.Cut(hardware.CUDAVersion, "."); major != "" {
				cudaRuntime += "." + major
			}
			plan.TensorRT = &TensorRTPlan{
				Library: library, Bindings: bindings, EngineDir: filepath.Join(root, "engines"), CUDARuntime: cudaRuntime,
				WorkspaceMiB: tensorRTWorkspaceMiB, BuildMinAvailableMiB: tensorRTBuildMinAvailableMiB,
				GPUBudgetMiB: tensorRTGPUBudgetMiB, BuildTimeoutSeconds: int(tensorRTBuildTimeout / time.Second),
			}
		}
	}
	if plan.Requested == data.AcceleratorTensorRT && plan.TensorRT == nil {
		return plan, fmt.Errorf("accelerator tensorrt is unavailable: %s", plan.Reason)
	}
	return plan, nil
}

// tensorRTBindings returns a directory holding the verified tensorrt package:
// the image's copy when it has one (the seam for shipping them in the image),
// else the agent's cached copy, else one downloaded now. It downloads at most
// once per tensorRTBindingsRetry after a failure.
func (f *ManagedFactory) tensorRTBindings(ctx context.Context, root string, binding tensorRTBinding) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, dir := range imageTensorRTBindingDirs {
		if validTensorRTBindings(dir, binding.files) == nil {
			return dir, nil
		}
	}
	dir := filepath.Join(root, "accel", "tensorrt-"+binding.version)
	if validTensorRTBindings(dir, binding.files) == nil {
		return dir, nil
	}
	if f.bindingsFailure != nil && time.Since(f.bindingsFailedAt) < tensorRTBindingsRetry {
		return "", f.bindingsFailure
	}
	if err := installTensorRTBindings(ctx, binding.url, binding.sha, binding.files, dir); err != nil {
		f.bindingsFailure, f.bindingsFailedAt = err, time.Now()
		return "", err
	}
	f.bindingsFailure = nil
	return dir, nil
}

// validTensorRTBindings checks every pinned file: a regular file, no symlink,
// with the pinned hash.
func validTensorRTBindings(dir string, files map[string]string) error {
	for name, checksum := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > 64<<20 {
			return fmt.Errorf("%s is not a regular file of at most 64 MiB", name)
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, err = io.Copy(hash, io.LimitReader(file, 64<<20))
		file.Close()
		if err != nil {
			return err
		}
		if hex.EncodeToString(hash.Sum(nil)) != checksum {
			return fmt.Errorf("%s does not match its pinned checksum", name)
		}
	}
	return nil
}

// installTensorRTBindings downloads the pinned package, checks it, and
// extracts exactly the pinned files of its tensorrt/ tree, each checked
// against its own hash, into destination. Nothing else in the package is
// written, and nothing in it runs.
func installTensorRTBindings(ctx context.Context, url, checksum string, files map[string]string, destination string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("downloading TensorRT bindings: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading TensorRT bindings: HTTP %d", response.StatusCode)
	}
	archive, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("downloading TensorRT bindings: %w", err)
	}
	digest := sha256.Sum256(archive)
	if hex.EncodeToString(digest[:]) != checksum {
		return errors.New("TensorRT bindings checksum mismatch")
	}
	contents, err := extractDebFiles(archive, files)
	if err != nil {
		return err
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".tensorrt-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for name, content := range contents {
		path := filepath.Join(tmp, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(path, content, 0600); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(destination); err != nil {
		return err
	}
	return os.Rename(tmp, destination)
}

// extractDebFiles reads a Debian package (an ar archive holding an
// xz-compressed data tarball) and returns the pinned files, by their path
// below debPythonPackages.
func extractDebFiles(deb []byte, files map[string]string) (map[string][]byte, error) {
	const magic = "!<arch>\n"
	if !bytes.HasPrefix(deb, []byte(magic)) {
		return nil, errors.New("TensorRT bindings are not a Debian package")
	}
	var payload []byte
	for offset := len(magic); offset+60 <= len(deb); {
		header := deb[offset : offset+60]
		if string(header[58:60]) != "`\n" {
			return nil, errors.New("TensorRT bindings package is malformed")
		}
		size, err := strconv.Atoi(strings.TrimSpace(string(header[48:58])))
		if err != nil || size < 0 || offset+60+size > len(deb) {
			return nil, errors.New("TensorRT bindings package is malformed")
		}
		name := strings.TrimSuffix(strings.TrimSpace(string(header[:16])), "/")
		if strings.HasPrefix(name, "data.tar") {
			if name != "data.tar.xz" {
				return nil, fmt.Errorf("TensorRT bindings package uses unsupported %s", name)
			}
			payload = deb[offset+60 : offset+60+size]
			break
		}
		offset += 60 + size + size%2
	}
	if payload == nil {
		return nil, errors.New("TensorRT bindings package has no data archive")
	}
	decompressed, err := xz.NewReader(bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("TensorRT bindings package: %w", err)
	}
	expanded := &io.LimitedReader{R: decompressed, N: (128 << 20) + 1}
	tr := tar.NewReader(expanded)
	contents := map[string][]byte{}
	for {
		header, err := tr.Next()
		if expanded.N <= 0 {
			return nil, errors.New("TensorRT bindings package exceeds 128MiB decompressed limit")
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("TensorRT bindings package: %w", err)
		}
		name, ok := strings.CutPrefix(path.Clean(strings.TrimPrefix(header.Name, "./")), debPythonPackages)
		if !ok {
			continue
		}
		checksum, pinned := files[name]
		if !pinned {
			continue
		}
		if header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > 64<<20 {
			return nil, fmt.Errorf("TensorRT bindings package has an invalid %s", name)
		}
		content, err := io.ReadAll(io.LimitReader(tr, header.Size))
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(content)
		if hex.EncodeToString(digest[:]) != checksum {
			return nil, fmt.Errorf("TensorRT bindings file %s does not match its pinned checksum", name)
		}
		contents[name] = content
	}
	for name := range files {
		if _, ok := contents[name]; !ok {
			return nil, fmt.Errorf("TensorRT bindings package lacks %s", name)
		}
	}
	return contents, nil
}
