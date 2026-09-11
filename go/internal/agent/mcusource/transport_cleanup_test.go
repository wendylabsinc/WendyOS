package mcusource

import (
	"context"
	"errors"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
	"net"
	"testing"
	"time"
)

type cleanupTransport struct {
	manifest *sensorlinkpb.SensorManifest
	err      error
	closes   int
}

func (t *cleanupTransport) Close() error { t.closes++; return nil }
func (t *cleanupTransport) FetchManifest(context.Context) (*sensorlinkpb.SensorManifest, error) {
	return t.manifest, t.err
}
func (*cleanupTransport) Stream(context.Context, []uint32) (<-chan *sensorlinkpb.SensorFrame, func() error, error) {
	panic("unexpected stream")
}

func TestSupervisorClosesTransportBeforeStream(t *testing.T) {
	for _, tc := range []struct {
		name     string
		manifest *sensorlinkpb.SensorManifest
		err      error
	}{
		{"manifest failure", nil, errors.New("unavailable")},
		{"wrong identity", &sensorlinkpb.SensorManifest{DeviceAssetId: 2}, nil},
		{"no authorized sensors", &sensorlinkpb.SensorManifest{DeviceAssetId: 1}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := &cleanupTransport{manifest: tc.manifest, err: tc.err}
			s := NewSupervisor(zap.NewNop(), nil, func(SensorPairing, string) (SensorTransport, error) { return tr, nil }, nil, nil)
			_, _ = s.streamOnce(context.Background(), SensorPairing{SourceAssetID: 1}, "source")
			if tr.closes != 1 {
				t.Fatalf("transport closed %d times", tr.closes)
			}
		})
	}
}

type pipeDialer struct{ conn net.Conn }

func (d pipeDialer) Dial(context.Context, string) (net.Conn, error) { return d.conn, nil }
func TestConnectCancellationInterruptsManifestRead(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := Connect(ctx, pipeDialer{client}, "source", nil); done <- err }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancelled handshake")
		}
	case <-time.After(time.Second):
		t.Fatal("manifest read did not stop")
	}
}
