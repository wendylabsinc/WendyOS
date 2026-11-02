package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/cli/netmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
)

func TestMeshDiscoveryRecoversAfterWorkerRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	joins, reads := 0, 0
	id := uuid.New()
	events := meshDiscoveryStreamWith(ctx, time.Millisecond, func(context.Context) (string, error) {
		joins++
		if joins == 2 {
			return "", errors.New("worker restarting")
		}
		return "session", nil
	}, func(context.Context, string) (netmesh.Status, error) {
		reads++
		if reads == 2 {
			return netmesh.Status{}, errors.New("socket closed")
		}
		return netmesh.Status{Devices: []netmesh.Device{{ID: id, Name: "robot", Port: 50052}}}, nil
	})
	for _, kind := range []discovery.LANEventKind{discovery.LANFound, discovery.LANRetracted, discovery.LANFound} {
		select {
		case event, ok := <-events:
			if !ok || event.Kind != kind || event.Device.ID != id.String() {
				t.Fatalf("wanted %v for %s, got %+v, open=%v", kind, id, event, ok)
			}
		case <-ctx.Done():
			t.Fatal("discovery did not recover")
		}
	}
	cancel()
	for range events {
	}
	if joins != 3 {
		t.Fatalf("expected reconnect with retry, joined %d times", joins)
	}
}

func TestMeshDiscoveryStopsWhenOrganizationRemoved(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	joins := 0
	events := meshDiscoveryStreamWith(ctx, time.Millisecond, func(context.Context) (string, error) {
		joins++
		if joins == 1 {
			return "session", nil
		}
		return "", nil
	}, func(context.Context, string) (netmesh.Status, error) {
		return netmesh.Status{}, errors.New("organization changed")
	})
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("unexpected device event")
		}
	case <-ctx.Done():
		t.Fatal("stream did not stop after organization removal")
	}
	if joins != 2 {
		t.Fatalf("expected organization recheck, got %d", joins)
	}
}

func TestMeshDiscoveryDebouncesRouteGapsButRetractsLostRoutes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		start := time.Now()
		id := uuid.New()
		events := meshDiscoveryStreamWith(ctx, time.Second, func(context.Context) (string, error) {
			return "session", nil
		}, func(context.Context, string) (netmesh.Status, error) {
			elapsed := time.Since(start)
			// A short gap recovers without restarting the row's metadata RPC.
			if elapsed < time.Second || elapsed >= 5*time.Second && elapsed < 6*time.Second {
				return netmesh.Status{Devices: []netmesh.Device{{ID: id, Name: "robot", Port: 50052}}}, nil
			}
			return netmesh.Status{}, nil
		})
		if event := meshMetadataEvent(t, ctx, events); event.Kind != discovery.LANFound {
			t.Fatal(event)
		}
		event := meshMetadataEvent(t, ctx, events)
		if event.Kind != discovery.LANRetracted || time.Since(start) != 6*time.Second+meshPresenceGrace {
			t.Fatalf("route gap restarted row or lost route stayed forever: event=%+v elapsed=%v", event, time.Since(start))
		}
		cancel()
		for range events {
		}
	})
}

func TestMeshWorkerRetiresAfterLocalRebuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wendy")
	if err := os.WriteFile(path, []byte("old"), 0700); err != nil {
		t.Fatal(err)
	}
	initial, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !meshWorkerBinaryCurrent(path, initial) {
		t.Fatal("unchanged executable retired worker")
	}
	if err := os.WriteFile(path+".new", []byte("new"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
	if meshWorkerBinaryCurrent(path, initial) {
		t.Fatal("worker survived replacement by a rebuilt CLI")
	}
}
