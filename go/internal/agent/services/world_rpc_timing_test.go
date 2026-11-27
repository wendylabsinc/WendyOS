package services

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
)

type worldTimingWriter struct{ written chan []byte }

func (w worldTimingWriter) Write(p []byte) (int, error) {
	w.written <- append([]byte(nil), p...)
	return len(p), nil
}

func TestWorldRPCTimingRequiresExactOptIn(t *testing.T) {
	writer := worldTimingWriter{written: make(chan []byte, 1)}
	for _, value := range []string{"", "0", "true", "yes", " 1", "1 ", "01"} {
		if got := worldRPCTimingFor(context.Background(), value, writer); got != nil {
			got.Close()
			t.Fatalf("enabled by %q", value)
		}
	}
	select {
	case <-writer.written:
		t.Fatal("disabled timing wrote output")
	default:
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := worldRPCTimingFor(ctx, "1", writer)
	if sink == nil {
		t.Fatal("explicit timing was unavailable")
	}
	defer sink.Close()
	trace, finish := rpctrace.Begin(ctx, sink, rpctrace.Labels{Method: rpctrace.MethodQuery, App: "com.example.test", Service: "reader", Generation: 7})
	rpctrace.Start(trace, rpctrace.StageConfig)()
	finish(rpctrace.OK)
	sink.Close()
	select {
	case raw := <-writer.written:
		var e rpctrace.Event
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatal(err)
		}
		if e.Method != "Query" || e.Service != "reader" || e.Generation != 7 || !e.Inclusive {
			t.Fatalf("unexpected event %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("separate trace worker did not write")
	}
	if !sink.Stats().Closed {
		t.Fatal("close did not seal")
	}
}
