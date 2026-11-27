package node

import (
	"context"
	"net/http"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
)

func (n *Node) lock() {
	if n.cfg.Metrics == nil {
		n.mu.Lock()
		return
	}
	started := time.Now()
	n.mu.Lock()
	n.cfg.Metrics.Observe(runtimeobs.NodeLockWait, time.Since(started))
}

// lockContext acquires the same node mutex without retaining canceled waiters.
// Polling TryLock keeps ownership in the calling goroutine and bounds contention
// retry work; cancellation does not wait for the current lock holder to finish.
func (n *Node) lockContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	started := time.Now()
	if !n.mu.TryLock() {
		retry := time.NewTicker(5 * time.Millisecond)
		defer retry.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-retry.C:
				if err := ctx.Err(); err != nil {
					return err
				}
				if !n.mu.TryLock() {
					continue
				}
			}
			break
		}
	}
	if n.cfg.Metrics != nil {
		n.cfg.Metrics.Observe(runtimeobs.NodeLockWait, time.Since(started))
	}
	return nil
}

type metricResponse struct {
	http.ResponseWriter
	status      int
	failedWrite bool
}

func (w *metricResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *metricResponse) WriteHeader(status int) {
	if status >= 200 && w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *metricResponse) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(data)
	w.failedWrite = w.failedWrite || err != nil
	return n, err
}
func (n *Node) measured(next http.Handler) http.Handler {
	if n.cfg.Metrics == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metric := runtimeobs.HTTPOther
		switch r.URL.Path {
		case "/v1/tasks/supervision/challenge":
			metric = runtimeobs.HTTPChallenge
		case "/v1/tasks/supervision/ack":
			metric = runtimeobs.HTTPAck
		case "/v1/tasks/evaluate":
			metric = runtimeobs.HTTPEvaluate
		case "/v1/tasks/stop":
			metric = runtimeobs.HTTPStop
		}
		started := time.Now()
		out := &metricResponse{ResponseWriter: w}
		returned := false
		defer func() {
			n.cfg.Metrics.Observe(metric, time.Since(started))
			if !returned || out.status >= 400 || out.failedWrite || r.Context().Err() != nil {
				n.cfg.Metrics.Increment(runtimeobs.HTTPFailures)
			}
		}()
		next.ServeHTTP(out, r)
		returned = true
	})
}
