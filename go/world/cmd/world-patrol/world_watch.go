package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolwatch"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"os"
	"sync"
	"time"
)

type worldReader struct {
	sync.Mutex
	records   []world.Component
	transport string
	until     time.Time
	cancel    context.CancelFunc
	done      chan struct{}
}

func startWorldReader(path string, p patrolwatch.Policy) (*worldReader, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Size() > 65536 {
		return nil, errors.New("bounded private World reader configuration required")
	}
	raw := make([]byte, st.Size())
	if _, e = f.ReadAt(raw, 0); e != nil {
		return nil, e
	}
	var c client.Config
	if e = authority.DecodeStrictLimit(raw, &c, 65536); e != nil {
		return nil, e
	}
	g := c.Grant.Grant
	if c.Audience != p.ReaderAudience || g.Principal != p.ReaderPrincipal || g.Tenant != p.Contract.Tenant || g.Site != p.Contract.Field.Site || g.Domain != "simulation" {
		return nil, errors.New("World reader identity or scope differs from signed policy")
	}
	allowed := false
	for _, s := range g.Scopes {
		if s == "read" {
			allowed = true
		}
	}
	if !allowed {
		return nil, errors.New("World reader requires read scope")
	}
	api, e := client.New(c)
	if e != nil {
		return nil, e
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &worldReader{transport: "unavailable", until: time.UnixMilli(g.NotAfterUnixMS), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		for {
			qctx, stop := context.WithTimeout(ctx, 400*time.Millisecond)
			snapshot, err := api.Query(qctx)
			stop()
			r.Lock()
			if err == nil {
				r.records = snapshot.Components
				r.transport = "connected"
			} else {
				r.transport = "unavailable"
				var status *client.HTTPStatusError
				if errors.As(err, &status) && status.StatusCode >= 400 && status.StatusCode < 500 {
					r.transport = "refused"
					r.records = nil
				}
			}
			r.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
		}
	}()
	return r, nil
}
func (r *worldReader) snapshot() ([]world.Component, string, time.Time) {
	r.Lock()
	defer r.Unlock()
	raw, _ := json.Marshal(r.records)
	var owned []world.Component
	_ = json.Unmarshal(raw, &owned)
	return owned, r.transport, r.until
}
func (r *worldReader) close() { r.cancel(); <-r.done }
