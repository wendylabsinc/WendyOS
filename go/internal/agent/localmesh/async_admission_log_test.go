package localmesh

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/shared/asyncdiag"
	"log"
	"sync"
	"testing"
	"time"
)

type reviewBlockedAdmissionWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *reviewBlockedAdmissionWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}
func TestClockAdmissionSinkDoesNotBlockPublication(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := asyncdiag.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	n := routePublicationNode(t)
	w := &reviewBlockedAdmissionWriter{entered: make(chan struct{}), release: make(chan struct{})}
	old := log.Writer()
	log.SetOutput(w)
	var once sync.Once
	release := func() { once.Do(func() { close(w.release) }) }
	defer func() {
		release()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := asyncdiag.Flush(ctx); err != nil {
			t.Error(err)
		}
		log.SetOutput(old)
	}()
	done := make(chan error, 1)
	go func() { done <- n.Persist(n.routing.Checkpoint()) }()
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("fixture did not reach diagnostic sink")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		release()
		<-done
		t.Fatal("committed publication and serialized Babel Persist callback blocked on diagnostic sink")
	}
}
