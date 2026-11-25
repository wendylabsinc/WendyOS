package main

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/wendylabsinc/wendy/go/world/internal/patrol"
)

// controlFence orders motion permission at authenticated request arrival, before
// cancellation or queueing can block. Stop remains latched for this executor.
type controlFence struct {
	mu         sync.Mutex
	blocked    *atomic.Bool
	generation uint64
	pending    int
	stopped    bool
}

func (f *controlFence) begin(action string) command {
	f.mu.Lock()
	defer f.mu.Unlock()
	if action == "pause" || action == "stop" {
		f.generation++
		f.pending++
		f.stopped = f.stopped || action == "stop"
		f.blocked.Store(true)
	}
	return command{action: action, reply: make(chan error, 1), generation: f.generation}
}

func (f *controlFence) complete(c command) {
	if c.action != "pause" && c.action != "stop" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending--
	// A failed enqueue/cancellation never implicitly restores motion permission.
}

func (f *controlFence) resume(c command, clear bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped || f.pending != 0 || c.generation != f.generation {
		return patrol.ErrInterrupted
	}
	if clear {
		f.blocked.Store(false)
	}
	return nil
}

func applyControl(c command, f *controlFence, engine *patrol.Engine, nowMS int64) error {
	defer f.complete(c)
	if c.action == "resume" {
		if err := f.resume(c, false); err != nil {
			return err
		}
	}
	err := engine.Control(c.action, nowMS)
	if c.action == "resume" && err == nil && (engine.State.State == "waiting" || engine.State.State == "return_planning") {
		// An interrupt may have arrived while Control was running. Check and clear
		// under the same lock used to register interrupts, never on recall success
		// or failure. A queued older resume cannot clear a newer queued Stop/Pause.
		return f.resume(c, true)
	}
	return err
}

func controlHandler(token string, commands chan command, fence *controlFence, interrupt func()) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "denied", 403)
			return
		}
		var body struct {
			Action string `json:"action"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		decoder.DisallowUnknownFields()
		var trailing any
		if decoder.Decode(&body) != nil || decoder.Decode(&trailing) != io.EOF {
			http.Error(w, "invalid control", 400)
			return
		}
		if body.Action != "pause" && body.Action != "resume" && body.Action != "stop" && body.Action != "recall" {
			http.Error(w, "unknown control", 400)
			return
		}
		c := fence.begin(body.Action)
		if body.Action == "pause" || body.Action == "stop" {
			interrupt()
			// Stop the navigation lease immediately, even during a route query.
		}
		select {
		case commands <- c:
		case <-r.Context().Done():
			fence.complete(c)
			return
		default:
			fence.complete(c)
			http.Error(w, "control busy", 503)
			return
		}
		select {
		case e := <-c.reply:
			if e != nil {
				http.Error(w, e.Error(), 409)
				return
			}
			w.Write([]byte(`{"accepted":true}`))
		case <-r.Context().Done():
		}
	}
}
