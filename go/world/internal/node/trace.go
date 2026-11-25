package node

import (
	"context"
	"github.com/wendylabsinc/wendy/go/world/internal/diagnostics"
	"net/http"
)

type traceResponse struct {
	http.ResponseWriter
	ctx    context.Context
	status int
}

func (w *traceResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *traceResponse) WriteHeader(status int) {
	p := diagnostics.Begin(w.ctx, diagnostics.Response)
	defer p.End()
	if status >= 200 && w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *traceResponse) Write(b []byte) (int, error) {
	p := diagnostics.Begin(w.ctx, diagnostics.Response)
	if w.status == 0 {
		w.status = 200
	}
	n, err := w.ResponseWriter.Write(b)
	p.End(err)
	return n, err
}
func (n *Node) traced(next http.Handler) http.Handler {
	if n.cfg.Trace == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		class := diagnostics.Other
		switch r.URL.Path {
		case "/v1/tasks/supervision/challenge":
			class = diagnostics.Challenge
		case "/v1/tasks/supervision/ack":
			class = diagnostics.Ack
		case "/v1/tasks/evaluate":
			class = diagnostics.Evaluate
		case "/v1/tasks/stop":
			class = diagnostics.Stop
		case "/v1/task-relay", "/v1/task-relay/replica":
			class = diagnostics.TaskRelay
		case "/v1/tasks":
			if r.Method == http.MethodGet {
				class = diagnostics.TaskList
			}
		case "/v1/components", "/v1/annotations":
			if r.Method == http.MethodPost {
				class = diagnostics.Publication
			} else if r.Method == http.MethodGet {
				class = diagnostics.Query
			}
		case "/v1/repair/inventory", "/v1/repair/fetch", "/v1/replica":
			class = diagnostics.Repair
		}
		ctx, request := n.cfg.Trace.Start(r.Context(), class)
		if request == nil {
			next.ServeHTTP(w, r)
			return
		}
		out := &traceResponse{ResponseWriter: w, ctx: ctx}
		defer func() { request.Finish(out.status) }()
		next.ServeHTTP(out, r.WithContext(ctx))
	})
}
