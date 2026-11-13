package analytics

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestActivationFollowupsMatureAtThresholdOnce(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	activationFollowups(dir, "install_finished", "completed", start, true)
	for _, tc := range []struct {
		elapsed time.Duration
		want    []string
	}{
		{24*time.Hour - time.Nanosecond, nil},
		{24 * time.Hour, []string{"next_launch_no_run_24h"}},
		{25 * time.Hour, nil},
		{7*24*time.Hour - time.Nanosecond, nil},
		{7 * 24 * time.Hour, []string{"next_launch_no_run_7d"}},
		{8 * 24 * time.Hour, nil},
	} {
		pending := activationFollowups(dir, "cli_seen", "completed", start.Add(tc.elapsed), true)
		if got := followupNames(pending); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("at %v: got %v, want %v", tc.elapsed, got, tc.want)
		}
		for _, followup := range pending {
			acknowledgeActivationFollowup(dir, followup)
		}
	}
	info, err := os.Stat(filepath.Join(dir, activationStateFile))
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		t.Fatalf("state must be private: %v, %v", info, err)
	}
}

func TestActivationFollowupsFirstReturnAfterWeekEmitsBoth(t *testing.T) {
	dir, now := t.TempDir(), time.Now().UTC()
	activationFollowups(dir, "install_finished", "completed", now, true)
	want := []string{"next_launch_no_run_24h", "next_launch_no_run_7d"}
	if got := followupNames(activationFollowups(dir, "cli_seen", "completed", now.Add(8*24*time.Hour), true)); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func followupNames(followups []activationFollowup) []string {
	var names []string
	for _, followup := range followups {
		names = append(names, followup.event)
	}
	return names
}

func TestActivationFollowupsRetryUntilDelivered(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WENDY_CONFIG_DIR", dir)
	start := time.Now().UTC().Add(-8 * 24 * time.Hour)
	activationFollowups(dir, "install_finished", "completed", start, true)
	before, err := os.ReadFile(filepath.Join(dir, activationStateFile))
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	bodies := make(chan eventPayload, 6)
	enableTestDelivery(t, func(w http.ResponseWriter, r *http.Request) {
		var body eventPayload
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode follow-up: %v", err)
		}
		bodies <- body
		if requests.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	ctx := WithEntrypoint(WithAttempt(context.Background()), "mcp")
	trackActivationFollowups(ctx, "cli_seen", "wendy mcp serve", "completed")
	Flush()
	afterFailure, err := os.ReadFile(filepath.Join(dir, activationStateFile))
	if err != nil || string(afterFailure) != string(before) {
		t.Fatalf("failed delivery acknowledged an observation: %s, %v", afterFailure, err)
	}
	trackActivationFollowups(ctx, "cli_seen", "wendy mcp serve", "completed")
	Flush()
	if pending := activationFollowups(dir, "cli_seen", "completed", time.Now().UTC(), true); len(pending) != 0 {
		t.Fatalf("successful delivery did not acknowledge observations: %+v", pending)
	}
	trackActivationFollowups(ctx, "cli_seen", "wendy mcp serve", "completed")
	Flush()
	if got := requests.Load(); got != 4 {
		t.Fatalf("got %d requests, want two failed attempts and two delivered retries", got)
	}
	close(bodies)
	counts := map[string]int{}
	for body := range bodies {
		counts[body.Event]++
		wantProperties := map[string]string{"entrypoint": "mcp", "outcome": "completed", "stage": "install"}
		if body.CommandName != "wendy mcp serve" || body.CommandRoot != "mcp" || body.AttemptID != AttemptID(ctx) || !body.Success || !reflect.DeepEqual(body.Properties, wantProperties) {
			t.Fatalf("follow-up lost stage properties: %+v", body)
		}
	}
	if want := map[string]int{"next_launch_no_run_24h": 2, "next_launch_no_run_7d": 2}; !reflect.DeepEqual(counts, want) {
		t.Fatalf("follow-up counts = %v, want %v", counts, want)
	}
}

func TestActivationFollowupDeliveryDoesNotAcknowledgeStaleWindow(t *testing.T) {
	for _, nextEvent := range []string{"run_started", "install_finished"} {
		t.Run(nextEvent, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("WENDY_CONFIG_DIR", dir)
			start := time.Now().UTC().Add(-8 * 24 * time.Hour)
			activationFollowups(dir, "install_finished", "completed", start, true)
			started := make(chan struct{}, 2)
			release := make(chan struct{})
			enableTestDelivery(t, func(w http.ResponseWriter, _ *http.Request) {
				started <- struct{}{}
				<-release
				w.WriteHeader(http.StatusNoContent)
			})
			// Release even when a request fails to start, before transport cleanup waits.
			t.Cleanup(func() { close(release) })
			trackActivationFollowups(context.Background(), "cli_seen", "wendy discover", "completed")
			for range 2 {
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatal("follow-up request did not start")
				}
			}
			activationFollowups(dir, nextEvent, "completed", time.Now().UTC(), true)
			before, beforeErr := os.ReadFile(filepath.Join(dir, activationStateFile))
			release <- struct{}{}
			release <- struct{}{}
			Flush()
			after, afterErr := os.ReadFile(filepath.Join(dir, activationStateFile))
			if string(before) != string(after) || os.IsNotExist(beforeErr) != os.IsNotExist(afterErr) {
				t.Fatalf("stale delivery changed the next window: before=%s (%v), after=%s (%v)", before, beforeErr, after, afterErr)
			}
		})
	}
}

func TestActivationFollowupsRunClearsAndFailedInstallDoesNotStartWindow(t *testing.T) {
	dir, now := t.TempDir(), time.Now().UTC()
	activationFollowups(dir, "install_finished", "failed", now, true)
	if _, err := os.Stat(filepath.Join(dir, activationStateFile)); !os.IsNotExist(err) {
		t.Fatal("failed install created a waiting window")
	}
	activationFollowups(dir, "os_install_finished", "completed", now, true)
	activationFollowups(dir, "run_started", "started", now.Add(time.Hour), true)
	if got := activationFollowups(dir, "cli_seen", "completed", now.Add(8*24*time.Hour), true); len(got) != 0 {
		t.Fatalf("observed a run but emitted %v", got)
	}
}

func TestActivationFollowupsDisabledDoesNotTouchState(t *testing.T) {
	dir, now := t.TempDir(), time.Now().UTC()
	t.Setenv("WENDY_CONFIG_DIR", dir)
	Disable()
	trackActivationFollowups(context.Background(), "install_finished", "wendy install", "completed")
	activationFollowups(dir, "install_finished", "completed", now, false)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("disabled analytics created state: %v, %v", entries, err)
	}
	activationFollowups(dir, "install_finished", "completed", now, true)
	before, _ := os.ReadFile(filepath.Join(dir, activationStateFile))
	trackActivationFollowups(context.Background(), "run_started", "wendy run", "started")
	trackActivationFollowups(context.Background(), "cli_seen", "wendy discover", "completed")
	activationFollowups(dir, "run_started", "started", now, false)
	if got := activationFollowups(dir, "cli_seen", "completed", now.Add(8*24*time.Hour), false); len(got) != 0 {
		t.Fatalf("disabled analytics emitted %v", got)
	}
	after, _ := os.ReadFile(filepath.Join(dir, activationStateFile))
	if string(before) != string(after) {
		t.Fatal("disabled analytics changed existing state")
	}
}
