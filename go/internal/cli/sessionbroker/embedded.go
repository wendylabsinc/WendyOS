package sessionbroker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"github.com/wendylabsinc/wendy/go/internal/shared/useripc"
)

// An embedded host uses the ordinary private broker protocol and router. Only
// process ownership changes: the app must cancel and join it before suspension
// or changing profiles. Desktop callers continue to launch detached helpers.
var embeddedHost atomic.Pointer[embeddedRuntime]

var ErrEmbeddedSuspended = errors.New("Wendy Net is paused while the app is in the background")

type embeddedEntry struct {
	spec   Spec
	cancel context.CancelFunc
	done   chan struct{}
	err    error // published by closing done
}

type embeddedRuntime struct {
	control sync.Mutex
	mu      sync.Mutex
	active  atomic.Bool
	entries map[string]*embeddedEntry
	run     func(context.Context, Spec) error
}

func newEmbeddedRuntime(run func(context.Context, Spec) error) *embeddedRuntime {
	h := &embeddedRuntime{entries: make(map[string]*embeddedEntry), run: run}
	h.active.Store(true)
	return h
}

// EnableEmbeddedHost is called once by the native mobile host, before discovery.
// No radio, network connection, login or participant starts until requested.
func EnableEmbeddedHost() {
	useripc.EnableInProcess()
	h := newEmbeddedRuntime(func(ctx context.Context, spec Spec) error {
		data, err := json.Marshal(spec)
		if err != nil {
			return err
		}
		err = Run(ctx, base64.RawURLEncoding.EncodeToString(data), DefaultIdleTTL, nil)
		if os.Getenv("WENDY_MESH_DEBUG") == "1" {
			fmt.Fprintf(os.Stderr, "wendy: embedded mesh broker stopped: %v (context: %v)\n", err, ctx.Err())
		}
		return err
	})
	embeddedHost.CompareAndSwap(nil, h)
}

// SetEmbeddedActive drains every generation before allowing new starts. A timed
// out drain leaves the gate shut; a later resume must join those same workers.
func SetEmbeddedActive(ctx context.Context, active bool) error {
	if h := embeddedHost.Load(); h != nil {
		return h.setActive(ctx, active)
	}
	return nil
}

// EmbeddedStatus is observational. Startup failures remain available to the UI
// between discovery retries, rather than being reported as an empty network.
func EmbeddedStatus() (bool, error) {
	h := embeddedHost.Load()
	if h == nil {
		return true, nil
	}
	if !h.active.Load() {
		return false, ErrEmbeddedSuspended
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, entry := range h.entries {
		select {
		case <-entry.done:
			if entry.err != nil && !errors.Is(entry.err, context.Canceled) {
				return true, entry.err
			}
		default:
		}
	}
	return true, nil
}

func (h *embeddedRuntime) setActive(ctx context.Context, active bool) error {
	h.control.Lock()
	defer h.control.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	h.mu.Lock()
	if active && h.active.Load() {
		h.mu.Unlock()
		return nil
	}
	h.active.Store(false)
	entries := make([]*embeddedEntry, 0, len(h.entries))
	for _, entry := range h.entries {
		entry.cancel()
		entries = append(entries, entry)
	}
	h.mu.Unlock()
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-entry.done:
		}
	}
	h.mu.Lock()
	h.entries = make(map[string]*embeddedEntry)
	h.active.Store(active)
	h.mu.Unlock()
	return nil
}

func (h *embeddedRuntime) start(spec Spec) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.active.Load() {
		return ErrEmbeddedSuspended
	}
	if err := spec.validate(); err != nil {
		return err
	}
	if entry := h.entries[spec.Key]; entry != nil {
		select {
		case <-entry.done:
			delete(h.entries, spec.Key)
			if entry.err != nil && !errors.Is(entry.err, context.Canceled) {
				return entry.err
			}
		default:
			old, _ := json.Marshal(entry.spec)
			next, _ := json.Marshal(spec)
			if string(old) != string(next) {
				return errors.New("embedded broker identity changed before its previous generation drained")
			}
			return nil
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	entry := &embeddedEntry{spec: spec, cancel: cancel, done: make(chan struct{})}
	h.entries[spec.Key] = entry
	go func() {
		defer close(entry.done)
		defer cancel()
		entry.err = h.run(ctx, spec)
	}()
	return nil
}
