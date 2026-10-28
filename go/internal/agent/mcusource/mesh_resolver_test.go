package mcusource

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"go.uber.org/zap"
)

const meshSourcePrincipal = "spiffe://wendy.sh/tenant/11111111-2222-4333-8444-555555555555/device/44444444-4444-4444-8444-444444444444"

func TestMeshSourceResolverTracksCurrentEndpoints(t *testing.T) {
	p := SensorPairing{SourcePrincipalURI: meshSourcePrincipal, Transport: "wendycom"}
	source := models.LANDevice{PrincipalURI: p.SourcePrincipalURI, WendyLite: true, Sensorlink: true, IsMTLS: true, IPAddress: "fd45:b891:567f::5", Port: 5054}
	resolve := newSourceResolver(func() []models.LANDevice { return []models.LANDevice{source} }, func(context.Context, SensorPairing) ([]string, bool) { return nil, false })
	got, ok := resolve(context.Background(), p)
	if !ok || !slices.Equal(got, []string{"[fd45:b891:567f::5]:5054"}) {
		t.Fatalf("initial: %v %v", got, ok)
	}
	source.Port = 5055
	got, ok = resolve(context.Background(), p)
	if !ok || !slices.Equal(got, []string{"[fd45:b891:567f::5]:5055"}) {
		t.Fatalf("port changed: %v %v", got, ok)
	}
}

func TestMeshSourceResolverUsesLANWhileMeshUnavailable(t *testing.T) {
	p := SensorPairing{SourcePrincipalURI: meshSourcePrincipal, Transport: "wendycom"}
	var sources []models.LANDevice
	resolve := newSourceResolver(func() []models.LANDevice { return sources }, func(context.Context, SensorPairing) ([]string, bool) {
		return []string{"192.0.2.20:5054"}, true
	})
	got, ok := resolve(context.Background(), p)
	if !ok || !slices.Equal(got, []string{"192.0.2.20:5054"}) {
		t.Fatalf("LAN fallback: %v %v", got, ok)
	}
	sources = []models.LANDevice{{PrincipalURI: p.SourcePrincipalURI, WendyLite: true, Sensorlink: true, IsMTLS: true, IPAddress: "fd45:b891:567f::5", Port: 5055}}
	got, ok = resolve(context.Background(), p)
	if !ok || !slices.Equal(got, []string{"192.0.2.20:5054", "[fd45:b891:567f::5]:5055"}) {
		t.Fatalf("mesh route returned: %v %v", got, ok)
	}
}

func TestDiscoveredPairingReResolvesAfterInitialAddressHint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var dialed []string
		sup := NewSupervisor(zap.NewNop(), nil, func(_ SensorPairing, addr string) (SensorTransport, error) {
			dialed = append(dialed, addr)
			if len(dialed) == 2 {
				cancel()
			}
			return nil, errors.New("source restarting")
		}, nil, nil)
		var resolves int
		sup.SetSourceResolver(func(_ context.Context, p SensorPairing) ([]string, bool) {
			if p.SourcePrincipalURI != meshSourcePrincipal {
				t.Fatalf("wrong identity: %+v", p)
			}
			resolves++
			return []string{"[fd45:b891:567f::5]:5055"}, true
		})
		p := SensorPairing{SourcePrincipalURI: meshSourcePrincipal, Transport: "wendycom"}
		runner := NewRunner(zap.NewNop(), sup)
		runner.StartDiscovered(p, "[fd45:b891:567f::5]:5054")
		synctest.Wait()
		<-ctx.Done()
		// No sensor nodes were allocated; stop waits for the reconcile loop.
		runner.StopPairing(p)
		if resolves != 1 || !slices.Equal(dialed, []string{"[fd45:b891:567f::5]:5054", "[fd45:b891:567f::5]:5055"}) {
			t.Fatalf("resolves=%d dialed=%v", resolves, dialed)
		}
	})
}

func TestSourceResolverBoundsLANBrowseAndRefreshesMeshFallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := SensorPairing{SourcePrincipalURI: meshSourcePrincipal, Transport: "wendycom"}
		source := models.LANDevice{PrincipalURI: p.SourcePrincipalURI, WendyLite: true, Sensorlink: true, IsMTLS: true, IPAddress: "fd45:b891:567f::5", Port: 5054}
		var mesh = []models.LANDevice{source}
		resolve := newSourceResolver(func() []models.LANDevice { return mesh }, func(ctx context.Context, _ SensorPairing) ([]string, bool) {
			<-ctx.Done()
			mesh[0].Port = 5055 // The live signed endpoint changes during browse.
			return nil, false
		})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		started := time.Now()
		got, ok := resolve(ctx, p)
		if !ok || !slices.Equal(got, []string{"[fd45:b891:567f::5]:5055"}) || time.Since(started) != time.Second || ctx.Err() != nil {
			t.Fatalf("bounded LAN lookup consumed mesh attempt: %v %v after %v", got, ok, time.Since(started))
		}
		resolve = newSourceResolver(func() []models.LANDevice { return mesh }, func(context.Context, SensorPairing) ([]string, bool) {
			mesh = nil // A route withdrawn during browsing is no longer a fallback.
			return []string{"192.0.2.20:5054"}, true
		})
		if got, ok := resolve(ctx, p); !ok || !slices.Equal(got, []string{"192.0.2.20:5054"}) {
			t.Fatalf("withdrawn mesh endpoint retained: %v %v", got, ok)
		}
	})
}

func TestSourceResolverCancellationDrainsLANLookup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := SensorPairing{SourcePrincipalURI: meshSourcePrincipal, Transport: "wendycom"}
		source := models.LANDevice{PrincipalURI: p.SourcePrincipalURI, WendyLite: true, Sensorlink: true, IsMTLS: true, IPAddress: "fd45:b891:567f::5", Port: 5054}
		ctx, cancel := context.WithCancel(context.Background())
		finished := false
		resolve := newSourceResolver(func() []models.LANDevice { return []models.LANDevice{source} }, func(ctx context.Context, _ SensorPairing) ([]string, bool) {
			cancel()
			<-ctx.Done()
			finished = true
			return nil, false
		})
		if got, ok := resolve(ctx, p); ok || len(got) != 0 || !finished {
			t.Fatalf("cancelled discovery retained work: %v %v", got, ok)
		}
	})
}
