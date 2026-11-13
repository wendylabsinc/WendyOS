package backend

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
)

func TestNearbyScanDoesNotBlockDeviceOperations(t *testing.T) {
	b := New()
	started, release := make(chan struct{}), make(chan struct{})
	b.discoverNearby = func(ctx context.Context) ([]commands.DesktopDevice, error) {
		close(started)
		select {
		case <-release:
			return []commands.DesktopDevice{{Name: "ser9"}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	defer b.resetNearbyDiscovery(true)
	scan := make(chan error, 1)
	go func() { _, err := b.Nearby(); scan <- err }()
	<-started
	// The same lock guards camera requests and both connection commit stages.
	operation := make(chan struct{})
	go func() { _, done := b.begin(); done(); close(operation) }()
	select {
	case <-operation:
	case <-time.After(time.Second):
		t.Fatal("device operation blocked behind nearby scan")
	}
	close(release)
	if err := <-scan; err != nil {
		t.Fatal(err)
	}
}

func TestNearbyScopeChangeDiscardsLateResults(t *testing.T) {
	b := New()
	started, release := make(chan struct{}), make(chan struct{})
	b.discoverNearby = func(context.Context) ([]commands.DesktopDevice, error) {
		close(started)
		<-release
		return []commands.DesktopDevice{{Name: "old-account"}}, nil
	}
	result := make(chan error, 1)
	go func() {
		devices, err := b.Nearby()
		if len(devices) > 0 {
			result <- errors.New("returned stale devices")
			return
		}
		result <- err
	}()
	<-started
	b.resetNearbyDiscovery(true)
	b.allowNearbyDiscovery()
	close(release)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("late result: %v", err)
	}
}

func TestNearbyBackgroundCancelsAndBlocksScans(t *testing.T) {
	b := New()
	started := make(chan struct{})
	b.discoverNearby = func(ctx context.Context) ([]commands.DesktopDevice, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	result := make(chan error, 1)
	go func() { _, err := b.Nearby(); result <- err }()
	<-started
	if err := b.SetMobileState(false, "", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("scan not canceled")
	}
	if _, err := b.Nearby(); !errors.Is(err, context.Canceled) {
		t.Fatalf("background scan: %v", err)
	}
	b.allowNearbyDiscovery()
	b.discoverNearby = func(context.Context) ([]commands.DesktopDevice, error) {
		return []commands.DesktopDevice{{Name: "new-scope"}}, nil
	}
	devices, err := b.Nearby()
	if err != nil || len(devices) != 1 || devices[0].Name != "new-scope" {
		t.Fatalf("foreground scan: %v %v", devices, err)
	}
}
