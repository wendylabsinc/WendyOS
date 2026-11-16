package meshsession

import (
	"context"
	"errors"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/directstream"
)

// An unadmitted app open can receive the use-upper reply immediately before
// the old, quiescent epoch finishes. Both notifications can be ready together.
func TestDirectDialKeepsCompletedReplyWhenEpochRetires(t *testing.T) {
	for i := 0; i < 128; i++ {
		ctx, cancel := context.WithCancelCause(context.Background())
		s := &DirectSession{ctx: ctx, done: make(chan struct{}), dial: make(chan *directRequest)}
		result := make(chan error, 1)
		go func() {
			_, err := s.Dial(context.Background(), directstream.OpenRequest{Service: directstream.ServiceTCP, Port: 28471})
			result <- err
		}()
		request := <-s.dial
		request.finish(nil, ErrDirectUseUpper)
		cancel(errDirectEpochRetired)
		close(s.done)
		if err := <-result; !errors.Is(err, ErrDirectUseUpper) {
			t.Fatalf("completed app reply lost to epoch retirement: %v", err)
		}
	}
}
