package mcusource

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"go.uber.org/zap"
)

const meshSourcePrincipal = "spiffe://wendy.sh/tenant/11111111-2222-4333-8444-555555555555/device/44444444-4444-4444-8444-444444444444"

func TestMeshSourceResolverTracksCurrentEndpoints(t *testing.T) {
	p := SensorPairing{SourcePrincipalURI: meshSourcePrincipal, Transport: "wendycom"}
	source := models.LANDevice{PrincipalURI: p.SourcePrincipalURI, WendyLite: true, Sensorlink: true, IsMTLS: true, IPAddress: "fd45:b891:567f::5", Port: 5054}
	resolve := NewSourceResolver(func() []models.LANDevice { return []models.LANDevice{source} })
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
	lan := discovery.MDNSService{IPAddress: "192.0.2.20", Port: 5054, TXTRecords: map[string]string{
		"tenant": "11111111-2222-4333-8444-555555555555", "principal": meshSourcePrincipal, "mtls": "true", "caps": "sensors",
	}}
	stubContinuousBrowse(t, lan)
	var sources []models.LANDevice
	resolve := NewSourceResolver(func() []models.LANDevice { return sources })
	got, ok := resolve(context.Background(), p)
	if !ok || !slices.Equal(got, []string{"192.0.2.20:5054"}) {
		t.Fatalf("LAN fallback: %v %v", got, ok)
	}
	sources = []models.LANDevice{{PrincipalURI: p.SourcePrincipalURI, WendyLite: true, Sensorlink: true, IsMTLS: true, IPAddress: "fd45:b891:567f::5", Port: 5055}}
	got, ok = resolve(context.Background(), p)
	if !ok || !slices.Equal(got, []string{"[fd45:b891:567f::5]:5055"}) {
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
