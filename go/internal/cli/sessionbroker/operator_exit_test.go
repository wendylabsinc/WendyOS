//go:build !windows

package sessionbroker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

func exitFixtureContext(parent context.Context) (context.Context, *operatorExit) {
	exit := &operatorExit{}
	return context.WithValue(parent, operatorExitKey{}, exit), exit
}

func TestOperatorExitNormalServeStopsRetainNilReturn(t *testing.T) {
	for _, reason := range []string{"context_done", "state_unavailable", "idle"} {
		t.Run(reason, func(t *testing.T) {
			dir := shortTempDir(t)
			upstream, cleanup := startUpstream(t)
			defer cleanup()
			parent, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			ctx, exit := exitFixtureContext(parent)
			spec := Spec{Key: "exit-fixture", Host: "fixture.invalid"}
			_, state, done := startServing(t, ctx, dir, spec, upstream, 40*time.Millisecond)
			cause := errors.New("private caller cancellation cause")
			switch reason {
			case "context_done":
				cancel(cause)
			case "state_unavailable":
				if err := os.Remove(state); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("diagnostics changed successful stop return: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("serve did not stop")
			}
			got := exit.first.Load()
			if got == nil || got.reason != reason {
				t.Fatalf("stop reason lost: %+v", got)
			}
			if reason == "context_done" && !errors.Is(got.err, cause) {
				t.Fatal("original context cancellation cause was discarded")
			}
			if reason != "context_done" && parent.Err() != nil {
				t.Fatal("diagnostic annotation canceled an uncanceled parent")
			}
		})
	}
}

func TestOperatorExitAttachmentCausePrecedesCancellation(t *testing.T) {
	for _, tc := range []struct {
		err    error
		reason string
	}{
		{errors.New("private terminal attachment error"), "attachment_stopped"},
		{errOperatorCredentialsChanged, "credentials_changed"},
		{errOperatorDisconnected, "disconnected"},
		{nil, "attachment_stopped"},
	} {
		ctx, exit := exitFixtureContext(context.Background())
		canceled := false
		runOperatorAttachment(ctx, func() {
			got := exit.first.Load()
			if got == nil || got.reason != tc.reason || !errors.Is(got.err, tc.err) {
				t.Fatalf("attachment cause missing before cancel: %+v", got)
			}
			canceled = true
		}, func() error { return tc.err })
		if !canceled || ctx.Err() != nil {
			t.Fatal("changed attachment cancellation behavior")
		}
	}
}

func TestOperatorExitRecordPreservesCarrierAndBoundsSanitizedReasons(t *testing.T) {
	dir, spec := operatorDiagnosticsFixture(t)
	d := newOperatorDiagnostics(dir, spec)
	d.record("tcp", context.DeadlineExceeded, false)
	carrierAt := d.last.At
	d.now = func() time.Time { return time.UnixMilli(carrierAt).Add(500 * time.Millisecond) }
	for _, reason := range []string{"context_done", "state_unavailable", "idle", "upstream_unavailable", "listener_failed", "server_ended", "startup_failed", "attachment_stopped", "credentials_changed", "disconnected", "intent_changed"} {
		d.recordExit(&operatorExitCause{reason: reason, err: errors.New("private address/key/token payload")})
		data, err := os.ReadFile(d.path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(d.path)
		if err != nil {
			t.Fatal(err)
		}
		var record operatorFailure
		if json.Unmarshal(data, &record) != nil || record.ExitReason != reason || record.ExitKind != "attachment_failed" || record.Kind != "timeout" || record.Transport != "tcp" || record.At != carrierAt || record.ExitAt != carrierAt+500 {
			t.Fatalf("terminal annotation lost concrete carrier cause: %s", data)
		}
		if len(data) > operatorFailureBytes || info.Mode().Perm() != 0600 || strings.Contains(string(data), "private") || strings.Contains(string(data), "payload") {
			t.Fatal("terminal receipt is unbounded, nonprivate or unsanitized")
		}
	}
	// A nil successful return and a canceled return must both remain visible.
	for _, err := range []error{nil, context.Canceled} {
		d = newOperatorDiagnostics(dir, spec)
		d.recordExit(&operatorExitCause{reason: "context_done", err: err})
		if d.last == nil || d.last.Kind != "broker_exit" || d.last.ExitReason != "context_done" {
			t.Fatal("nil/canceled terminal event was discarded")
		}
		if err != nil && d.last.ExitKind != "canceled" {
			t.Fatal("cancellation classification lost")
		}
	}
}

func TestOperatorExitFirstCauseIsNonblockingAndCannotBeOverwritten(t *testing.T) {
	ctx, exit := exitFixtureContext(context.Background())
	cause := errors.New("original cause")
	noteOperatorExit(ctx, "state_unavailable", cause)
	var writers sync.WaitGroup
	for range 128 {
		writers.Add(1)
		go func() { defer writers.Done(); noteOperatorExit(ctx, "context_done", context.Canceled) }()
	}
	writers.Wait()
	got := exit.first.Load()
	if got.reason != "state_unavailable" || !errors.Is(got.err, cause) {
		t.Fatal("consequential cancellation overwrote the initiating reason")
	}
	noteOperatorExit(context.Background(), "context_done", cause) // nil-default callback
	ctx, exit = exitFixtureContext(context.Background())
	noteOperatorExit(ctx, "arbitrary remote reason", cause)
	if exit.first.Load() != nil {
		t.Fatal("unbounded reason accepted")
	}
}

func TestOperatorExitNominationDoesNotWaitForDiagnosticSink(t *testing.T) {
	dir, spec := operatorDiagnosticsFixture(t)
	d := newOperatorDiagnostics(dir, spec)
	ctx, exit := exitFixtureContext(context.WithValue(context.Background(), operatorDiagnosticsKey{}, d))
	d.mu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runOperatorAttachment(ctx, func() {}, func() error { return errOperatorDisconnected })
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		d.mu.Unlock()
		t.Fatal("actor/cancel path waited for diagnostic writer")
	}
	d.mu.Unlock()
	if exit.first.Load() == nil {
		t.Fatal("actor cause lost")
	}
	if _, err := os.Stat(d.path); !os.IsNotExist(err) {
		t.Fatal("actor wrote receipt before owning Run joined producers")
	}
	d.recordExit(exit.first.Load())
	if d.last == nil || d.last.ExitReason != "disconnected" {
		t.Fatal("joined owner could not persist cause")
	}
}

func TestOperatorExitColdStartupRetainsSpecificRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, kind, explanation, step string
		err                           error
		retry                         bool
	}{
		{"timeout", "timeout", "timed out", "reachable", context.DeadlineExceeded, true},
		{"issuer", "issuer_chain", "issuer chain", "certificate chain", &certs.IssuerPathError{Err: errors.New("private issuer payload")}, false},
		{"pin", "device_pin", "saved device identity", "saved certificate pin", operatorFailurePinRejection{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, spec := operatorDiagnosticsFixture(t)
			d := newOperatorDiagnostics(dir, spec)
			d.recordExit(&operatorExitCause{reason: "startup_failed", err: tc.err})
			got := operatorStartupError(spec, context.DeadlineExceeded)
			retry, steps := OperatorRecovery(got)
			if d.last.Kind != tc.kind || retry != tc.retry || !strings.Contains(got.Error(), tc.explanation) || !strings.Contains(strings.Join(steps, " "), tc.step) {
				t.Fatalf("cold startup recovery changed: record=%+v retry=%v steps=%v error=%v", d.last, retry, steps, got)
			}
			if strings.Contains(got.Error(), "private") {
				t.Fatal("raw startup payload leaked")
			}
		})
	}
}
