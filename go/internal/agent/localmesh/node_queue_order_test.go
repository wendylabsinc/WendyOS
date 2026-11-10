package localmesh

import (
	"context"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

type orderedControlLink struct {
	expiryNodeLink
	first, release chan struct{}
	observed       chan string
	writes         int
}

func (l *orderedControlLink) WriteControl(m ControlMessage) error {
	l.observed <- m.Principal
	l.writes++
	if l.writes == 1 {
		close(l.first)
		<-l.release
	}
	runtime.Gosched()
	return nil
}

// Use the production dequeue/writer, rather than an artificial consumer that
// does not participate in its queue ownership contract.
func TestIdleSweepPreservesConcurrentControlWriterOrder(t *testing.T) {
	previous := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(previous)
	const count = 64
	for attempt := range 200 {
		link := &orderedControlLink{first: make(chan struct{}), release: make(chan struct{}), observed: make(chan string, count)}
		p := &nodePeer{control: make(chan ControlMessage, count), link: link}
		n := &Node{peers: map[babel.LinkID]*nodePeer{1: p}}
		for index := range count {
			p.control <- ControlMessage{Kind: "identity-missing", Principal: strconv.Itoa(index)}
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- n.writeControls(ctx, p) }()
		<-link.first
		close(link.release)
		n.sweepAnnouncements(time.Now())
		for index := range count {
			select {
			case got := <-link.observed:
				if got != strconv.Itoa(index) {
					cancel()
					t.Fatalf("idle sweep reordered controls at attempt%d index%d: %s", attempt, index, got)
				}
			case <-time.After(time.Second):
				cancel()
				t.Fatal("control writer stalled")
			}
		}
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("control writer did not stop")
		}
	}
}
