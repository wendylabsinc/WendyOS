package serviceevidence

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"golang.org/x/sys/unix"
)

func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var err error
	if s.inner != nil {
		err = s.inner.Close()
	}
	if s.lock != nil {
		if e := unix.Flock(int(s.lock.Fd()), unix.LOCK_UN); err == nil {
			err = e
		}
		if e := s.lock.Close(); err == nil {
			err = e
		}
	}
	return err
}
func (s *Store) owner(o Owner) (string, error) {
	if o.Tenant != s.cfg.Tenant || o.Site != s.cfg.Site || o.Device != s.cfg.Device {
		return "", ErrDenied
	}
	h, err := o.SHA256()
	if err != nil {
		return "", ErrDenied
	}
	return "wendy.world.service-evidence.v1:" + h, nil
}
func (s *Store) fail(err error) error { s.fault = errors.Join(ErrFaulted, err); return s.fault }
func (s *Store) begin(ctx context.Context) (trustedtime.Interval, error) {
	if ctx == nil {
		return trustedtime.Interval{}, ErrDenied
	}
	if err := ctx.Err(); err != nil {
		return trustedtime.Interval{}, err
	}
	if s.closed {
		return trustedtime.Interval{}, ErrClosed
	}
	if s.fault != nil {
		return trustedtime.Interval{}, s.fault
	}
	w, err := s.now()
	if err != nil {
		return trustedtime.Interval{}, s.fail(err)
	}
	// Fence retention before the reused store can prune or alter any bytes.
	if err = s.saveFloor(ctx, w); err != nil {
		return trustedtime.Interval{}, s.fail(err)
	}
	// A slow fsync must not carry an expired clock into the blob operation.
	w, err = s.now()
	if err != nil {
		return trustedtime.Interval{}, s.fail(err)
	}
	if err = ctx.Err(); err != nil {
		return trustedtime.Interval{}, err
	}
	s.storeNow = w.Latest
	return w, nil
}
func (s *Store) finish(ctx context.Context) (trustedtime.Interval, error) {
	w, err := s.now()
	if err != nil {
		return trustedtime.Interval{}, s.fail(errors.Join(ErrUncertain, err))
	}
	if err = s.saveFloor(ctx, w); err != nil {
		return trustedtime.Interval{}, s.fail(errors.Join(ErrUncertain, err))
	}
	// Recheck after the final durability boundary as well as the blob operation.
	w, err = s.now()
	if err != nil {
		return trustedtime.Interval{}, s.fail(errors.Join(ErrUncertain, err))
	}
	if err = ctx.Err(); err != nil {
		return trustedtime.Interval{}, errors.Join(ErrUncertain, err)
	}
	return w, nil
}
func validDigest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && strings.ToLower(s) == s
}
func validType(s string) bool { return s == "image/png" || s == "image/jpeg" || s == "audio/wav" }
func reference(o Owner, sha string, size int64, mime string, until int64) Reference {
	h, _ := o.SHA256()
	return Reference{Owner: o, OwnerSHA256: h, SHA256: sha, Size: size, ContentType: mime, RetainUntilUnixMS: until}
}
func (s *Store) Put(ctx context.Context, o Owner, r PutRequest) (PutResponse, error) {
	if s == nil {
		return PutResponse{}, ErrClosed
	}
	s.traceLock(ctx)
	defer s.mu.Unlock()
	owner, err := s.owner(o)
	if err != nil {
		return PutResponse{}, err
	}
	if !validDigest(r.SHA256) || r.Size < 1 || r.Size > s.cfg.MaxBlobBytes || r.Offset < 0 || r.Offset > r.Size || len(r.Data) > MaxChunk || int64(len(r.Data)) > r.Size-r.Offset || !validType(r.ContentType) || r.RetainUntilUnixMS < 0 {
		return PutResponse{}, ErrDenied
	}
	if _, err = s.begin(ctx); err != nil {
		return PutResponse{}, err
	}
	// Own mutable caller data before passing it to storage; the host must also
	// avoid concurrent mutation during the call, as for other Go byte inputs.
	r.Data = append([]byte(nil), r.Data...)
	got, operationErr := s.inner.Put(owner, evidence.PutRequest{SHA256: r.SHA256, Size: r.Size, Offset: r.Offset, Data: r.Data, ContentType: r.ContentType, RetainUntilUnixMS: r.RetainUntilUnixMS, RetentionClass: evidence.RetentionProtected})
	after, err := s.finish(ctx)
	if err != nil {
		return PutResponse{}, err
	}
	if operationErr != nil {
		return PutResponse{}, operationErr
	}
	if got.RetainUntilUnixMS <= after.Latest.UnixMilli() {
		return PutResponse{}, ErrUnavailable
	}
	return PutResponse{Reference: reference(o, got.SHA256, got.Size, got.ContentType, got.RetainUntilUnixMS), Received: got.Received, Complete: got.Complete}, nil
}
func (s *Store) Get(ctx context.Context, o Owner, r GetRequest) (GetResponse, error) {
	if s == nil {
		return GetResponse{}, ErrClosed
	}
	s.traceLock(ctx)
	defer s.mu.Unlock()
	owner, err := s.owner(o)
	if err != nil {
		return GetResponse{}, err
	}
	if !validDigest(r.SHA256) || r.Offset < 0 || r.Length < 1 || r.Length > MaxChunk {
		return GetResponse{}, ErrDenied
	}
	if _, err = s.begin(ctx); err != nil {
		return GetResponse{}, err
	}
	readDone := rpctrace.Start(ctx, rpctrace.StageEvidenceRead)
	got, operationErr := s.inner.GetOwned(owner, evidence.GetRequest{SHA256: r.SHA256, Offset: r.Offset, Length: r.Length})
	readDone()
	after, err := s.finish(ctx)
	if err != nil {
		return GetResponse{}, err
	}
	if operationErr != nil {
		return GetResponse{}, operationErr
	}
	if got.RetainUntilUnixMS <= after.Latest.UnixMilli() {
		return GetResponse{}, ErrUnavailable
	}
	return GetResponse{Reference: reference(o, got.SHA256, got.Size, got.ContentType, got.RetainUntilUnixMS), Offset: got.Offset, Data: got.Data}, nil
}
func (s *Store) MatchComplete(ctx context.Context, o Owner, sha string, size int64, contentType string) (Reference, error) {
	ref, _, err := s.ReadComplete(ctx, o, sha, size, contentType)
	return ref, err
}

// ReadComplete is for bounded in-agent schema validation, not a caller-facing
// bulk read API. It performs one whole-blob integrity read, checks exact owner
// metadata and physical retention, and exposes nothing after a clock failure.
func (s *Store) ReadComplete(ctx context.Context, o Owner, sha string, size int64, contentType string) (Reference, []byte, error) {
	if s == nil {
		return Reference{}, nil, ErrClosed
	}
	s.traceLock(ctx)
	defer s.mu.Unlock()
	return s.readCompleteLocked(ctx, o, sha, size, contentType)
}

func (s *Store) readCompleteLocked(ctx context.Context, o Owner, sha string, size int64, contentType string) (Reference, []byte, error) {
	owner, err := s.owner(o)
	if err != nil {
		return Reference{}, nil, err
	}
	if !validDigest(sha) || size < 1 || size > s.cfg.MaxBlobBytes || !validType(contentType) {
		return Reference{}, nil, ErrDenied
	}
	if _, err = s.begin(ctx); err != nil {
		return Reference{}, nil, err
	}
	readDone := rpctrace.Start(ctx, rpctrace.StageEvidenceRead)
	got, data, operationErr := s.inner.ReadCompleteOwned(owner, evidence.Reference{SHA256: sha, Size: size, ContentType: contentType})
	readDone()
	after, err := s.finish(ctx)
	if err != nil {
		return Reference{}, nil, err
	}
	if operationErr != nil {
		return Reference{}, nil, operationErr
	}
	if got.RetainUntilUnixMS <= after.Latest.UnixMilli() {
		return Reference{}, nil, ErrUnavailable
	}
	return reference(o, got.SHA256, got.Size, got.ContentType, got.RetainUntilUnixMS), data, nil
}
