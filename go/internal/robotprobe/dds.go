package robotprobe

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/rtps"
)

// Lease is the part of an rtps.Lease a reader uses. Taking it as an interface keeps the
// sampling logic testable against recorded endpoints and samples, with no DDS domain and
// no robot.
type Lease interface {
	Endpoints() []rtps.Endpoint
	Subscribe(rtps.Endpoint) error
	Samples() <-chan rtps.Sample
	Done() <-chan struct{}
}

// DDSReader samples ROS 2 topics over an RTPS participant, without a ROS installation on
// either side. It satisfies TopicReader, so probes neither know nor care that this is
// DDS underneath.
//
// A lease carries one sample channel shared by every subscription on it, so samples are
// filtered by the writer's GUID. Without that, sampling two topics at once would credit
// one topic's messages to the other and report a rate that no publisher produced.
type DDSReader struct {
	lease Lease

	mu         sync.Mutex
	subscribed map[rtps.GUID]struct{}
}

// NewDDSReader wraps a lease whose discovery has already settled, so the caller decides
// how long to wait for the graph to appear.
func NewDDSReader(lease Lease) *DDSReader {
	return &DDSReader{lease: lease, subscribed: map[rtps.GUID]struct{}{}}
}

// TopicsOfType returns the ROS-style names of every discovered topic published with the
// given DDS type, so a caller can ask "which cameras are there" rather than being told.
func (r *DDSReader) TopicsOfType(typeName string) []string {
	var topics []string
	seen := map[string]struct{}{}
	for _, ep := range r.lease.Endpoints() {
		if ep.Type != typeName {
			continue
		}
		name := rosTopicName(ep.Topic)
		if _, already := seen[name]; already {
			continue
		}
		seen[name] = struct{}{}
		topics = append(topics, name)
	}
	return topics
}

// Sample collects payloads published on topic for at most window, stopping early once
// maxMessages have arrived. A topic nobody publishes on returns no payloads and no
// error: that is a finding for the caller to report, not a transport failure.
func (r *DDSReader) Sample(ctx context.Context, topic, typeName string, window time.Duration, maxMessages int) ([][]byte, error) {
	endpoint, ok := r.findEndpoint(topic, typeName)
	if !ok {
		return nil, nil
	}
	if err := r.subscribe(endpoint); err != nil {
		return nil, fmt.Errorf("subscribing to %s: %w", topic, err)
	}

	deadline := time.NewTimer(window)
	defer deadline.Stop()

	var payloads [][]byte
	for {
		if maxMessages > 0 && len(payloads) >= maxMessages {
			return payloads, nil
		}
		select {
		case <-ctx.Done():
			// Whatever arrived before the caller gave up is still worth returning.
			return payloads, ctx.Err()
		case <-deadline.C:
			return payloads, nil
		case <-r.lease.Done():
			return payloads, fmt.Errorf("participant closed while sampling %s", topic)
		case sample := <-r.lease.Samples():
			if sample.Writer != endpoint.GUID {
				continue
			}
			payloads = append(payloads, sample.Payload)
		}
	}
}

// findEndpoint resolves a ROS topic name against the discovered graph. An empty typeName
// matches any type, which is how a caller samples a topic whose type it has not pinned.
func (r *DDSReader) findEndpoint(topic, typeName string) (rtps.Endpoint, bool) {
	for _, ep := range r.lease.Endpoints() {
		if typeName != "" && ep.Type != typeName {
			continue
		}
		if rosTopicName(ep.Topic) == rosTopicName(topic) {
			return ep, true
		}
	}
	return rtps.Endpoint{}, false
}

// subscribe is idempotent per writer, so sampling the same topic twice in one pass does
// not stack subscriptions on the participant.
func (r *DDSReader) subscribe(ep rtps.Endpoint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, already := r.subscribed[ep.GUID]; already {
		return nil
	}
	if err := r.lease.Subscribe(ep); err != nil {
		return err
	}
	r.subscribed[ep.GUID] = struct{}{}
	return nil
}

// rosTopicName undoes the mangling ROS 2 applies on the DDS wire, where a topic is
// published as "rt" plus the ROS name. Normalising both sides means a caller can ask for
// either spelling and a report always prints the name an operator would type.
func rosTopicName(topic string) string {
	// Trim "rt/" and not "rt": trimming the bare prefix turned a native DDS topic
	// named rtps_status into /ps_status. This matches ros2camera.TopicName, which
	// already had it right.
	name := strings.TrimPrefix(topic, "rt/")
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	return name
}

// participantLease adapts a directly created Participant to the Lease seam. A pooled
// lease reports its own closure through Done(); a bare participant is driven by
// Run(ctx), so its liveness is that context's.
type participantLease struct {
	*rtps.Participant
	done <-chan struct{}
}

func (p participantLease) Done() <-chan struct{} { return p.done }

// NewParticipantLease presents a participant the caller is running itself as a Lease.
// done should be the Done channel of the context passed to Participant.Run, so a reader
// stops waiting when the participant stops being driven.
func NewParticipantLease(p *rtps.Participant, done <-chan struct{}) Lease {
	return participantLease{Participant: p, done: done}
}
