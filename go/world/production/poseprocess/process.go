// Package poseprocess runs the image-only pose worker alongside an ordinary app.
// The agent remains responsible for publication identity and source authority.
package poseprocess

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/pose"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
)

type Config struct {
	// Command is an explicit executable and arguments. The normal deployment
	// uses python /app/perception/pose.py --model /app/model.onnx --serve.
	Command   []string
	Directory string
	Timeout   time.Duration
}

type response struct {
	raw []byte
	err error
}
type Worker struct {
	mu        sync.Mutex
	cancel    context.CancelFunc
	input     io.WriteCloser
	replies   chan response
	done      chan struct{}
	directory string
	timeout   time.Duration
	closed    bool
}

func New(ctx context.Context, cfg Config) (*Worker, error) {
	if ctx == nil || len(cfg.Command) < 1 || len(cfg.Command) > 32 || !filepath.IsAbs(cfg.Command[0]) || !filepath.IsAbs(cfg.Directory) {
		return nil, errors.New("explicit pose executable and private temporary directory required")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Second
	}
	if cfg.Timeout < time.Millisecond || cfg.Timeout > 30*time.Second {
		return nil, errors.New("invalid pose timeout")
	}
	info, err := os.Stat(cfg.Directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("pose temporary directory must be private")
	}
	run, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(run, cfg.Command[0], cfg.Command[1:]...)
	// Diagnostics never mix with the NDJSON result protocol or image bytes.
	cmd.Stderr = io.Discard
	in, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		in.Close()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		cancel()
		in.Close()
		return nil, err
	}
	w := &Worker{cancel: cancel, input: in, replies: make(chan response, 1), done: make(chan struct{}), directory: cfg.Directory, timeout: cfg.Timeout}
	go func() {
		defer func() { _ = cmd.Wait(); close(w.done) }()
		scanner := bufio.NewScanner(out)
		scanner.Buffer(make([]byte, 4096), 64<<10)
		for scanner.Scan() {
			select {
			case w.replies <- response{raw: bytes.Clone(scanner.Bytes())}:
			case <-run.Done():
				return
			}
		}
		err := scanner.Err()
		if err == nil {
			err = io.EOF
		}
		select {
		case w.replies <- response{err: err}:
		case <-run.Done():
		}
	}()
	return w, nil
}

// Infer sends one private image path. No calibration, actor identity or scene
// coordinates are exposed to the model worker. A failed call ends this worker,
// preventing an interrupted response from being consumed by a later image.
func (w *Worker) Infer(ctx context.Context, data []byte) (pose.ImageResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var value pose.ImageResult
	if w.closed || ctx == nil || len(data) == 0 || len(data) > 16<<20 {
		return value, errors.New("invalid pose request")
	}
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return value, err
	}
	f, err := os.CreateTemp(w.directory, "image-*.bin")
	if err != nil {
		return value, err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return value, err
	}
	if err = f.Close(); err != nil {
		return value, err
	}
	raw, _ := json.Marshal(map[string]string{"image_path": name})
	if _, err = w.input.Write(append(raw, '\n')); err != nil {
		w.fail()
		return value, err
	}
	var reply response
	select {
	case reply = <-w.replies:
	case <-ctx.Done():
		w.fail()
		return value, ctx.Err()
	}
	if reply.err != nil {
		w.fail()
		return value, reply.err
	}
	d := json.NewDecoder(bytes.NewReader(reply.raw))
	d.DisallowUnknownFields()
	if err = d.Decode(&value); err != nil {
		w.fail()
		return value, errors.New("invalid pose response")
	}
	if err = d.Decode(new(any)); !errors.Is(err, io.EOF) {
		w.fail()
		return value, errors.New("trailing pose response")
	}
	hash := sha256.Sum256(data)
	if value.ImageSHA256 != hex.EncodeToString(hash[:]) || value.ModelID == "" || value.ModelVersion == "" || !sensing.DigestValid(value.ModelSHA256) || value.Detections == nil || len(value.Detections) > 64 || value.ImageWidth < 1 || value.ImageHeight < 1 {
		w.fail()
		return pose.ImageResult{}, errors.New("pose result does not bind this image")
	}
	return value, nil
}

func (w *Worker) fail() { w.closed = true; w.cancel(); _ = w.input.Close() }
func (w *Worker) Close() error {
	w.mu.Lock()
	w.fail()
	w.mu.Unlock()
	select {
	case <-w.done:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("pose process did not exit")
	}
}
