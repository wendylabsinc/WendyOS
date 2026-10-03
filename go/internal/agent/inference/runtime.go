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
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

//go:embed worker.py pyproject.toml uv.lock
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
	Label string     `json:"label"`
	Score float64    `json:"score"`
	Box   [4]float64 `json:"box"`
}

type Result struct {
	DroppedResults uint64      `json:"dropped_results,omitempty"`
	Type           string      `json:"type"`
	SourceID       string      `json:"source_id"`
	Generation     uint64      `json:"generation"`
	Detections     []Detection `json:"detections,omitempty"`
	Error          string      `json:"error,omitempty"`
}

type Session interface {
	Send(Input) error
	Results() <-chan Result
	Close() error
}

type Factory interface {
	Start(context.Context, data.CampaignInference) (Session, error)
}

// ManagedFactory installs a checksum-pinned uv and a locked, isolated CPU
// runtime into the agent's writable data partition on first use.
type ManagedFactory struct {
	Root string
	mu   sync.Mutex
}

var assetFiles = []string{"worker.py", "pyproject.toml", "uv.lock"}

func (f *ManagedFactory) prepare(ctx context.Context) (string, string, error) {
	return f.Prepare(ctx, assets, assetFiles)
}

// Prepare installs the shared, checksum-pinned uv under Root and materialises
// files from assets into a runtime directory named by their content hash, so
// every worker shares one uv while keeping its own locked environment.
func (f *ManagedFactory) Prepare(ctx context.Context, assets fs.FS, files []string) (uv, dir string, err error) {
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
	uv = filepath.Join(root, "uv-"+uvVersion)
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
	contents := make([][]byte, len(files))
	for i, name := range files {
		if contents[i], err = fs.ReadFile(assets, name); err != nil {
			return "", "", err
		}
		hash.Write(contents[i])
	}
	dir = filepath.Join(root, "runtime-"+hex.EncodeToString(hash.Sum(nil))[:16])
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", "", err
	}
	for i, name := range files {
		b := contents[i]
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
	childCtx, cancel := context.WithCancel(ctx)
	command := BindContext(childCtx, Command(uv, dir, "worker.py"))
	if err := os.MkdirAll(filepath.Join(f.Root, "home"), 0700); err != nil {
		cancel()
		return nil, fmt.Errorf("creating model runtime home: %w", err)
	}
	command.Env = RuntimeEnvironment(f.Root)
	ConfigureProcess(command)
	stderr := &TailBuffer{}
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
		cancel()
		waitErr := command.Wait()
		if !initialized {
			ready <- fmt.Errorf("loading campaign model: %v: %s", waitErr, stderr.String())
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
}

func (s *processSession) Send(input Input) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.NewEncoder(s.stdin).Encode(input)
}
func (s *processSession) Results() <-chan Result { return s.results }
func (s *processSession) Close() error           { s.cancel(); <-s.done; return s.stdin.Close() }

// Command builds the locked, offline uv invocation that runs script from a
// runtime directory returned by Prepare. The caller sets the environment.
func Command(uv, dir, script string) *exec.Cmd {
	command := exec.Command(uv, "run", "--project", dir, "--frozen", "--no-dev", "--no-build", "--managed-python", "--python", "3.12", "python", "-u", filepath.Join(dir, script))
	command.Dir = dir
	return command
}

// BindContext rebuilds an unstarted command's path, arguments, directory and
// environment as exec.CommandContext would, so it is killed when ctx is done.
func BindContext(ctx context.Context, command *exec.Cmd) *exec.Cmd {
	bound := exec.CommandContext(ctx, command.Path)
	bound.Args, bound.Dir, bound.Env, bound.Err = command.Args, command.Dir, command.Env, command.Err
	return bound
}

// TailBuffer keeps the last 8KiB written to it, for worker diagnostics.
type TailBuffer struct {
	mu sync.Mutex
	b  []byte
}

// tailBuffer and runtimeEnvironment keep the original private names working.
type tailBuffer = TailBuffer

func runtimeEnvironment(root string) []string { return RuntimeEnvironment(root) }

func (b *TailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.b = append(b.b, p...)
	if len(b.b) > 8192 {
		b.b = b.b[len(b.b)-8192:]
	}
	return len(p), nil
}
func (b *TailBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return string(b.b) }

// RuntimeEnvironment deliberately excludes agent tokens, cloud credentials,
// Python injection settings and the agent's home/configuration directories.
func RuntimeEnvironment(root string) []string {
	env := []string{
		"HOME=" + filepath.Join(root, "home"),
		"UV_CACHE_DIR=" + filepath.Join(root, "cache"),
		"UV_PYTHON_INSTALL_DIR=" + filepath.Join(root, "python"),
		"HF_HOME=" + filepath.Join(root, "models"),
		"UV_NO_PROGRESS=1", "TOKENIZERS_PARALLELISM=false", "OMP_NUM_THREADS=2",
	}
	for _, key := range []string{"PATH", "LANG", "LC_ALL", "TMPDIR", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}
