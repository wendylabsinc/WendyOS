package replication

import (
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Ephemeral transfer hints only. Original proofs/floors belong in the durable
// stores; neither this cursor nor a saved policy number grants authority.
type progress struct {
	Cursor                            []byte
	Last, Binding                     string
	LocalGeneration, RemoteGeneration uint64
	Expires                           time.Duration
	Deferred                          bool
}

func (s *Service) clearProgress(device string) {
	s.mu.Lock()
	delete(s.progress, device)
	s.mu.Unlock()
}
func (s *Service) loadProgress(p Identity, g uint64) progress {
	now := s.elapsed()
	s.mu.Lock()
	defer s.mu.Unlock()
	for device, v := range s.progress {
		if now < 0 || v.Expires <= now {
			delete(s.progress, device)
		}
	}
	v := s.progress[p.Device]
	if v.Binding != p.BindingSHA256 || v.LocalGeneration != g {
		delete(s.progress, p.Device)
		return progress{}
	}
	v.Cursor = append([]byte(nil), v.Cursor...)
	return v
}
func (s *Service) saveProgress(p Identity, g, remote uint64, cursor []byte, last string, started time.Duration, deferred bool) error {
	now := s.elapsed()
	if now < started || started < 0 || now-started >= 2*time.Minute || len(cursor) == 0 || len(cursor) > MaxCursorBytes || !validOrder(last) {
		s.clearProgress(p.Device)
		return ErrProtocol
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for device, v := range s.progress {
		if v.Expires <= now {
			delete(s.progress, device)
		}
	}
	if _, exists := s.progress[p.Device]; !exists && len(s.progress) >= s.cfg.Limits.MaxPeers {
		return ErrCapacity
	}
	s.progress[p.Device] = progress{Cursor: append([]byte(nil), cursor...), Last: last, Binding: p.BindingSHA256, LocalGeneration: g, RemoteGeneration: remote, Expires: started + 2*time.Minute, Deferred: deferred}
	return nil
}
func resetProgress(err error) bool {
	if errors.Is(err, ErrProtocol) || errors.Is(err, ErrDenied) || errors.Is(err, ErrUncertain) {
		return true
	}
	switch status.Code(err) {
	case codes.InvalidArgument, codes.PermissionDenied, codes.Unauthenticated, codes.FailedPrecondition:
		return true
	}
	return false
}
