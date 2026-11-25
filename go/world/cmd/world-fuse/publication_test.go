package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type uncertainPublisher struct {
	prepared []world.Component
	sent     []world.Component
	fail     bool
}

func (p *uncertainPublisher) PreparePublication(c world.Component) (client.PreparedPublication, error) {
	p.prepared = append(p.prepared, c)
	return client.PreparedPublication{}, nil
}
func (p *uncertainPublisher) PublishPrepared(_ context.Context, _ client.PreparedPublication) error {
	p.sent = append(p.sent, p.prepared[len(p.prepared)-1])
	if p.fail {
		return io.EOF
	}
	return nil
}
func hazard(now time.Time, ttl time.Duration) world.Component {
	return world.Component{Key: world.Key{Entity: "hazard"}, Timing: world.Timing{SampleTime: now}, TTL: ttl, Payload: []byte(`{"hazard":true}`)}
}
func update(t *testing.T, s *publicationState, now time.Time, c *world.Component, input string) {
	t.Helper()
	if err := s.update(now, c, input); err != nil {
		t.Fatal(err)
	}
}
func send(t *testing.T, s *publicationState, now time.Time, wantError bool) *world.Component {
	t.Helper()
	c, err := s.send(context.Background(), now)
	if (err != nil) != wantError {
		t.Fatalf("send error=%v want error=%v", err, wantError)
	}
	return c
}
func TestUncertainDeliveryRetriesOriginalWithoutNewSequenceOrExpiry(t *testing.T) {
	now := time.Unix(100, 0)
	publisher := &uncertainPublisher{fail: true}
	state := publicationState{publisher: publisher, epoch: 12}
	c := hazard(now, 5*time.Second)
	update(t, &state, now, &c, "inputs-1")
	send(t, &state, now, true)
	// The same evidence seen later cannot extend the first signed claim.
	later := hazard(now.Add(time.Second), 5*time.Second)
	update(t, &state, now.Add(time.Second), &later, "inputs-1")
	publisher.fail = false
	send(t, &state, now.Add(time.Second), false)
	if len(publisher.prepared) != 1 || len(publisher.sent) != 2 {
		t.Fatal("retry prepared another origin record")
	}
	if c := publisher.sent[1]; c.Version != (world.Version{Epoch: 12, Sequence: 1}) || !expiry(c).Equal(now.Add(5*time.Second)) {
		t.Fatal("retry changed version or expiry")
	}
	if send(t, &state, now.Add(2*time.Second), false) != nil || len(publisher.sent) != 2 {
		t.Fatal("acknowledged record resent")
	}
}
func TestNewNegativeWithdrawsPossiblyAcceptedHazardAndRetriesTombstone(t *testing.T) {
	now := time.Unix(100, 0)
	publisher := &uncertainPublisher{fail: true}
	state := publicationState{publisher: publisher, epoch: 12}
	c := hazard(now, 5*time.Second)
	update(t, &state, now, &c, "inputs-1")
	send(t, &state, now, true)
	update(t, &state, now.Add(time.Second), nil, "")
	send(t, &state, now.Add(time.Second), true)
	update(t, &state, now.Add(2*time.Second), nil, "")
	publisher.fail = false
	send(t, &state, now.Add(2*time.Second), false)
	if len(publisher.prepared) != 2 || len(publisher.sent) != 3 {
		t.Fatal("withdrawal did not retain a bounded retry")
	}
	for _, withdrawal := range publisher.sent[1:] {
		if !withdrawal.Tombstone || withdrawal.Payload != nil || withdrawal.Version != (world.Version{Epoch: 12, Sequence: 2}) || !expiry(withdrawal).Equal(expiry(c)) || !withdrawal.Timing.SampleTime.Equal(c.Timing.SampleTime) {
			t.Fatal("withdrawal failed to cover uncertain accepted origin")
		}
	}
	if state.possible != nil {
		t.Fatal("acknowledged withdrawal left possible live claim")
	}
}
func TestNewInputsSupersedePendingAndNegativeCoversLongestPossibleClaim(t *testing.T) {
	now := time.Unix(100, 0)
	publisher := &uncertainPublisher{}
	state := publicationState{publisher: publisher, epoch: 3}
	long := hazard(now, 10*time.Second)
	update(t, &state, now, &long, "old")
	send(t, &state, now, false)
	short := hazard(now.Add(time.Second), 3*time.Second)
	publisher.fail = true
	update(t, &state, now.Add(time.Second), &short, "new")
	send(t, &state, now.Add(time.Second), true)
	if publisher.sent[1].Version.Sequence != 2 {
		t.Fatal("new inputs did not advance sequence")
	}
	update(t, &state, now.Add(2*time.Second), nil, "")
	publisher.fail = false
	withdrawal := send(t, &state, now.Add(2*time.Second), false)
	if withdrawal == nil || !withdrawal.Tombstone || withdrawal.Version != (world.Version{Epoch: 3, Sequence: 3}) || !expiry(*withdrawal).Equal(expiry(long)) {
		t.Fatal("short uncertain replacement allowed long accepted claim to survive")
	}
}
func TestExpiredPendingNeverRepublishedOrExtended(t *testing.T) {
	now := time.Unix(100, 0)
	publisher := &uncertainPublisher{fail: true}
	state := publicationState{publisher: publisher, epoch: 2}
	c := hazard(now, time.Second)
	update(t, &state, now, &c, "old")
	send(t, &state, now, true)
	update(t, &state, now.Add(time.Second), &c, "old")
	if send(t, &state, now.Add(time.Second), false) != nil || len(publisher.sent) != 1 || state.current != nil || state.possible != nil {
		t.Fatal("expired uncertain record was retained or resent")
	}
	fresh := hazard(now.Add(2*time.Second), time.Second)
	update(t, &state, now.Add(2*time.Second), &fresh, "fresh")
	publisher.fail = false
	published := send(t, &state, now.Add(2*time.Second), false)
	if published == nil || published.Version != (world.Version{Epoch: 2, Sequence: 2}) {
		t.Fatal("recovery changed epoch or reused old sequence")
	}
}
func TestUnsentCandidateDoesNotNeedWithdrawalAndCanceledSendIsNotPossible(t *testing.T) {
	now := time.Unix(100, 0)
	publisher := &uncertainPublisher{}
	state := publicationState{publisher: publisher, epoch: 1}
	c := hazard(now, time.Second)
	update(t, &state, now, &c, "old")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := state.send(ctx, now); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	update(t, &state, now, nil, "")
	if state.current != nil || state.possible != nil || len(publisher.sent) != 0 {
		t.Fatal("unsent candidate became a remote claim")
	}
}
func TestRetryBackoffIsBoundedAndResets(t *testing.T) {
	now := time.Unix(100, 0)
	var retry retryDelay
	for _, want := range []time.Duration{400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond, 3200 * time.Millisecond, 5 * time.Second, 5 * time.Second} {
		retry.failed(now)
		if retry.delay != want || retry.ready(now) || !retry.ready(now.Add(want)) {
			t.Fatal("incorrect bounded retry", retry)
		}
		now = now.Add(want)
	}
	retry.reset()
	if !retry.ready(now) {
		t.Fatal("healthy recovery did not reset retry")
	}
}
