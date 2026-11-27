package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
)

// Carrier health is independent of the Internet-sharing controller's mode.
// Epochs prevent callbacks from a drained configuration reviving its status.
type meshCarrierHealth struct {
	mu       sync.Mutex
	epoch    uint64
	config   string
	carriers map[string]localmesh.CarrierStatus
}

func (h *meshCarrierHealth) begin(config string, names []string) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	next := make(map[string]localmesh.CarrierStatus, len(names))
	for _, name := range names {
		value := localmesh.CarrierStatus{}
		// A retry isn't recovery: keep the last error until actual readiness.
		if h.config == config && h.carriers[name].Err != nil {
			value.Err = h.carriers[name].Err
		}
		next[name] = value
	}
	h.epoch++
	h.config, h.carriers = config, next
	return h.epoch
}

func (h *meshCarrierHealth) report(epoch uint64, name string, value localmesh.CarrierStatus) {
	h.mu.Lock()
	defer h.mu.Unlock()
	previous, enabled := h.carriers[name]
	if epoch != h.epoch || !enabled {
		return
	}
	if !value.Ready && !value.NoHardware && !value.Waiting && value.Err == nil {
		value.Err = previous.Err
	}
	h.carriers[name] = value
}

func (h *meshCarrierHealth) end(epoch uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.epoch == epoch {
		h.epoch++
		h.carriers = nil
	}
}

func (h *meshCarrierHealth) fail(epoch uint64, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.epoch != epoch {
		return
	}
	for name := range h.carriers {
		h.carriers[name] = localmesh.CarrierStatus{Err: err}
	}
}

func (h *meshCarrierHealth) status(sharing services.LocalMeshRuntimeStatus) services.LocalMeshRuntimeStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.carriers) == 0 {
		return sharing
	}
	out := sharing // Preserve authenticated peers and signed gateway identity.
	out.Available = true
	out.CarrierConfig = h.config
	out.Carriers = make(map[string]string, len(h.carriers))
	ready, absent, initializing, failed := 0, 0, false, false
	for name, value := range h.carriers {
		if value.Ready {
			ready++
		}
		if value.Err != nil {
			failed = true
		}
		if value.NoHardware {
			absent++
		}
		state := carrierState(value)
		initializing = initializing || state == "starting"
		detail := value.Detail
		if value.Err != nil {
			if detail != "" {
				detail += "; "
			}
			detail += value.Err.Error()
		}
		if detail != "" {
			state += " (" + compactCarrierDetail(detail) + ")"
		}
		out.Carriers[name] = state
	}
	switch {
	case failed && ready == 0:
		out.State = "failed"
	case failed || sharing.State == "error":
		out.State = "degraded"
	case ready > 0:
		out.State = "mesh-active"
	case absent == len(h.carriers):
		out.State = "unavailable"
	case initializing:
		out.State = "initializing"
	default:
		out.State = "waiting"
	}
	out.Detail = ""
	if sharing.State != "" || sharing.Detail != "" {
		out.Detail = "Internet sharing: " + sharing.State
		if sharing.Detail != "" {
			out.Detail += " (" + sharing.Detail + ")"
		}
		out.Detail += "."
	}
	return out
}

func carrierState(value localmesh.CarrierStatus) string {
	switch {
	case value.Err != nil:
		return "fault"
	case value.Ready:
		return "ok"
	case value.NoHardware:
		return "no hardware"
	case value.Waiting:
		return "waiting"
	default:
		return "starting"
	}
}

func compactCarrierDetail(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const limit = 512
	runes := []rune(s)
	if len(runes) > limit {
		s = string(runes[:limit]) + "…"
	}
	return s
}

// The provider must report readiness after setup succeeds. Merely entering a
// retry cannot clear a failure. Late callbacks from an exited attempt cannot
// overwrite a newer failure or recovery.
func runMeshCarrier(ctx context.Context, retry time.Duration, report func(localmesh.CarrierStatus), run func(context.Context, func(localmesh.CarrierStatus)) error) {
	for ctx.Err() == nil {
		var mu sync.Mutex
		active := true
		observe := func(value localmesh.CarrierStatus) {
			mu.Lock()
			defer mu.Unlock()
			if active && ctx.Err() == nil {
				report(value)
			}
		}
		err := run(ctx, observe)
		mu.Lock()
		active = false
		mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, localmesh.ErrNoCarrierHardware) {
			report(localmesh.CarrierStatus{NoHardware: true, Detail: strings.TrimPrefix(err.Error(), localmesh.ErrNoCarrierHardware.Error()+": ")})
			return
		}
		if err == nil {
			err = errors.New("mesh connection stopped unexpectedly")
		}
		report(localmesh.CarrierStatus{Err: err})
		timer := time.NewTimer(retry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
