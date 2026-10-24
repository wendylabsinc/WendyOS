package backend

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/internal/cli/modelhub"
)

type ModelRunSnapshot struct {
	ID                     uint64
	Port                   uint32
	Model, Progress, Error string
	Done                   bool
}
type ModelRun struct {
	mu       sync.Mutex
	cancel   context.CancelFunc
	snapshot ModelRunSnapshot
}

func (b *Backend) ModelHardware(session uint64) (modelhub.Hardware, error) {
	b.mu.Lock()
	if session == 0 {
		b.mu.Unlock()
		return modelhub.Hardware{}, errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		b.mu.Unlock()
		return modelhub.Hardware{}, err
	}
	ctx, cancel := context.WithTimeout(b.life, 15*time.Second)
	b.mu.Unlock()
	defer cancel()
	return commands.DesktopModelHardware(ctx, conn), nil
}

func (b *Backend) StartModelRun(session, job uint64, port, accelerator uint32, repo, file, revision string, install, acceptCapacityWarning bool) error {
	model, err := modelhub.ModelName(repo, file)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if session == 0 || job == 0 {
		return errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return err
	}
	if install {
		port = 11434
	}
	transport, err := deviceChatTransport(conn, port)
	if err != nil {
		return err
	}
	transport.ResponseHeaderTimeout = 10 * time.Minute
	if accelerator > 3 {
		return errors.New("unsupported accelerator")
	}
	if b.modelRun != nil {
		b.modelRun.mu.Lock()
		done := b.modelRun.snapshot.Done
		b.modelRun.mu.Unlock()
		if !done {
			return errors.New("a model is already being prepared")
		}
	}
	ctx, cancel := context.WithTimeout(b.life, 2*time.Hour)
	run := &ModelRun{cancel: cancel, snapshot: ModelRunSnapshot{ID: job, Model: model, Port: port, Progress: "Checking model files"}}
	b.modelRun = run
	go func() {
		defer cancel()
		defer func() { transport.CloseIdleConnections() }()
		progress := func(s string) { run.mu.Lock(); run.snapshot.Progress = s; run.mu.Unlock() }
		work := func() error {
			details, err := modelhub.New().Details(ctx, repo)
			if err != nil {
				return err
			}
			if details.Revision != revision {
				return errors.New("the model changed on Hugging Face; reopen it to review the current download")
			}
			found := false
			var weights int64
			for _, f := range details.Files {
				if f.Name == file {
					found = true
					weights = f.Bytes
				}
			}
			if !found {
				return errors.New("the selected model file is no longer available")
			}
			hardware := commands.DesktopModelHardware(ctx, conn)
			if install && (hardware.OS != "linux" || (hardware.Architecture != "arm64" && hardware.Architecture != "aarch64" && hardware.Architecture != "amd64" && hardware.Architecture != "x86_64")) {
				return errors.New("automatic runtime installation requires a Linux arm64 or x86-64 device")
			}
			// Re-detect before checking capacity and choosing a runtime. Older UI
			// clients cannot force CPU on a device with supported GPU acceleration.
			accelerator = hardware.Accelerator()
			if hardware.Fit(weights, accelerator, install) != 1 && !acceptCapacityWarning {
				return errors.New("available hardware capacity changed or is unknown; review the model's memory and storage warning before running")
			}
			if install {
				progress("Choosing an available model runtime port")
				probePort := func(parent context.Context, candidate uint32) (bool, error) {
					probe, cancel := context.WithTimeout(parent, 3*time.Second)
					defer cancel()
					dial, e := deviceChatTransport(conn, candidate)
					if e != nil {
						return false, e
					}
					defer dial.CloseIdleConnections()
					socket, e := dial.DialContext(probe, "tcp", "wendy-device:80")
					if e == nil {
						socket.Close()
						return true, nil
					}
					if strings.Contains(strings.ToLower(e.Error()), "connection refused") {
						return false, nil
					}
					return false, fmt.Errorf("could not check port %d: %w", candidate, e)
				}
				chosen, e := commands.DesktopSelectModelPort(ctx, conn, accelerator, probePort)
				if e != nil {
					return e
				}
				port = chosen
				transport.CloseIdleConnections()
				transport, e = deviceChatTransport(conn, port)
				if e != nil {
					return e
				}
				transport.ResponseHeaderTimeout = 10 * time.Minute
				run.mu.Lock()
				run.snapshot.Port = port
				run.mu.Unlock()
				occupied := func() bool {
					probe, c := context.WithTimeout(ctx, 3*time.Second)
					defer c()
					socket, e := transport.DialContext(probe, "tcp", "wendy-device:80")
					if e != nil {
						return !strings.Contains(strings.ToLower(e.Error()), "connection refused")
					}
					socket.Close()
					return true
				}
				if err = commands.DesktopEnsureModelRuntime(ctx, conn, port, accelerator, occupied, progress); err != nil {
					return err
				}
			}
			progress("Waiting for the model server")
			readyCtx, stop := context.WithTimeout(ctx, 90*time.Second)
			defer stop()
			for {
				probe, c := context.WithTimeout(readyCtx, 3*time.Second)
				err = chat.DeviceOllamaReady(probe, transport)
				c()
				if err == nil {
					break
				}
				if !install {
					return fmt.Errorf("the selected port must serve Ollama to download models: %w", err)
				}
				select {
				case <-readyCtx.Done():
					return fmt.Errorf("Ollama did not become ready; check the runtime app's logs: %w", err)
				case <-time.After(time.Second):
				}
			}
			progress("Downloading model weights on the device")
			if install {
				if err = commands.DesktopVerifyModelRuntime(ctx, conn, port); err != nil {
					return err
				}
			}
			if err = chat.DevicePullModel(ctx, transport, model, progress); err != nil {
				return err
			}
			progress("Loading model and checking inference")
			warmCtx, finish := context.WithTimeout(ctx, 10*time.Minute)
			defer finish()
			if err = chat.DeviceWarmModel(warmCtx, transport, model); err != nil {
				return err
			}
			if install {
				if err = commands.DesktopVerifyModelRuntime(ctx, conn, port); err != nil {
					return err
				}
			}
			progress("Ready to chat")
			return nil
		}
		err := work()
		run.mu.Lock()
		defer run.mu.Unlock()
		run.snapshot.Done = true
		if err != nil {
			if errors.Is(err, context.Canceled) {
				run.snapshot.Error = "Cancelled. Downloaded files and an installed runtime remain on the device."
			} else {
				run.snapshot.Error = err.Error()
			}
		}
	}()
	return nil
}
func (b *Backend) ModelRunStatus(session, job uint64) (ModelRunSnapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if session == 0 {
		return ModelRunSnapshot{}, errors.New("select a device first")
	}
	if _, err := b.connection(session); err != nil {
		return ModelRunSnapshot{}, err
	}
	if b.modelRun == nil {
		return ModelRunSnapshot{}, errors.New("no active model download")
	}
	b.modelRun.mu.Lock()
	defer b.modelRun.mu.Unlock()
	if b.modelRun.snapshot.ID != job {
		return ModelRunSnapshot{}, errors.New("model download changed")
	}
	return b.modelRun.snapshot, nil
}
func (b *Backend) CancelModelRun(session, job uint64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if session == 0 {
		return errors.New("select a device first")
	}
	if _, err := b.connection(session); err != nil {
		return err
	}
	if b.modelRun != nil && b.modelRun.snapshot.ID == job {
		b.modelRun.cancel()
	}
	return nil
}
