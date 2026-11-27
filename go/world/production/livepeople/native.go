package livepeople

import (
	"bytes"
	"context"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
)

// NewNative uses the mounted Wendy Agent's complete Query admission for this
// Step's current-source witness. It still verifies original signatures, typed
// captures, exact images and clocks. The Agent's Publish must revalidate exact
// retained dependencies, current source authority and media before committing.
// Clients without that native admission contract must use New.
func NewNative(c Config, api appv1.WorldAppServiceClient, infer InferFunc) (*Session, error) {
	s, err := New(c, api, infer)
	if err != nil {
		return nil, err
	}
	s.native = true
	return s, nil
}

// RunNative selects NewNative for the ordinary mounted World application.
func RunNative(ctx context.Context, c Config, api appv1.WorldAppServiceClient, infer InferFunc, emit func(Result) error) (Summary, error) {
	return run(ctx, c, api, infer, emit, NewNative)
}

type admittedKey struct{}
type admittedOriginals struct {
	owner  *Session
	proofs []proof
}

// The witness is private to one locked Step, created only after its complete
// Query succeeded. Invalidate it on every return, including unknown Publish
// outcomes. It is never stored on Session or used to renew an original.
func (s *Session) admittedContext(ctx context.Context, current []proof) (context.Context, func()) {
	w := &admittedOriginals{owner: s, proofs: append([]proof(nil), current...)}
	return context.WithValue(ctx, admittedKey{}, w), func() { w.owner, w.proofs = nil, nil }
}

func (s *Session) authorizationOriginals(ctx context.Context) ([]proof, error) {
	if !s.native {
		return s.query(ctx)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	w, ok := ctx.Value(admittedKey{}).(*admittedOriginals)
	if !ok || w.owner != s {
		return nil, ErrSource
	}
	return w.proofs, nil
}

func containsOriginal(current []proof, old proof) bool {
	for _, p := range current {
		if p.reference == old.reference && bytes.Equal(p.raw, old.raw) {
			return true
		}
	}
	return false
}
