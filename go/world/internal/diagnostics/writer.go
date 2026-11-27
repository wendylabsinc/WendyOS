package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const MaxSnapshotBytes = 8 << 20

type Config struct {
	Path        string `json:"path"`
	TriggerPath string `json:"trigger_path"`
	RunNonce    string `json:"run_nonce"`
	WindowMS    int    `json:"window_ms"`
	GraceMS     int    `json:"grace_ms"`
	MaxRequests int    `json:"max_requests"`
	MaxEvents   int    `json:"max_events"`
}

// Writer assumes its parent directories are trusted. The embedding executable
// must reject aliases with every configuration/state/lock/staging path before Open.
// One final point-in-time export is retained; it is not updated after grace.
type Writer struct {
	cfg       Config
	collector *Collector
	mu        sync.Mutex
	ran       bool
}

func Open(cfg Config) (*Writer, error) {
	if !filepath.IsAbs(cfg.Path) || !filepath.IsAbs(cfg.TriggerPath) || filepath.Clean(cfg.Path) == filepath.Clean(cfg.TriggerPath) {
		return nil, errors.New("invalid trace paths")
	}
	for _, p := range []string{cfg.Path, cfg.TriggerPath} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			return nil, errors.New("trace paths must be absent")
		}
	}
	c, err := New(cfg.RunNonce, Limits{cfg.WindowMS, cfg.GraceMS, cfg.MaxRequests, cfg.MaxEvents})
	if err != nil {
		return nil, err
	}
	w := &Writer{cfg: cfg, collector: c}
	if err = w.export(true); err != nil {
		return nil, err
	}
	return w, nil
}
func (w *Writer) Collector() *Collector {
	if w == nil {
		return nil
	}
	return w.collector
}
func (w *Writer) export(initial bool) error {
	return w.exportSnapshot(w.collector.Snapshot(), initial)
}
func (w *Writer) exportSnapshot(snapshot Snapshot, initial bool) error {
	b, err := json.Marshal(snapshot)
	if err != nil || len(b) > MaxSnapshotBytes {
		return errors.New("trace snapshot exceeds bound")
	}
	f, err := os.CreateTemp(filepath.Dir(w.cfg.Path), ".request-trace-")
	if err != nil {
		return errors.New("trace output unavailable")
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return errors.New("trace output write failed")
	}
	if initial {
		err = os.Link(tmp, w.cfg.Path)
	} else {
		err = os.Rename(tmp, w.cfg.Path)
	}
	if err != nil {
		return errors.New("trace output install failed")
	}
	return nil
}
func (w *Writer) trigger() (bool, error) {
	fd, err := syscall.Open(w.cfg.TriggerPath, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, errors.New("invalid trace trigger")
	}
	f := os.NewFile(uintptr(fd), "trace-trigger")
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > 256 {
		return false, errors.New("invalid trace trigger")
	}
	b, err := io.ReadAll(io.LimitReader(f, 257))
	if err != nil || len(b) > 256 {
		return false, errors.New("invalid trace trigger")
	}
	var compact bytes.Buffer
	if json.Compact(&compact, b) != nil {
		return false, errors.New("invalid trace trigger")
	}
	expected := []byte(`{"version":1,"run_nonce":"` + w.cfg.RunNonce + `"}`)
	if !bytes.Equal(compact.Bytes(), expected) {
		return false, errors.New("invalid trace trigger")
	}
	return true, nil
}
func (w *Writer) Run(ctx context.Context) error {
	if w == nil {
		<-ctx.Done()
		return nil
	}
	w.mu.Lock()
	if w.ran {
		w.mu.Unlock()
		return errors.New("trace writer already run")
	}
	w.ran = true
	w.mu.Unlock()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var deadline <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return w.export(false)
		case <-deadline:
			return w.export(false)
		case <-ticker.C:
			armed, err := w.trigger()
			if err != nil {
				return err
			}
			if !armed {
				continue
			}
			ack, err := w.collector.armSnapshot(w.cfg.RunNonce)
			if err != nil {
				return err
			}
			// Capture the empty acknowledgement atomically with arming, before requests.
			if err = w.exportSnapshot(ack, false); err != nil {
				return err
			}
			ticker.Stop()
			timer = time.NewTimer(time.Duration(w.cfg.WindowMS+w.cfg.GraceMS) * time.Millisecond)
			deadline = timer.C
		}
	}
}
