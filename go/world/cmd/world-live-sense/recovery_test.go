package main

import (
	"context"
	"errors"
	"io"
	"net/url"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/livecapture"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type captureSigner struct{ versions []world.Version }

func (s *captureSigner) PreparePublication(c world.Component) (client.PreparedPublication, error) {
	s.versions = append(s.versions, c.Version)
	return client.PreparedPublication{}, nil
}
func TestCaptureRetryDoesNotResignAndNewCaptureSupersedes(t *testing.T) {
	signer := &captureSigner{}
	var pending capturePublication
	component := world.Component{Version: world.Version{Epoch: 9, Sequence: 10}}
	if err := pending.prepare(signer, 10, component); err != nil {
		t.Fatal(err)
	}
	if err := pending.prepare(signer, 10, component); err != nil {
		t.Fatal(err)
	}
	if len(signer.versions) != 1 {
		t.Fatal("same capture retry signed a different origin")
	}
	component.Version.Sequence = 11
	if err := pending.prepare(signer, 11, component); err != nil {
		t.Fatal(err)
	}
	if len(signer.versions) != 2 || signer.versions[1] != (world.Version{Epoch: 9, Sequence: 11}) {
		t.Fatal("fresh capture failed to supersede in the same epoch")
	}
	if err := pending.prepare(signer, 10, component); err == nil {
		t.Fatal("regressing capture was retried")
	}
}

type epochResult struct {
	epoch uint64
	err   error
}
type epochPublisher struct {
	calls   int
	results []epochResult
}

func (p *epochPublisher) NextEpoch(context.Context) (uint64, error) {
	r := p.results[p.calls]
	p.calls++
	return r.epoch, r.err
}
func TestEpochRetryAdoptsOnlyAcknowledgedEpoch(t *testing.T) {
	publisher := &epochPublisher{results: []epochResult{{epoch: 3, err: io.EOF}, {epoch: 4}}}
	epoch, err := awaitEpoch(context.Background(), publisher)
	if err != nil || epoch != 4 || publisher.calls != 2 {
		t.Fatal("unacknowledged epoch adopted", epoch, err)
	}
	publisher = &epochPublisher{results: []epochResult{{err: &client.HTTPStatusError{StatusCode: 403}}}}
	if _, err = awaitEpoch(context.Background(), publisher); err == nil || publisher.calls != 1 {
		t.Fatal("authority denial retried")
	}
}
func TestEpochWaitCancelsWithoutAllocatingAgain(t *testing.T) {
	publisher := &epochPublisher{results: []epochResult{{err: io.EOF}}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := awaitEpoch(ctx, publisher); !errors.Is(err, context.DeadlineExceeded) || publisher.calls != 1 {
		t.Fatal("canceled wait allocated another epoch", err)
	}
}

func TestCaptureRecoveryKeepsLineageAndAuthorityFailuresTerminal(t *testing.T) {
	for _, err := range []error{livecapture.ErrBootChanged, errors.New("live capture reused sequence with different content"), livecapture.HTTPError{Status: 401}, livecapture.HTTPError{Status: 403}, &url.Error{Op: "Get", URL: "http://scene", Err: errors.New("capture redirects forbidden")}} {
		if retryableCapture(err) {
			t.Fatalf("terminal capture fault would loop: %v", err)
		}
	}
	for _, err := range []error{livecapture.ErrStale, livecapture.HTTPError{Status: 408}, livecapture.HTTPError{Status: 503}, io.EOF, context.DeadlineExceeded} {
		if !retryableCapture(err) {
			t.Fatalf("capture availability fault not retryable: %v", err)
		}
	}
}
