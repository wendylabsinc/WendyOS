// Package runtimediag writes optional process-local timing snapshots. These
// files are diagnostic observations, never durable control or authority state.
package runtimediag

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
)

type Writer struct {
	mu       sync.Mutex
	path     string
	metrics  *runtimeobs.Collector
	started  time.Time
	instance string
}

// Open requires a new file in an existing, trusted local directory. An existing
// file, symlink or FIFO is refused; callers choose a fresh path after restart.
func Open(path string, metrics *runtimeobs.Collector) (*Writer, error) {
	if path == "" || metrics == nil {
		return nil, errors.New("diagnostics require a new path and collector")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, errors.New("diagnostic instance identity unavailable")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, errors.New("diagnostics output must be a new local file")
	}
	if err = f.Close(); err != nil {
		_ = os.Remove(path)
		return nil, errors.New("diagnostics output close failed")
	}
	w := &Writer{path: path, metrics: metrics, started: time.Now(), instance: hex.EncodeToString(nonce[:])}
	if err = w.Flush(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	return w, nil
}

// Flush atomically replaces a bounded snapshot. It deliberately does not fsync:
// metrics must not add durable work to the storage path being measured.
func (w *Writer) Flush() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	value := struct {
		runtimeobs.Snapshot
		CapturedAtUnixMS int64  `json:"captured_at_unix_ms"`
		UptimeNS         int64  `json:"uptime_ns"`
		InstanceID       string `json:"instance_id"`
	}{w.metrics.Snapshot(), time.Now().UnixMilli(), time.Since(w.started).Nanoseconds(), w.instance}
	data, err := json.Marshal(value)
	if err != nil || len(data) > 64<<10 {
		return errors.New("diagnostics snapshot exceeds bound")
	}
	f, err := os.CreateTemp(filepath.Dir(w.path), ".world-metrics-*")
	if err != nil {
		return errors.New("diagnostics temporary output unavailable")
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.New("diagnostics output write failed")
	}
	if err = os.Rename(f.Name(), w.path); err != nil {
		return errors.New("diagnostics output replacement failed")
	}
	return nil
}

// Run returns after cancellation and a final flush, or on an output error.
func (w *Writer) Run(ctx context.Context) error {
	if w == nil {
		return nil
	}
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return w.Flush()
		case <-timer.C:
			if err := w.Flush(); err != nil {
				return err
			}
		}
	}
}
