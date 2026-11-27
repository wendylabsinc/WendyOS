package serviceevidence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
)

func TestTimingCompleteReadPreservesClockCallsAndBytes(t *testing.T) {
	cfg, clock := fixture(t)
	s := open(t, cfg)
	body := []byte("exact protected media")
	req := request(body)
	if _, err := s.Put(context.Background(), owner(), req); err != nil {
		t.Fatal(err)
	}
	before := clock.calls
	plain, data, err := s.ReadComplete(context.Background(), owner(), req.SHA256, req.Size, req.ContentType)
	if err != nil {
		t.Fatal(err)
	}
	plainCalls := clock.calls - before
	sink, _ := rpctrace.New(rpctrace.Config{})
	ctx, finish := rpctrace.Begin(context.Background(), sink, rpctrace.Labels{Method: rpctrace.MethodGetEvidence, App: owner().App, Service: owner().Service, Generation: 7})
	before = clock.calls
	traced, tracedData, err := s.ReadComplete(ctx, owner(), req.SHA256, req.Size, req.ContentType)
	if err != nil {
		t.Fatal(err)
	}
	tracedCalls := clock.calls - before
	finish(rpctrace.OK)
	if plain != traced || !bytes.Equal(data, tracedData) || !bytes.Equal(data, body) || plainCalls != tracedCalls || plainCalls == 0 {
		t.Fatalf("read/authority changed: %d %d", plainCalls, tracedCalls)
	}
	if !s.mu.TryLock() {
		t.Fatal("read retained store lock")
	}
	s.mu.Unlock()
	sink.Close()
	var out bytes.Buffer
	if err := sink.Run(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	var event rpctrace.Event
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &event); err != nil {
		t.Fatal(err)
	}
	counts := map[string]uint64{}
	for _, stage := range event.Stages {
		counts[stage.Stage] = stage.Count
	}
	if counts["evidence_wait"] != 1 || counts["evidence_read"] != 1 || counts["evidence_floor"] != 2 {
		t.Fatalf("wrong trace: %+v", counts)
	}
}
func TestTimingCanceledReadReleasesStore(t *testing.T) {
	cfg, _ := fixture(t)
	s := open(t, cfg)
	body := []byte("same data after cancellation")
	req := request(body)
	if _, err := s.Put(context.Background(), owner(), req); err != nil {
		t.Fatal(err)
	}
	sink, _ := rpctrace.New(rpctrace.Config{})
	base, cancel := context.WithCancel(context.Background())
	ctx, finish := rpctrace.Begin(base, sink, rpctrace.Labels{Method: rpctrace.MethodGetEvidence, App: owner().App, Service: owner().Service, Generation: 7})
	cancel()
	_, got, err := s.ReadComplete(ctx, owner(), req.SHA256, req.Size, req.ContentType)
	if !errors.Is(err, context.Canceled) || len(got) != 0 {
		t.Fatal(err)
	}
	if !s.mu.TryLock() {
		t.Fatal("canceled read retained store lock")
	}
	s.mu.Unlock()
	finish(rpctrace.Canceled)
	sink.Close()
	var out bytes.Buffer
	if err := sink.Run(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if sink.Stats().Written != 1 {
		t.Fatal("cancellation lost trace cleanup")
	}
	if _, got, err = s.ReadComplete(context.Background(), owner(), req.SHA256, req.Size, req.ContentType); err != nil || !bytes.Equal(got, body) {
		t.Fatal("cancellation changed store", err)
	}
}
