package robotwizard

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotcal"
)

// fakeSource is a robot that reports joints and nothing else. One joint moves
// at a time, chosen by the test, which is what lets the joint-map check be
// exercised without a robot: "the operator was asked for A and B moved" is just
// moving being set to B.
type fakeSource struct {
	mu sync.Mutex
	// rest is where every joint sits when it is not being moved.
	rest map[string]float64
	// order is what the source reports its joint order as.
	order []string
	// moving are the joints that respond to this step's sweep. More than one,
	// because a limp robot does not move only the joint you push: an arm
	// hanging under gravity swings its distal joints too, which is what the
	// joint-map margin exists for.
	moving []movedJoint
	high   bool
	closed bool
	tick   chan struct{}
	err    error
}

func newFakeSource(order []string) *fakeSource {
	rest := make(map[string]float64, len(order))
	for _, j := range order {
		rest[j] = 0
	}
	return &fakeSource{rest: rest, order: order, tick: make(chan struct{}, 4096)}
}

// movedJoint is one joint that moves during a step, and how far.
type movedJoint struct {
	name string
	span robotcal.Span
}

// sweep tells the fake which joints the next step will actually move.
func (f *fakeSource) sweep(moved ...movedJoint) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.moving, f.high = moved, false
}

func (f *fakeSource) Read(ctx context.Context) (JointReading, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return JointReading{}, f.err
	}
	positions := make(map[string]float64, len(f.rest))
	for j, v := range f.rest {
		positions[j] = v
	}
	// Alternating between the extremes gives a span whose first and last
	// readings differ, which is what DirectionSign reads.
	for _, m := range f.moving {
		if f.high {
			positions[m.name] = m.span.Max
		} else {
			positions[m.name] = m.span.Min
		}
	}
	if len(f.moving) > 0 {
		f.high = !f.high
	}
	select {
	case f.tick <- struct{}{}:
	default:
	}
	return JointReading{At: time.Now(), Positions: positions, Order: f.order}, nil
}

func (f *fakeSource) Describe() string { return "fake joint source" }

func (f *fakeSource) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// waitFor blocks until n more readings have been taken, so a test never depends
// on a sleep to get enough samples.
func (f *fakeSource) waitFor(n int) {
	deadline := time.After(5 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-f.tick:
		case <-deadline:
			return
		}
	}
}

// step is one scripted operator action.
type step struct {
	// sweeps names the joint that will actually move when this step runs, and
	// how far. An empty name means nothing moves.
	sweeps   string
	min, max float64
	// also is a second joint that moves during the same step — a limp joint
	// dragged along by the one being swept.
	also             string
	alsoMin, alsoMax float64
	action           StepAction
	// samples is how many readings to let through before answering.
	samples int
}

// fakePrompter plays a scripted operator.
type fakePrompter struct {
	t       *testing.T
	source  *fakeSource
	confirm bool
	choice  int
	steps   []step
	at      int

	announced []string
	infos     []string
}

func (p *fakePrompter) Announce(heading string, lines ...string) {
	p.announced = append(p.announced, heading)
	p.announced = append(p.announced, lines...)
}

func (p *fakePrompter) Infof(format string, args ...any) {
	p.infos = append(p.infos, fmt.Sprintf(format, args...))
}

func (p *fakePrompter) Confirm(string) (bool, error) { return p.confirm, nil }

func (p *fakePrompter) Choose(string, []string) (int, error) { return p.choice, nil }

func (p *fakePrompter) Step(string) (StepAction, error) {
	if p.at >= len(p.steps) {
		p.t.Fatalf("the wizard asked for more steps than the script has (%d)", len(p.steps))
	}
	s := p.steps[p.at]
	p.at++
	var moved []movedJoint
	if s.sweeps != "" {
		moved = append(moved, movedJoint{s.sweeps, robotcal.Span{Min: s.min, Max: s.max}})
	}
	if s.also != "" {
		moved = append(moved, movedJoint{s.also, robotcal.Span{Min: s.alsoMin, Max: s.alsoMax}})
	}
	p.source.sweep(moved...)
	if s.samples > 0 {
		p.source.waitFor(s.samples)
	}
	// Stop moving before answering, so the sampling that runs on into the next
	// step's window does not carry this joint's travel with it.
	p.source.sweep()
	return s.action, nil
}

// announcedContains reports whether any announced line contains sub.
func (p *fakePrompter) announcedContains(sub string) bool {
	for _, l := range p.announced {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}
