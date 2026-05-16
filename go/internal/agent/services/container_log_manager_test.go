package services

import (
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	otelpb "github.com/wendylabsinc/wendy/proto/gen/otelpb"
)

func TestContainerLogManager_SubscribeUnsubscribe(t *testing.T) {
	broadcaster := NewTelemetryBroadcaster()
	lm := NewContainerLogManager(zap.NewNop(), broadcaster)

	subID, ch := lm.Subscribe("test-app")
	if subID == "" {
		t.Fatal("expected non-empty subscription ID")
	}
	if ch == nil {
		t.Fatal("expected non-nil channel")
	}

	lm.Unsubscribe("test-app", subID)

	// Channel should be closed after unsubscribe.
	_, ok := <-ch
	if ok {
		t.Error("expected channel to be closed after unsubscribe")
	}
}

func TestContainerLogManager_MultipleSubscribers(t *testing.T) {
	broadcaster := NewTelemetryBroadcaster()
	lm := NewContainerLogManager(zap.NewNop(), broadcaster)

	id1, ch1 := lm.Subscribe("test-app")
	id2, ch2 := lm.Subscribe("test-app")
	defer lm.Unsubscribe("test-app", id1)
	defer lm.Unsubscribe("test-app", id2)

	output := ContainerOutput{Stdout: []byte("hello")}
	lm.Publish("test-app", output)

	for i, ch := range []<-chan ContainerOutput{ch1, ch2} {
		select {
		case got := <-ch:
			if string(got.Stdout) != "hello" {
				t.Errorf("subscriber %d: got stdout %q, want %q", i+1, got.Stdout, "hello")
			}
		case <-time.After(time.Second):
			t.Errorf("subscriber %d did not receive message", i+1)
		}
	}
}

func TestContainerLogManager_DifferentApps(t *testing.T) {
	broadcaster := NewTelemetryBroadcaster()
	lm := NewContainerLogManager(zap.NewNop(), broadcaster)

	id1, ch1 := lm.Subscribe("app-a")
	id2, ch2 := lm.Subscribe("app-b")
	defer lm.Unsubscribe("app-a", id1)
	defer lm.Unsubscribe("app-b", id2)

	lm.Publish("app-a", ContainerOutput{Stdout: []byte("from-a")})

	select {
	case got := <-ch1:
		if string(got.Stdout) != "from-a" {
			t.Errorf("app-a subscriber: got %q, want %q", got.Stdout, "from-a")
		}
	case <-time.After(time.Second):
		t.Error("app-a subscriber did not receive message")
	}

	// app-b should not receive app-a's message.
	select {
	case <-ch2:
		t.Error("app-b subscriber should not have received app-a's message")
	case <-time.After(50 * time.Millisecond):
		// Good: no message received.
	}
}

func TestContainerLogManager_PublishBridgesToTelemetry(t *testing.T) {
	broadcaster := NewTelemetryBroadcaster()
	lm := NewContainerLogManager(zap.NewNop(), broadcaster)

	// Subscribe to telemetry logs.
	telID, telCh := broadcaster.SubscribeLogs()
	defer broadcaster.UnsubscribeLogs(telID)

	lm.Publish("my-app", ContainerOutput{Stdout: []byte("hello from container")})

	select {
	case got := <-telCh:
		if got == nil || len(got.ResourceLogs) == 0 {
			t.Fatal("expected non-empty ResourceLogs")
		}
		rl := got.ResourceLogs[0]
		// Check service.name attribute.
		found := false
		for _, attr := range rl.Resource.Attributes {
			if attr.Key == "service.name" && attr.Value.GetStringValue() == "my-app" {
				found = true
			}
		}
		if !found {
			t.Error("expected service.name=my-app in resource attributes")
		}
		// Check log record body.
		records := rl.ScopeLogs[0].LogRecords
		if len(records) != 1 {
			t.Fatalf("expected 1 log record, got %d", len(records))
		}
		if records[0].Body.GetStringValue() != "hello from container" {
			t.Errorf("log body = %q; want %q", records[0].Body.GetStringValue(), "hello from container")
		}
		if records[0].SeverityNumber != otelpb.SeverityNumber_SEVERITY_NUMBER_INFO {
			t.Errorf("severity = %v; want INFO", records[0].SeverityNumber)
		}
	case <-time.After(time.Second):
		t.Error("did not receive telemetry log")
	}
}

func TestContainerLogManager_StderrBridgesAsWarn(t *testing.T) {
	broadcaster := NewTelemetryBroadcaster()
	lm := NewContainerLogManager(zap.NewNop(), broadcaster)

	telID, telCh := broadcaster.SubscribeLogs()
	defer broadcaster.UnsubscribeLogs(telID)

	lm.Publish("my-app", ContainerOutput{Stderr: []byte("error output")})

	select {
	case got := <-telCh:
		records := got.ResourceLogs[0].ScopeLogs[0].LogRecords
		if len(records) != 1 {
			t.Fatalf("expected 1 log record, got %d", len(records))
		}
		if records[0].SeverityNumber != otelpb.SeverityNumber_SEVERITY_NUMBER_WARN {
			t.Errorf("severity = %v; want WARN", records[0].SeverityNumber)
		}
		if records[0].Body.GetStringValue() != "error output" {
			t.Errorf("log body = %q; want %q", records[0].Body.GetStringValue(), "error output")
		}
	case <-time.After(time.Second):
		t.Error("did not receive telemetry log")
	}
}

func TestContainerLogManager_DoneNotBroadcast(t *testing.T) {
	broadcaster := NewTelemetryBroadcaster()
	lm := NewContainerLogManager(zap.NewNop(), broadcaster)

	telID, telCh := broadcaster.SubscribeLogs()
	defer broadcaster.UnsubscribeLogs(telID)

	// Done markers should not produce telemetry.
	lm.Publish("my-app", ContainerOutput{Done: true})

	select {
	case <-telCh:
		t.Error("should not have received telemetry for Done marker")
	case <-time.After(50 * time.Millisecond):
		// Good.
	}
}

func TestContainerLogManager_SlowSubscriber(t *testing.T) {
	broadcaster := NewTelemetryBroadcaster()
	lm := NewContainerLogManager(zap.NewNop(), broadcaster)

	id, _ := lm.Subscribe("test-app")
	defer lm.Unsubscribe("test-app", id)

	// Fill the subscriber buffer.
	for i := 0; i < 64; i++ {
		lm.Publish("test-app", ContainerOutput{Stdout: []byte("msg")})
	}

	// The 65th publish should not block.
	done := make(chan struct{})
	go func() {
		lm.Publish("test-app", ContainerOutput{Stdout: []byte("overflow")})
		close(done)
	}()

	select {
	case <-done:
		// Good: did not block.
	case <-time.After(time.Second):
		t.Error("Publish blocked on slow subscriber")
	}
}

func TestContainerLogManager_ConcurrentPublish(t *testing.T) {
	broadcaster := NewTelemetryBroadcaster()
	lm := NewContainerLogManager(zap.NewNop(), broadcaster)

	id, ch := lm.Subscribe("test-app")
	defer lm.Unsubscribe("test-app", id)

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			lm.Publish("test-app", ContainerOutput{Stdout: []byte("concurrent")})
		}()
	}

	received := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range n {
			select {
			case <-ch:
				received++
			case <-time.After(2 * time.Second):
				return
			}
		}
	}()

	wg.Wait()
	<-done

	if received == 0 {
		t.Error("expected at least some messages to be received")
	}
}

func TestContainerLogManager_UnsubscribeNonexistent(t *testing.T) {
	broadcaster := NewTelemetryBroadcaster()
	lm := NewContainerLogManager(zap.NewNop(), broadcaster)

	// Should not panic.
	lm.Unsubscribe("nonexistent-app", "nonexistent-sub")
}

// TestContainerLogManager_ConcurrentPublishUnsubscribe verifies that concurrent
// calls to Publish and Unsubscribe do not panic or cause a data race.
// The logSubscriber mutex ensures that closing a channel and sending to it
// cannot race, so no send-on-closed-channel panic can occur.
func TestContainerLogManager_ConcurrentPublishUnsubscribe(t *testing.T) {
	broadcaster := NewTelemetryBroadcaster()
	lm := NewContainerLogManager(zap.NewNop(), broadcaster)

	const iterations = 500

	for i := 0; i < iterations; i++ {
		subID, _ := lm.Subscribe("test-app")

		var wg sync.WaitGroup
		wg.Add(2)

		// Goroutine 1: publish once, racing against the unsubscribe below.
		go func() {
			defer wg.Done()
			lm.Publish("test-app", ContainerOutput{Stdout: []byte("msg")})
		}()

		// Goroutine 2: unsubscribe (closes the channel) concurrently.
		go func() {
			defer wg.Done()
			lm.Unsubscribe("test-app", subID)
		}()

		wg.Wait()
	}
	// If we reach here without panicking, the fix is working.
}
