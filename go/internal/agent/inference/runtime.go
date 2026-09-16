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
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

//go:embed worker.py pyproject.toml uv.lock
var assets embed.FS

const uvVersion = "0.10.9"

var uvArtifacts = map[string]struct{ target, sha string }{
	"linux/amd64":  {"x86_64-unknown-linux-gnu", "20d79708222611fa540b5c9ed84f352bcd3937740e51aacc0f8b15b271c57594"},
	"linux/arm64":  {"aarch64-unknown-linux-gnu", "cc0c5a8573e7d6d78aecb954e0a62b5c0d18217bb81f1e19363b428c57a9962a"},
	"darwin/arm64": {"aarch64-apple-darwin", "a92f61e9ac9b0f29668c15f56152e4a60143fca148ff5bfadb86718472c3f376"},
}

func Supported() bool { _, ok := uvArtifacts[runtime.GOOS+"/"+runtime.GOARCH]; return ok }

type Input struct {
	SourceID      string `json:"source_id"`
	Generation    uint64 `json:"generation"`
	Encoding      string `json:"encoding,omitempty"`
	Payload       []byte `json:"payload,omitempty"`
	DroppedBefore uint64 `json:"dropped_before,omitempty"`
	End           bool   `json:"end,omitempty"`
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
	if _, err := os.Stat(uv); errors.Is(err, os.ErrNotExist) {
		url := "https://github.com/astral-sh/uv/releases/download/" + uvVersion + "/uv-" + artifact.target + ".tar.gz"
		if err := installUV(ctx, url, artifact.sha, artifact.target, uv); err != nil {
			return "", "", err
		}
	} else if err != nil {
		return "", "", err
	}
	hash := sha256.New()
	files := []string{"worker.py", "pyproject.toml", "uv.lock"}
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
	tr := tar.NewReader(reader)
	for {
		header, err := tr.Next()
		if err != nil {
			return fmt.Errorf("campaign runtime archive missing uv: %w", err)
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
	command := exec.CommandContext(childCtx, uv, "run", "--project", dir, "--frozen", "--no-dev", "--no-build", "--managed-python", "--python", "3.12", "python", "-u", filepath.Join(dir, "worker.py"))
	command.Dir = dir
	command.Env = append(os.Environ(), "UV_CACHE_DIR="+filepath.Join(f.Root, "cache"), "UV_PYTHON_INSTALL_DIR="+filepath.Join(f.Root, "python"), "HF_HOME="+filepath.Join(f.Root, "models"), "UV_NO_PROGRESS=1", "TOKENIZERS_PARALLELISM=false", "OMP_NUM_THREADS=2")
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
