package services

import (
	"context"
	"errors"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/agent/data/live"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// maxAppDataStreams bounds the StreamData calls one app holds open at once.
// The hub serves at most live.DefaultLimits.MaxSubscribers callers for the
// whole device, operators included, so one app must not be able to take every
// slot. Eight leaves an on-device page room for a few viewers with different
// selections; a page with more viewers should share one subscription.
const maxAppDataStreams = 8

// reservedOperatorDataStreams are the hub slots apps never take. All apps
// together hold at most live.DefaultLimits.MaxSubscribers minus these, so
// however many data-read apps a device runs, an operator's viewer still gets
// in.
const reservedOperatorDataStreams = 8

// appDataStreamQuota counts the StreamData calls all of a device's apps hold
// open together.
type appDataStreamQuota struct {
	mu   sync.Mutex
	open int
	max  int
}

func newAppDataStreamQuota() *appDataStreamQuota {
	return &appDataStreamQuota{max: live.DefaultLimits.MaxSubscribers - reservedOperatorDataStreams}
}

// acquire takes a slot, and reports false when every app slot is taken. A nil
// quota has no cap.
func (q *appDataStreamQuota) acquire() bool {
	if q == nil {
		return true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.open >= q.max {
		return false
	}
	q.open++
	return true
}

func (q *appDataStreamQuota) release() {
	if q == nil {
		return
	}
	q.mu.Lock()
	q.open--
	q.mu.Unlock()
}

// errDataReadNarrowed ends a stream whose app no longer may read everything
// the stream was opened for.
var errDataReadNarrowed = errors.New("this app's data-read entitlement changed; subscribe again")

// systemDataService is DataService as an app sees it on its System API socket:
// StreamData and nothing else. Every other method stays unimplemented, and the
// socket's interceptors refuse them before they get here.
//
// It reads from the same hub as the control-plane handler, so an app sees the
// same records, timestamps and delivery rules as an operator, limited to what
// its data-read allowlist admits. The socket is the caller's credential, as for
// notifications: the request carries no identity. A stream is opened with the
// app's grant as it stands then, and ends when a later change narrows it.
type systemDataService struct {
	agentpbv2.UnimplementedDataServiceServer
	hub *live.Hub
	// quota is shared by every app's socket; see reservedOperatorDataStreams.
	quota *appDataStreamQuota
	// scope returns the app's data-read allowlist, and false when no service
	// of the app holds data-read any more. It takes the socket manager's lock,
	// so it is never called with that lock held.
	scope func() (allow []string, ok bool)

	mu   sync.Mutex
	open map[*openDataStream]struct{}
}

type openDataStream struct {
	allow  []string
	cancel context.CancelCauseFunc
}

// dataStreamWithContext is a StreamData stream whose context the service can
// end on its own.
type dataStreamWithContext struct {
	grpc.ServerStreamingServer[agentpbv2.DataRecord]
	ctx context.Context
}

func (s dataStreamWithContext) Context() context.Context { return s.ctx }

func (s *systemDataService) StreamData(req *agentpbv2.StreamDataRequest, out grpc.ServerStreamingServer[agentpbv2.DataRecord]) error {
	ctx, cancel := context.WithCancelCause(out.Context())
	defer cancel(nil)
	// Read the scope and register under one lock, so a narrowing either
	// happened before this call read its scope or sees this call.
	s.mu.Lock()
	allow, ok := s.scope()
	if !ok {
		s.mu.Unlock()
		return status.Error(codes.PermissionDenied, "System API capability is not authorized for this app")
	}
	if len(s.open) >= maxAppDataStreams {
		s.mu.Unlock()
		return status.Errorf(codes.ResourceExhausted, "an app may hold at most %d live data streams at once", maxAppDataStreams)
	}
	if !s.quota.acquire() {
		s.mu.Unlock()
		return status.Errorf(codes.ResourceExhausted, "the apps on this device already hold %d live data streams, all that apps may hold together; the rest are kept for operators", s.quota.max)
	}
	call := &openDataStream{allow: allow, cancel: cancel}
	if s.open == nil {
		s.open = make(map[*openDataStream]struct{})
	}
	s.open[call] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.open, call)
		s.mu.Unlock()
		s.quota.release()
	}()

	err := streamLiveData(s.hub, req, allow, dataStreamWithContext{out, ctx})
	if errors.Is(context.Cause(ctx), errDataReadNarrowed) {
		return status.Error(codes.Aborted, errDataReadNarrowed.Error())
	}
	return err
}

// narrow ends the open streams that the app's current scope no longer covers.
// The socket manager calls it, without its lock, after the app's owners
// change. A wider scope ends nothing; the app subscribes again to use it.
func (s *systemDataService) narrow() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.open) == 0 {
		return
	}
	allow, ok := s.scope()
	for call := range s.open {
		if !ok || !covers(allow, call.allow) {
			call.cancel(errDataReadNarrowed)
		}
	}
}

// covers reports whether scope admits every stream that within admits. An
// entry of within names its stream ids by prefix, so scope covers it when it
// permits that prefix as a stream id.
func covers(scope, within []string) bool {
	if len(scope) == 0 {
		return true
	}
	if len(within) == 0 {
		return false
	}
	for _, entry := range within {
		if !live.Permits(scope, entry) {
			return false
		}
	}
	return true
}
