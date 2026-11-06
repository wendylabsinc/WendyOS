package discovery

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"testing"
	"time"
)

func TestSupplementalDiscoveryOutlivesSettledLANAndCancels(t *testing.T) {
	previous := lanBackendFn
	t.Cleanup(func() { lanBackendFn = previous })
	lanBackendFn = func(ctx context.Context, _ string, _ func(MDNSService)) error { <-ctx.Done(); return ctx.Err() }
	stopped := make(chan struct{})
	opts := StreamOptions{Supplemental: func(ctx context.Context) <-chan LANEvent {
		out := make(chan LANEvent)
		go func() {
			defer close(out)
			defer close(stopped)
			select {
			case out <- LANEvent{Kind: LANFound, Device: models.LANDevice{ID: "mesh", Mesh: true}}:
			case <-ctx.Done():
				return
			}
			<-ctx.Done()
		}()
		return out
	}}
	ctx, cancel := context.WithCancel(context.Background())
	events := StreamLAN(ctx, opts)
	select {
	case event := <-events:
		if !event.Device.Mesh {
			t.Fatal("mesh result absent")
		}
	case <-time.After(time.Second):
		t.Fatal("mesh stream blocked by LAN")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("mesh stream leaked")
	}
	for range events {
	}
}

func TestSupplementalMeshSurvivesLANUpdatesAndRetractions(t *testing.T) {
	rows := supplementalRows{primary: map[string]LANEvent{}, extra: map[string]LANEvent{}}
	lan := LANEvent{Kind: LANFound, Device: models.LANDevice{ID: "robot", Hostname: "robot.local"}}
	mesh := LANEvent{Kind: LANFound, Device: models.LANDevice{ID: "robot", Hostname: "mesh:robot", Mesh: true}}
	rows.accept(lan, false)
	if event, ok := rows.accept(mesh, true); !ok || !event.Device.Mesh {
		t.Fatal("mesh did not replace direct selector")
	}
	if _, ok := rows.accept(lan, false); ok {
		t.Fatal("LAN overwrote live mesh")
	}
	retract := lan
	retract.Kind = LANRetracted
	if _, ok := rows.accept(retract, false); ok {
		t.Fatal("LAN erased live mesh")
	}
	rows.accept(lan, false)
	retract = mesh
	retract.Kind = LANRetracted
	if event, ok := rows.accept(retract, true); !ok || event.Device.Mesh || event.Kind != LANUpdated {
		t.Fatal("direct path not restored after mesh disappears")
	}
}

func TestSupplementalMeshPreservesLANAliasRetirement(t *testing.T) {
	rows := supplementalRows{primary: map[string]LANEvent{}, extra: map[string]LANEvent{}}
	old := LANEvent{Kind: LANFound, Device: models.LANDevice{ID: "boot-id", DisplayName: "Ci Mesh 1", Hostname: "ci-mesh-1.local"}}
	mesh := LANEvent{Kind: LANFound, Device: models.LANDevice{ID: "enrolled-id", DisplayName: "Ci Mesh 1", Hostname: "mesh:enrolled-id", Mesh: true}, Probed: true}
	rows.accept(old, false)
	rows.accept(mesh, true)
	enrolled := LANEvent{Kind: LANUpdated, Device: models.LANDevice{ID: "enrolled-id", DisplayName: "Ci Mesh 1", Hostname: "ci-mesh-1.local"}, Supersedes: "boot-id"}
	event, ok := rows.accept(enrolled, false)
	if !ok || !event.Device.Mesh || !event.Probed || event.Supersedes != "boot-id" {
		t.Fatalf("alias retirement lost behind mesh row: %+v, %v", event, ok)
	}
	if _, ok := rows.primary["boot-id"]; ok {
		t.Fatal("stale LAN identity retained")
	}
	mesh.Kind = LANRetracted
	event, ok = rows.accept(mesh, true)
	if !ok || event.Device.Mesh || event.Device.ID != "enrolled-id" {
		t.Fatalf("incorrect LAN fallback: %+v, %v", event, ok)
	}
}
