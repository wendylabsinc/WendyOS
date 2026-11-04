package services

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"go.uber.org/zap"
)

func TestLegacyLANDialHasBudgetAfterFullDiscoveryWindow(t *testing.T) {
	provisioning := &ProvisioningService{enrolled: true, cloudHost: "unused:443", orgID: 64, assetID: 674}
	peers := NewLegacyPeerDialer(zap.NewNop(), provisioning)
	peers.browse = func(ctx context.Context) ([]models.LANDevice, error) {
		// Model a scan concluding on its own timer after the complete scan window.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(legacyLANScanTimeout):
		}
		return []models.LANDevice{{OrgID: 64, AssetID: 675, IPAddress: "192.0.2.5", IsMTLS: true, Port: 50052}}, nil
	}
	peers.dialTCP = func(ctx context.Context, address string) (net.Conn, error) {
		if address != "192.0.2.5:5000" {
			t.Errorf("wrong registry address %s", address)
		}
		if err := ctx.Err(); err != nil {
			t.Fatalf("discovery consumed TCP budget: %v", err)
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) < 500*time.Millisecond {
			t.Fatal("no TCP setup budget")
		}
		a, b := net.Pipe()
		b.Close()
		return a, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, mode, err := peers.DialTarget(ctx, DeliveryTarget{LegacyOrgID: 64, LegacyAssetID: 675}, 5000)
	if err != nil || mode != "lan-direct" {
		t.Fatalf("LAN delivery=%q %v", mode, err)
	}
	conn.Close()
}
func TestLegacyAppLookupDoesNotCancelItsScanTimer(t *testing.T) {
	provisioning := &ProvisioningService{enrolled: true, cloudHost: "cloud:443", orgID: 64, assetID: 674}
	peers := NewLegacyPeerDialer(zap.NewNop(), provisioning)
	peers.browse = func(ctx context.Context) ([]models.LANDevice, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < 2*time.Second {
			t.Fatal("lookup shortened caller context to scan timeout")
		}
		return []models.LANDevice{{OrgID: 64, AssetID: 675, IPAddress: "192.0.2.5", IsMTLS: true, Port: 50052}}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	enrollment, _ := provisioning.CloudEnrollment()
	if addr, ok := peers.legacyAppLANAddr(ctx, enrollment, 675); !ok || addr != "192.0.2.5:50052" {
		t.Fatalf("lookup=%s %v", addr, ok)
	}
}
func TestLegacyCanceledLookupDoesNotCacheMiss(t *testing.T) {
	provisioning := &ProvisioningService{enrolled: true, cloudHost: "cloud:443", orgID: 64, assetID: 674}
	peers := NewLegacyPeerDialer(zap.NewNop(), provisioning)
	calls := 0
	peers.browse = func(ctx context.Context) ([]models.LANDevice, error) {
		calls++
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return []models.LANDevice{{OrgID: 64, AssetID: 675, IPAddress: "192.0.2.5", IsMTLS: true, Port: 50052}}, nil
	}
	e, _ := provisioning.CloudEnrollment()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := peers.legacyAppLANAddr(ctx, e, 675); ok {
		t.Fatal("canceled lookup resolved")
	}
	if _, ok := peers.legacyAppLANAddr(context.Background(), e, 675); !ok || calls != 2 {
		t.Fatal("canceled lookup poisoned cache")
	}
}

func TestLegacyPeerScanDoesNotSettleOnSelfOrForeignEnrollment(t *testing.T) {
	filter := legacyPeerFilter{org: 64, self: 674}
	for _, device := range []models.LANDevice{{OrgID: 64, AssetID: 674, IsMTLS: true}, {OrgID: 65, AssetID: 675, IsMTLS: true}, {OrgID: 64, AssetID: 0, IsMTLS: true}, {OrgID: 64, AssetID: 675}} {
		if !filter.Exclude(device) {
			t.Fatalf("unusable sighting can settle scan: %+v", device)
		}
	}
	if filter.Exclude(models.LANDevice{OrgID: 64, AssetID: 675, IsMTLS: true}) {
		t.Fatal("same-org peer excluded")
	}
}
