package providers

import (
	"context"
	"crypto/x509"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/litetopology"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var liteTopologySetupBudget = litetopology.SetupBudget
var liteTopologyReadBudget = litetopology.AggregateBudget

type liteTopologyClient interface {
	NetTopology(context.Context) (*pb.LocalMeshTopology, error)
	PeerCertificate() *x509.Certificate
	GetDeviceIdentity(time.Duration) (*liteclient.DeviceIdentity, error)
	Close() error
}

var connectLiteTopologyClient = func(ctx context.Context, p *MicroWendyProvider, device models.ExternalDevice) (liteTopologyClient, error) {
	return p.connectClient(ctx, device)
}

func (p *MicroWendyProvider) NetTopology(parent context.Context, device models.ExternalDevice) (*pb.LocalMeshTopology, error) {
	ctx, finishSetup, cancel := litetopology.BeginSetup(parent, liteTopologySetupBudget)
	defer cancel()
	client, err := connectLiteTopologyClient(ctx, p, device)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	if err := finishSetup(); err != nil {
		return nil, err
	}
	readCtx, stopRead := context.WithTimeout(ctx, liteTopologyReadBudget)
	defer stopRead()
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stop()
	if client.PeerCertificate() == nil && device.ConnectionInfo["type"] != "USB" {
		return nil, status.Error(codes.Unauthenticated, "topology requires an authenticated device or physical USB")
	}
	snapshot, err := client.NetTopology(readCtx)
	if err != nil {
		return nil, err
	}
	if peer := client.PeerCertificate(); peer != nil {
		identity, ok, err := certs.IdentityFromCert(peer)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "topology reporter identity was not verified")
		}
		if err := litetopology.ValidateReporter(snapshot, identity); err != nil {
			return nil, err
		}
	} else {
		expected := device.ConnectionInfo["enrolledDeviceId"]
		if expected == "" {
			identity, err := client.GetDeviceIdentity(3 * time.Second)
			if err != nil {
				return nil, err
			}
			expected = identity.EnrolledDeviceID
		}
		if expected == "" || snapshot.GetSelfDeviceId() != expected {
			return nil, status.Error(codes.PermissionDenied, "topology reporter does not match the selected physical USB device")
		}
		identity, err := certs.ParsePrincipal(snapshot.GetSelfPrincipal())
		if err != nil {
			return nil, err
		}
		if err := litetopology.ValidateReporter(snapshot, identity); err != nil {
			return nil, err
		}
	}
	return snapshot, nil
}
