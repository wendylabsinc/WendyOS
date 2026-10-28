package mcusource

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	sensorpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
)

func nativeVideoClient() *fakeWendycomClient {
	c := newFakeWendycomClient()
	c.peerCert = certWithURI(meshSourcePrincipal)
	c.manifest = &sensorpb.SensorManifest{Sensors: []*sensorpb.SensorDescriptor{{ChannelId: 1, Name: "camera",
		Format: &sensorpb.SensorDescriptor_Video{Video: &sensorpb.VideoFormat{Codec: sensorpb.VideoFormat_MJPEG}}}}}
	c.onSubscribe = func() { c.send(wholeFrame(1, 1)) }
	return c
}

// Adapted from PR2458's sequential fallback/re-resolution fixtures. Exercise
// the actual cached WendyCom consumer, fresh manifest budget and subscription
// lifecycle, keeping each constrained board's handshakes strictly sequential.
func TestSensorSupervisorSequentialLANMeshSubscriptionAndRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		p := SensorPairing{SourcePrincipalURI: meshSourcePrincipal, Transport: "wendycom"}
		source := models.LANDevice{PrincipalURI: p.SourcePrincipalURI, WendyLite: true, Sensorlink: true, IsMTLS: true,
			IPAddress: "fd45:b891:567f::5", Port: 5055}
		lan := "192.0.2.20:5054"
		var attempts []string
		var transports []*wendycomTransport
		meshClient, recoveredLAN := nativeVideoClient(), nativeVideoClient()
		active, resolutions := 0, 0
		sup := NewSupervisor(zap.NewNop(), statusLoopback{}, func(pair SensorPairing, address string) (SensorTransport, error) {
			if pair.SourcePrincipalURI != p.SourcePrincipalURI || active != 0 {
				t.Fatalf("overlapping or unpinned native attempt: %+v active=%d", pair, active)
			}
			if len(transports) != 0 && !transports[len(transports)-1].closed {
				t.Fatal("previous candidate was not closed before fallback")
			}
			attempts = append(attempts, address)
			tr := &wendycomTransport{logger: zap.NewNop(), sourcePrincipalURI: p.SourcePrincipalURI, connect: func(setup context.Context) (wendycomClient, error) {
				active++
				defer func() { active-- }()
				deadline, ok := setup.Deadline()
				if !ok || time.Until(deadline) != 75*time.Second {
					t.Fatalf("source candidate lost native setup budget: %v %v", deadline, ok)
				}
				switch address {
				case "192.0.2.20:5054":
					<-setup.Done() // An ordinary stale LAN endpoint consumes its finite setup ceiling.
					return nil, setup.Err()
				case "[fd45:b891:567f::5]:5055":
					select {
					case <-time.After(33 * time.Second):
						return meshClient, nil // Measured C6 certificate flight, beyond the old 15s limit.
					case <-setup.Done():
						return nil, setup.Err()
					}
				case "192.0.2.21:5056":
					return recoveredLAN, nil
				default:
					t.Fatalf("stale candidate after recovery: %q", address)
					return nil, errors.New("unexpected endpoint")
				}
			}}
			transports = append(transports, tr)
			return tr, nil
		}, func(string) ros2camera.CameraWriter { return statusWriter{} }, nil)
		sup.SetSourceResolver(newSourceResolver(func() []models.LANDevice { return []models.LANDevice{source} },
			func(context.Context, SensorPairing) ([]string, bool) { resolutions++; return []string{lan}, true }))
		done := make(chan error, 1)
		go func() { done <- sup.RunPairing(ctx, p, "") }()
		time.Sleep(110 * time.Second)
		synctest.Wait()
		if !sup.IsPairingConnected(p) || !slices.Equal(attempts, []string{"192.0.2.20:5054", "[fd45:b891:567f::5]:5055"}) {
			t.Fatalf("LAN timeout did not reach a healthy mesh subscription: %v connected=%v", attempts, sup.IsPairingConnected(p))
		}
		time.Sleep(80 * time.Second)
		if !sup.IsPairingConnected(p) || resolutions != 1 {
			t.Fatal("completed setup deadline interrupted an established subscription")
		}
		// The mesh route remains advertised, but its native connection drops.
		// A new attempt must resolve again and use the now healthy LAN endpoint.
		lan, source.Port = "192.0.2.21:5056", 5057
		meshClient.dropOff()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if resolutions != 2 || !sup.IsPairingConnected(p) || !slices.Equal(attempts, []string{"192.0.2.20:5054", "[fd45:b891:567f::5]:5055", "192.0.2.21:5056"}) {
			t.Fatalf("reconnect did not recover through LAN: %v resolutions=%d", attempts, resolutions)
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if sup.IsPairingConnected(p) || meshClient.closeCount() != 1 || recoveredLAN.closeCount() != 1 || active != 0 ||
			len(recoveredLAN.unsubscribes()) != 1 || recoveredLAN.listenerCount() != 0 {
			t.Fatal("cancellation retained a native subscription or connection")
		}
	})
}

func TestSensorSupervisorCancellationStopsSequentialFallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		p := SensorPairing{SourcePrincipalURI: meshSourcePrincipal, Transport: "wendycom"}
		var attempts []string
		var tr *wendycomTransport
		sup := NewSupervisor(zap.NewNop(), nil, func(_ SensorPairing, address string) (SensorTransport, error) {
			attempts = append(attempts, address)
			tr = &wendycomTransport{logger: zap.NewNop(), sourcePrincipalURI: p.SourcePrincipalURI, connect: func(ctx context.Context) (wendycomClient, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}}
			return tr, nil
		}, nil, nil)
		sup.SetSourceResolver(func(context.Context, SensorPairing) ([]string, bool) { return []string{"lan", "mesh"}, true })
		if err := sup.RunPairing(ctx, p, ""); !errors.Is(err, context.DeadlineExceeded) || !slices.Equal(attempts, []string{"lan"}) || !tr.closed {
			t.Fatalf("cancelled candidate leaked or started fallback: attempts=%v err=%v", attempts, err)
		}
	})
}
