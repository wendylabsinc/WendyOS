// Package inference runs the agent-owned Hugging Face backend. Users deploy
// campaign YAML; they never install Python or provide an executable.
package inference

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

//go:embed worker.py accelerator.py pyproject.toml uv.lock
var assets embed.FS

const uvVersion = "0.10.9"

var uvArtifacts = map[string]struct{ target, sha, binarySHA string }{
	"linux/amd64":  {"x86_64-unknown-linux-gnu", "20d79708222611fa540b5c9ed84f352bcd3937740e51aacc0f8b15b271c57594", "8f8aa2a27b00bf3b35880b2e943bb8fd58714abe0981f8467b90e75faab41131"},
	"linux/arm64":  {"aarch64-unknown-linux-gnu", "cc0c5a8573e7d6d78aecb954e0a62b5c0d18217bb81f1e19363b428c57a9962a", "2452f3680578ab0e1bee5e035dcac2486445770ac4ccc98cefc743c5740c352f"},
	"darwin/arm64": {"aarch64-apple-darwin", "a92f61e9ac9b0f29668c15f56152e4a60143fca148ff5bfadb86718472c3f376", "bc50ab0e90f24491f0e794f5b8649722f8fd2bf483c53490c012b41b89151ef9"},
}

func Supported() bool { _, ok := uvArtifacts[runtime.GOOS+"/"+runtime.GOARCH]; return ok }

type Input struct {
	SourceID       string `json:"source_id"`
	Generation     uint64 `json:"generation"`
	Encoding       string `json:"encoding,omitempty"`
	Initialization []byte `json:"initialization,omitempty"`
	Payload        []byte `json:"payload,omitempty"`
	DroppedBefore  uint64 `json:"dropped_before,omitempty"`
	End            bool   `json:"end,omitempty"`
}

type Detection struct {
	Label string  `json:"label"`
	Score float64 `json:"score"`
	// Box is x1, y1, x2, y2: the top-left and bottom-right corners, in pixels
	// of the scored picture (Result.Width by Result.Height).
	Box [4]float64 `json:"box"`
}

type Result struct {
	DroppedResults uint64      `json:"dropped_results,omitempty"`
	Type           string      `json:"type"`
	SourceID       string      `json:"source_id"`
	Generation     uint64      `json:"generation"`
	Detections     []Detection `json:"detections,omitempty"`
	// Width and Height are the scored picture's size in pixels, the frame of
	// the detection boxes.
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	Error  string `json:"error,omitempty"`
	// Engine, Precision and InferMs say what scored a prediction: cpu or
	// tensorrt, at fp32 or fp16, taking this long.
	Engine    string  `json:"engine,omitempty"`
	Precision string  `json:"precision,omitempty"`
	InferMs   float64 `json:"infer_ms,omitempty"`
	// Accelerator and Stats are the process's own report of what it runs on,
	// on ready and on accelerator results. A ready result reaches Results as
	// an accelerator result.
	Accelerator *data.AcceleratorStatus `json:"accelerator,omitempty"`
	Stats       *data.InferenceStats    `json:"stats,omitempty"`
}

// workerConfig is the process's config line: the campaign's inference block
// and the accelerators the agent offers.
type workerConfig struct {
	data.CampaignInference
	AcceleratorPlan AcceleratorPlan `json:"accelerator_plan"`
}

type Session interface {
	Send(Input) error
	Results() <-chan Result
	// Err says why the process ended, with the tail of its stderr. It is
	// set once Results is closed, and nil when the process was asked to stop.
	Err() error
	Close() error
}

type Factory interface {
	Start(context.Context, data.CampaignInference) (Session, error)
}

// ManagedFactory installs a checksum-pinned uv and a locked, isolated CPU
// runtime into the agent's writable data partition on first use. On a device
// whose Hardware reports an NVIDIA GPU with CUDA and that has the TensorRT
// libraries, it also fetches the pinned TensorRT Python bindings once.
type ManagedFactory struct {
	Root string
	// Hardware probes the device's accelerators; nil means none.
	Hardware func() Hardware
	mu       sync.Mutex

	bindingsFailure  error
	bindingsFailedAt time.Time

	// crashes are the recent unasked exits of workers offered TensorRT, by
	// model. Separate from mu, which a download can hold for minutes.
	crashMu sync.Mutex
	crashes map[string]*tensorRTCrashes
}

func (f *ManagedFactory) prepare(ctx context.Context) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	artifact, ok := uvArtifacts[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return "", "", fmt.Errorf("campaign inference is unsupported on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	root, err := filepath.Abs(f.Root)
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", "", err
	}
	uv := filepath.Join(root, "uv-"+uvVersion)
	valid, err := validCachedUV(uv, artifact.binarySHA)
	if err != nil {
		return "", "", err
	}
	if !valid {
		url := "https://github.com/astral-sh/uv/releases/download/" + uvVersion + "/uv-" + artifact.target + ".tar.gz"
		if err := installUV(ctx, url, artifact.sha, artifact.target, uv); err != nil {
			return "", "", err
		}
	}

	hash := sha256.New()
	files := []string{"worker.py", "accelerator.py", "pyproject.toml", "uv.lock"}
	for _, name := range files {
		b, _ := assets.ReadFile(name)
		hash.Write(b)
	}
	dir := filepath.Join(root, "runtime-"+hex.EncodeToString(hash.Sum(nil))[:16])
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", "", err
	}
	for _, name := range files {
		b, _ := assets.ReadFile(name)
		path := filepath.Join(dir, name)
		existing, err := os.ReadFile(path)
		if err == nil && bytes.Equal(existing, b) {
			continue
		}
		tmp, err := os.CreateTemp(dir, ".asset-*")
		if err != nil {
			return "", "", err
		}
		_, writeErr := tmp.Write(b)
		closeErr := tmp.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			os.Remove(tmp.Name())
			return "", "", err
		}
		if err := os.Rename(tmp.Name(), path); err != nil {
			os.Remove(tmp.Name())
			return "", "", err
		}
	}
	return uv, dir, nil
}

// Binary digests are derived from the corresponding checksum-verified release
// archives. A writable sidecar checksum would not authenticate cached contents.
func validCachedUV(path, checksum string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Size() > 128<<20 {
		return false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, (128<<20)+1)); err != nil {
		return false, err
	}
	return hex.EncodeToString(hash.Sum(nil)) == checksum, nil
}

func installUV(ctx context.Context, url, checksum, target, destination string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("downloading campaign runtime: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading campaign runtime: HTTP %d", response.StatusCode)
	}
	archive, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return err
	}
	digest := sha256.Sum256(archive)
	if hex.EncodeToString(digest[:]) != checksum {
		return errors.New("campaign runtime checksum mismatch")
	}
	reader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	defer reader.Close()
	expanded := &io.LimitedReader{R: reader, N: (256 << 20) + 1}
	tr := tar.NewReader(expanded)
	for {
		header, err := tr.Next()
		if expanded.N <= 0 {
			return errors.New("campaign runtime archive exceeds 256MiB decompressed limit")
		}
		if err != nil {
			return fmt.Errorf("campaign runtime archive missing uv: %w", err)
		}
		if header.Size >= expanded.N {
			return errors.New("campaign runtime archive exceeds 256MiB decompressed limit")
		}
		if header.Name != "uv-"+target+"/uv" {
			continue
		}
		if header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > 128<<20 {
			return errors.New("invalid uv executable in campaign runtime archive")
		}
		tmp, err := os.CreateTemp(filepath.Dir(destination), ".uv-*")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		_, copyErr := io.CopyN(tmp, tr, header.Size)
		chmodErr := tmp.Chmod(0700)
		closeErr := tmp.Close()
		if err := errors.Join(copyErr, chmodErr, closeErr); err != nil {
			return err
		}
		return os.Rename(tmp.Name(), destination)
	}
}

func (f *ManagedFactory) Start(ctx context.Context, config data.CampaignInference) (Session, error) {
	uv, dir, err := f.prepare(ctx)
	if err != nil {
		return nil, err
	}
	root, err := filepath.Abs(f.Root)
	if err != nil {
		return nil, err
	}
	plan, err := f.acceleratorPlan(ctx, root, config)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, "home"), 0700); err != nil {
		return nil, fmt.Errorf("creating model runtime home: %w", err)
	}
	var crashed func(string)
	if plan.TensorRT != nil {
		// A worker offered TensorRT that keeps dying loses it (accelerator.go).
		key := tensorRTCrashKey(config)
		crashed = func(reason string) { f.workerCrashed(key, reason) }
	}
	argv := []string{uv, "run", "--project", dir, "--frozen", "--no-dev", "--no-build", "--managed-python", "--python", "3.12", "python", "-u", filepath.Join(dir, "worker.py")}
	return startWorker(ctx, argv, dir, runtimeEnvironment(root), workerConfig{config, plan}, crashed)
}

// The end of a worker's stderr that a status reason and a log line carry: a
// Python traceback's last lines name the exception.
const (
	stderrTailLines = 8
	stderrTailBytes = 1024
)

// startWorker runs the model process, sends it its config line and waits for
// it to be ready. crashed, when set, is told why the process ended if it ended
// without being asked to and without reporting an error of its own.
func startWorker(ctx context.Context, argv []string, dir string, env []string, config workerConfig, crashed func(reason string)) (Session, error) {
	childCtx, cancel := context.WithCancel(ctx)
	command := exec.CommandContext(childCtx, argv[0], argv[1:]...)
	command.Dir = dir
	command.Env = env
	configureProcess(command)
	stderr := &tailBuffer{}
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
		// loadError is the worker's own reason it could not load the model,
		// such as a model that is not on the device while the network is down.
		loadError := ""
		// reported is set when the worker sent an error of its own, which
		// explains its exit. violation describes a line that broke the
		// protocol, which ends the worker.
		reported, violation := false, ""
		var dropped uint64
		for scanner.Scan() {
			var result Result
			if err := json.Unmarshal(scanner.Bytes(), &result); err != nil {
				violation = fmt.Sprintf("it wrote a line to stdout that is not protocol JSON: %q", truncate(scanner.Text(), 300))
				cancel()
				break
			}
			if result.Type == "error" {
				reported = true
			}
			if !initialized {
				if result.Type != "ready" {
					if result.Type == "error" {
						loadError = result.Error
					}
					cancel()
					break
				}
				initialized = true
				ready <- nil
				if result.Accelerator != nil {
					// The results channel is empty here, so this never blocks.
					result.Type = "accelerator"
					select {
					case session.results <- result:
					case <-childCtx.Done():
					}
				}
				continue
			}
			result.DroppedResults = dropped
			select {
			case session.results <- result:
			case <-childCtx.Done():
			default:
				// Inference is best effort: discard the oldest pending result rather
				// than deadlocking camera teardown behind a full stdout pipe.
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
		if violation == "" && scanner.Err() != nil {
			violation = fmt.Sprintf("its stdout failed: %v", scanner.Err())
		}
		// Whether the process was asked to stop, by its campaign or by Close.
		// The cancel below, or one for a protocol violation, is not a request.
		asked := ctx.Err() != nil || session.closing.Load()
		cancel()
		waitErr := command.Wait()
		// What ended the process. After a protocol violation the agent killed
		// it, so the violation is the reason, not the signal.
		exit := violation
		if exit == "" && waitErr != nil {
			exit = waitErr.Error()
		} else if exit == "" {
			exit = "exit status 0"
		}
		if tail := stderr.Tail(stderrTailLines, stderrTailBytes); tail != "" {
			exit += "; stderr: " + tail
		}
		if !initialized {
			if loadError != "" {
				ready <- fmt.Errorf("loading campaign model: %s", loadError)
			} else if violation != "" {
				ready <- fmt.Errorf("loading campaign model: %s", exit)
			} else {
				ready <- fmt.Errorf("loading campaign model: %v: %s", waitErr, stderr.String())
			}
		} else if !asked {
			// Before, an exit after ready said only "model process exited",
			// and its stderr was lost.
			session.setErr(fmt.Errorf("model process exited: %s", exit))
		}
		if crashed != nil && !asked && !reported {
			crashed(exit)
		}
	}()
	if err := json.NewEncoder(stdin).Encode(config); err != nil {
		session.Close()
		return nil, err
	}
	timer := time.NewTimer(15 * time.Minute)
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
		return nil, errors.New("campaign model runtime did not become ready within 15 minutes")
	}
}

type processSession struct {
	mu      sync.Mutex
	stdin   io.WriteCloser
	cancel  context.CancelFunc
	results chan Result
	done    chan struct{}
	closing atomic.Bool

	errMu sync.Mutex
	err   error
}

func (s *processSession) Send(input Input) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.NewEncoder(s.stdin).Encode(input)
}
func (s *processSession) Results() <-chan Result { return s.results }
func (s *processSession) Err() error             { s.errMu.Lock(); defer s.errMu.Unlock(); return s.err }
func (s *processSession) setErr(err error)       { s.errMu.Lock(); s.err = err; s.errMu.Unlock() }
func (s *processSession) Close() error {
	s.closing.Store(true)
	s.cancel()
	<-s.done
	return s.stdin.Close()
}

type tailBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.b = append(b.b, p...)
	if len(b.b) > 8192 {
		b.b = b.b[len(b.b)-8192:]
	}
	return len(p), nil
}
func (b *tailBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return string(b.b) }

// Tail is the end of what the process wrote, on one line for a status reason
// or a log line: its last nonblank lines, joined by " | ", at most maxBytes.
func (b *tailBuffer) Tail(maxLines, maxBytes int) string {
	var lines []string
	for _, line := range strings.Split(b.String(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	tail := strings.Join(lines[max(0, len(lines)-maxLines):], " | ")
	if len(tail) > maxBytes {
		tail = "..." + tail[len(tail)-maxBytes:]
	}
	return strings.ToValidUTF8(tail, "")
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return strings.ToValidUTF8(text[:limit], "") + "..."
}

// cudaCacheMaxBytes bounds CUDA's compile cache in the inference directory:
// three times what TensorRT used on a Jetson Orin Nano.
const cudaCacheMaxBytes = 512 << 20

// runtimeEnvironment deliberately excludes agent tokens, cloud credentials,
// Python injection settings and the agent's home/configuration directories.
func runtimeEnvironment(root string) []string {
	env := []string{
		"HOME=" + filepath.Join(root, "home"),
		"UV_CACHE_DIR=" + filepath.Join(root, "cache"),
		"UV_PYTHON_INSTALL_DIR=" + filepath.Join(root, "python"),
		"HF_HOME=" + filepath.Join(root, "models"),
		"UV_NO_PROGRESS=1", "TOKENIZERS_PARALLELISM=false", "OMP_NUM_THREADS=2",
		// CUDA loads kernels when first used rather than all at start, which
		// keeps a TensorRT context small. Inert without CUDA.
		"CUDA_MODULE_LOADING=LAZY",
		// CUDA compiles the GPU code it has no binary for and caches what it
		// compiled, so that the next start does not compile it again: about
		// 172 MB with TensorRT on a Jetson Orin Nano. Without these it went to
		// HOME/.nv/ComputeCache, unbounded. Past the bound CUDA evicts the
		// oldest entries. Inert without CUDA.
		"CUDA_CACHE_PATH=" + filepath.Join(root, "cuda-cache"),
		"CUDA_CACHE_MAXSIZE=" + strconv.Itoa(cudaCacheMaxBytes),
	}
	for _, key := range []string{"PATH", "LANG", "LC_ALL", "TMPDIR", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}
