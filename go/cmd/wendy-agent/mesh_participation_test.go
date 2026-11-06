package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshsharing"
)

func TestParticipationDisabledNeverStarts(t *testing.T) {
	for _, value := range []string{"", `{"participate":false}`, `{"participate":false,"roam":true}`, `broken`} {
		t.Run(value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "nan-mesh.json")
			if value != "" {
				if err := os.WriteFile(path, []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			enabled, err := runWhileMeshParticipating(context.Background(), path, time.Millisecond, func(context.Context) error { t.Fatal("disabled/invalid policy started carriers"); return nil })
			if enabled {
				t.Fatal("reported enabled")
			}
			if (value == `broken` || value == `{"participate":false,"roam":true}`) != (err != nil) {
				t.Fatalf("unexpected validation: %v", err)
			}
		})
	}
}

func TestParticipationDisableDrainsAndReenablePreservesCarriers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nan-mesh.json")
	carrierPath := filepath.Join(dir, "local-mesh.json")
	carriers := `{"nan":true,"ble":true,"ethernet":true,"infrastructureWifi":true,"listen":"127.0.0.1:43023"}`
	if err := os.WriteFile(carrierPath, []byte(carriers), 0600); err != nil {
		t.Fatal(err)
	}
	save := func(config meshsharing.Config) {
		t.Helper()
		if err := meshsharing.SaveConfig(path, config); err != nil {
			t.Fatal(err)
		}
	}
	for cycle := 0; cycle < 2; cycle++ {
		save(meshsharing.Config{Participate: true})
		started, cancelSeen, allowDrain := make(chan struct{}), make(chan struct{}), make(chan struct{})
		type result struct {
			enabled bool
			err     error
		}
		done := make(chan result, 1)
		go func() {
			enabled, err := runWhileMeshParticipating(context.Background(), path, time.Millisecond, func(ctx context.Context) error {
				close(started)
				<-ctx.Done()
				close(cancelSeen)
				<-allowDrain // Real runner closes links/routes/catalog before returning.
				return ctx.Err()
			})
			done <- result{enabled, err}
		}()
		<-started
		save(meshsharing.Config{Participate: true, ShareUplink: true})
		select {
		case <-cancelSeen:
			t.Fatal("sharing-only update restarted carriers")
		case <-time.After(20 * time.Millisecond):
		}
		save(meshsharing.Config{})
		select {
		case <-cancelSeen:
		case <-time.After(time.Second):
			t.Fatal("disable did not cancel carriers")
		}
		select {
		case <-done:
			t.Fatal("disabled reported before owned runtime drained")
		default:
		}
		close(allowDrain)
		select {
		case r := <-done:
			if r.enabled || r.err != nil {
				t.Fatalf("disable result %+v", r)
			}
		case <-time.After(time.Second):
			t.Fatal("disable did not finish")
		}
		data, err := os.ReadFile(carrierPath)
		if err != nil || string(data) != carriers {
			t.Fatalf("carrier choices changed: %s %v", data, err)
		}
	}
}

func TestParticipationRemovalCorruptionAndParentCancellation(t *testing.T) {
	for _, mode := range []string{"removed", "corrupt", "parent", "runtime-error"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "nan-mesh.json")
			if err := meshsharing.SaveConfig(path, meshsharing.Config{Participate: true}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cause := errors.New("carrier cleanup failed")
			enabled, err := runWhileMeshParticipating(ctx, path, time.Millisecond, func(active context.Context) error {
				switch mode {
				case "removed":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				case "corrupt":
					if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
						t.Fatal(err)
					}
				case "parent":
					cancel()
				case "runtime-error":
					return cause
				}
				select {
				case <-active.Done():
					return active.Err()
				case <-time.After(time.Second):
					t.Fatal("runtime not canceled")
					return nil
				}
			})
			if mode == "removed" && (enabled || err != nil) {
				t.Fatalf("removed: %t %v", enabled, err)
			}
			if mode == "corrupt" && (enabled || err == nil) {
				t.Fatalf("corrupt: %t %v", enabled, err)
			}
			if mode == "parent" && (!enabled || !errors.Is(err, context.Canceled)) {
				t.Fatalf("parent: %t %v", enabled, err)
			}
			if mode == "runtime-error" && (!enabled || !errors.Is(err, cause)) {
				t.Fatalf("runtime: %t %v", enabled, err)
			}
		})
	}
}
