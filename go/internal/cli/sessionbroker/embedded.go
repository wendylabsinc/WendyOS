package sessionbroker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
)

var embedded = struct {
	sync.Mutex
	paused  bool
	workers map[string]*embeddedWorker
}{workers: make(map[string]*embeddedWorker)}

type embeddedWorker struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// iOS cannot spawn CLI helpers. Its app and extension use the same private IPC
// and identity locks; only one process owns a broker for a given identity.
func startEmbedded(spec Spec) error {
	data, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	key := spec.Key
	embedded.Lock()
	defer embedded.Unlock()
	if embedded.paused {
		return errors.New("VPN extension owns the mobile mesh; wait for it to connect")
	}
	if embedded.workers[key] != nil {
		return nil
	}
	if len(embedded.workers) >= 16 {
		return errors.New("embedded broker capacity reached")
	}
	ctx, cancel := context.WithCancel(context.Background())
	worker := &embeddedWorker{cancel: cancel, done: make(chan struct{})}
	embedded.workers[key] = worker
	go func() {
		defer close(worker.done)
		_ = Run(ctx, base64.RawURLEncoding.EncodeToString(data), DefaultIdleTTL, nil)
		embedded.Lock()
		delete(embedded.workers, key)
		embedded.Unlock()
	}()
	return nil
}

// SuspendEmbedded drains app-owned workers before handing the profile to the
// VPN extension. New calls may still attach to the extension's existing IPC.
func SuspendEmbedded() {
	embedded.Lock()
	embedded.paused = true
	workers := make([]*embeddedWorker, 0, len(embedded.workers))
	for _, worker := range embedded.workers {
		workers = append(workers, worker)
		worker.cancel()
	}
	embedded.Unlock()
	for _, worker := range workers {
		<-worker.done
	}
}
func ResumeEmbedded() { embedded.Lock(); embedded.paused = false; embedded.Unlock() }
