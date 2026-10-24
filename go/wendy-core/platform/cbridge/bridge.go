// Package cbridge adapts registered C host callbacks to Core's Go capabilities.
// It is linked by the native engine; pure Go hosts use platform directly.
package cbridge

/*
#include "callbacks.h"
*/
import "C"

import (
	"bytes"
	"context"
	"io"
	"sync"
	"unsafe"

	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
)

var registry = struct {
	sync.Mutex
	next  uint64
	hosts map[uint64]*Host
	calls map[uint64]*call
}{hosts: make(map[uint64]*Host), calls: make(map[uint64]*call)}

// Host supplies signing and RPCs through one registered C callback table.
// Release of its C registration cancels all operations, including ones invoked
// through previously retained Go adapters. Host must not be copied.
type Host struct {
	callbacks C.WendyHostCallbacks
	algorithm platform.SignatureAlgorithm
	mu        sync.Mutex
	closed    bool
	refs      int // registration and active calls (including their callbacks)
	calls     map[*call]context.CancelFunc
}

var _ platform.Signer = (*Host)(nil)
var _ platform.Transport = (*Host)(nil)

// Lookup returns the capabilities for a live WendyHost registration. The ID is
// scoped to this engine/process. It does not transfer registration ownership.
func Lookup(id uint64) (*Host, bool) {
	registry.Lock()
	defer registry.Unlock()
	h, ok := registry.hosts[id]
	return h, ok
}

// Algorithm returns the immutable, copied signature algorithm.
func (h *Host) Algorithm() platform.SignatureAlgorithm { return h.algorithm }

// CertificateChain fetches host-owned identity's DER certificates, leaf first.
func (h *Host) CertificateChain(ctx context.Context) ([][]byte, error) {
	r, err := h.unary(ctx, C.WendyHostCertificateChain, platform.RPCRequest{})
	return r.certificates, err
}

// PublicKey fetches the host's DER SubjectPublicKeyInfo.
func (h *Host) PublicKey(ctx context.Context) ([]byte, error) {
	r, err := h.unary(ctx, C.WendyHostPublicKey, platform.RPCRequest{})
	return r.response.Message, err
}

// Sign asks the host to sign exactly message without hashing or encoding it.
func (h *Host) Sign(ctx context.Context, message []byte) ([]byte, error) {
	r, err := h.unary(ctx, C.WendyHostSign, platform.RPCRequest{Message: message})
	return r.response.Message, err
}

// Unary sends a serialized RPC using host-owned networking.
func (h *Host) Unary(ctx context.Context, request platform.RPCRequest) (platform.RPCResponse, error) {
	r, err := h.unary(ctx, C.WendyHostUnary, request)
	return r.response, err
}

// ServerStream opens an RPC. Headers and messages arrive asynchronously; Recv
// requests one message at a time. The caller must Close the returned stream.
func (h *Host) ServerStream(ctx context.Context, request platform.RPCRequest) (platform.ServerStream, error) {
	c, err := h.start(ctx, C.WendyHostServerStream, request)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	err = c.result.err
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &stream{c: c}, nil
}

type result struct {
	response     platform.RPCResponse
	certificates [][]byte
	err          error
}

type call struct {
	id        uint64
	host      *Host
	ctx       context.Context
	cancel    context.CancelFunc
	streaming bool
	dispatch  sync.Mutex // serializes C start, next and cancel (never held by completion)
	mu        sync.Mutex
	done      chan struct{}
	changed   chan struct{}
	terminal  bool
	result    result
	headers   bool
	demanded  bool
	item      []byte
	hasItem   bool
}

func (h *Host) unref() {
	h.mu.Lock()
	h.refs--
	last := h.refs == 0
	h.mu.Unlock()
	if last {
		C.host_release(h.callbacks)
	}
}

func (h *Host) start(ctx context.Context, operation C.uint32_t, request platform.RPCRequest) (*call, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	c := &call{host: h, ctx: ctx, cancel: cancel, streaming: operation == C.WendyHostServerStream,
		done: make(chan struct{}), changed: make(chan struct{}, 1)}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		cancel()
		return nil, context.Canceled
	}
	h.refs++
	h.calls[c] = cancel
	h.mu.Unlock()
	registry.Lock()
	registry.next++
	c.id = registry.next
	registry.calls[c.id] = c
	registry.Unlock()

	c.dispatch.Lock()
	var a arena
	defer func() { a.free() }()
	r := C.WendyHostRequest{operation: operation, message: a.data(request.Message),
		method: a.data([]byte(request.Method)), metadata: a.metadata(request.Metadata)}
	if deadline, ok := ctx.Deadline(); ok {
		r.deadline_unix_millis = C.int64_t(deadline.UnixMilli())
	}
	C.host_start(h.callbacks, C.WendyHostCall(c.id), &r)
	c.dispatch.Unlock()
	go c.watch()
	return c, nil
}

func (c *call) watch() {
	select {
	case <-c.ctx.Done():
		c.mu.Lock()
		canceled := c.finishLocked(result{err: c.ctx.Err()})
		c.mu.Unlock()
		if canceled {
			c.dispatch.Lock()
			C.host_cancel(c.host.callbacks, C.WendyHostCall(c.id))
			c.dispatch.Unlock()
		}
	case <-c.done:
	}
	registry.Lock()
	delete(registry.calls, c.id)
	registry.Unlock()
	// A next callback may still be returning after an inline terminal result.
	c.dispatch.Lock()
	c.dispatch.Unlock()
	c.cancel()
	c.host.mu.Lock()
	delete(c.host.calls, c)
	c.host.mu.Unlock()
	c.host.unref()
}

func (c *call) signal() {
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

// c.mu is held; terminal delivery never replaces previously delivered headers.
func (c *call) finishLocked(r result) bool {
	if c.terminal {
		return false
	}
	if c.headers {
		r.response.InitialMetadata = c.result.response.InitialMetadata
	}
	if status, ok := r.err.(*platform.RPCStatusError); ok {
		status.InitialMetadata = cloneMetadata(r.response.InitialMetadata)
	}
	c.result = r
	c.terminal = true
	close(c.done)
	c.signal()
	return true
}

func (h *Host) unary(ctx context.Context, operation C.uint32_t, request platform.RPCRequest) (result, error) {
	c, err := h.start(ctx, operation, request)
	if err != nil {
		return result{}, err
	}
	<-c.done
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.result, c.result.err
}

type stream struct {
	c                *call
	terminalObserved bool
}

func (s *stream) Header() (platform.Metadata, error) {
	c := s.c
	for {
		c.mu.Lock()
		if c.headers || c.terminal {
			headers, err := cloneMetadata(c.result.response.InitialMetadata), c.result.err
			if c.headers {
				err = nil
			}
			c.mu.Unlock()
			return headers, err
		}
		c.mu.Unlock()
		<-c.changed
	}
}

func (s *stream) Recv() ([]byte, error) {
	c := s.c
	for {
		c.mu.Lock()
		if c.hasItem {
			item := c.item
			c.item, c.hasItem = nil, false
			c.mu.Unlock()
			return item, nil
		}
		if c.terminal {
			s.terminalObserved = true
			err := c.result.err
			if err == nil {
				err = io.EOF
			}
			c.mu.Unlock()
			return nil, err
		}
		request := !c.demanded
		c.demanded = true
		c.mu.Unlock()
		if request {
			c.dispatch.Lock()
			c.mu.Lock()
			live := !c.terminal
			c.mu.Unlock()
			if live {
				C.host_next(c.host.callbacks, C.WendyHostCall(c.id))
			}
			c.dispatch.Unlock()
		}
		<-c.changed
	}
}

func (s *stream) Trailer() platform.Metadata {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	if !s.terminalObserved {
		return nil
	}
	return cloneMetadata(s.c.result.response.TrailingMetadata)
}

func (s *stream) Close() error { s.c.cancel(); return nil }

func cloneMetadata(m platform.Metadata) platform.Metadata {
	if m == nil {
		return nil
	}
	out := make(platform.Metadata, len(m))
	for i, e := range m {
		out[i] = platform.MetadataEntry{Name: e.Name, Value: bytes.Clone(e.Value)}
	}
	return out
}

func findCall(id C.WendyHostCall) *call {
	registry.Lock()
	defer registry.Unlock()
	return registry.calls[uint64(id)]
}

//export wendyHostCreate
func wendyHostCreate(cb *C.WendyHostCallbacks) C.WendyHost {
	if C.host_valid(cb) == 0 {
		return 0
	}
	h := &Host{callbacks: *cb, algorithm: platform.SignatureAlgorithm(readBytes(cb.algorithm)),
		refs: 1, calls: make(map[*call]context.CancelFunc)}
	registry.Lock()
	defer registry.Unlock()
	registry.next++
	id := registry.next
	registry.hosts[id] = h
	return C.WendyHost(id)
}

//export wendyHostRelease
func wendyHostRelease(id C.WendyHost) {
	registry.Lock()
	h := registry.hosts[uint64(id)]
	delete(registry.hosts, uint64(id))
	registry.Unlock()
	if h == nil {
		return
	}
	h.mu.Lock()
	h.closed = true
	for _, cancel := range h.calls {
		cancel()
	}
	h.mu.Unlock()
	h.unref()
}

//export wendyHostComplete
func wendyHostComplete(id C.WendyHostCall, r *C.WendyHostResult) {
	c := findCall(id)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.terminal {
		return
	}
	if r == nil {
		c.finishLocked(result{err: &platform.RPCStatusError{Code: platform.RPCInternal}})
		return
	}
	response := platform.RPCResponse{Message: readBytes(r.message),
		InitialMetadata: readMetadata(r.initial_metadata), TrailingMetadata: readMetadata(r.trailing_metadata)}
	out := result{response: response}
	for _, cert := range unsafe.Slice(r.certificates, int(r.certificate_count)) {
		out.certificates = append(out.certificates, readBytes(cert))
	}
	if r.code != 0 {
		out.response.Message = nil
		out.certificates = nil
		out.err = &platform.RPCStatusError{Code: platform.RPCStatusCode(r.code), Message: string(readBytes(r.status_message)),
			InitialMetadata: cloneMetadata(response.InitialMetadata), TrailingMetadata: cloneMetadata(response.TrailingMetadata)}
	}
	c.finishLocked(out)
}

//export wendyHostStreamHeader
func wendyHostStreamHeader(id C.WendyHostCall, m C.WendyHostMetadata) C.int32_t {
	c := findCall(id)
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.streaming || c.terminal || c.headers {
		return 0
	}
	c.result.response.InitialMetadata = readMetadata(m)
	c.headers = true
	c.signal()
	return 1
}

//export wendyHostStreamMessage
func wendyHostStreamMessage(id C.WendyHostCall, message C.WendyHostBytes) C.int32_t {
	c := findCall(id)
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.streaming || c.terminal || !c.headers || !c.demanded || c.hasItem {
		return 0
	}
	c.item = readBytes(message)
	c.hasItem, c.demanded = true, false
	c.signal()
	return 1
}
