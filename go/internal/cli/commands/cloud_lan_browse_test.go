package commands

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

func TestCloudLANBrowseRetainsResultsUntilRosterIsReady(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	auth, asset := desktopConnectionTestSetup(t)
	previous := cloudLANStream
	t.Cleanup(func() { cloudLANStream = previous })
	calls := 0
	cloudLANStream = func(ctx context.Context, opts discovery.StreamOptions) <-chan discovery.LANEvent {
		calls++
		if !opts.UseCache || opts.Prober != nil {
			t.Error("browse should collect routing hints without authenticating devices")
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > cloudLANBrowseTimeout {
			t.Error("browse has no bounded deadline")
		}
		out := make(chan discovery.LANEvent, 3)
		for _, d := range []models.LANDevice{
			{ID: "other-device", IsMTLS: true, IPAddress: "192.168.100.51", Port: 50052},
			{ID: "42", IsMTLS: false, IPAddress: "192.168.100.52", Port: 50052},
			{ID: "42", IsMTLS: true, IPAddress: "192.168.100.50", Port: 50052},
		} {
			out <- discovery.LANEvent{Kind: discovery.LANFound, Device: d}
		}
		close(out)
		return out
	}
	ctx, cancel := startCloudLANBrowse(context.Background())
	defer cancel()
	// Model a roster response arriving after the browse has already finished.
	<-ctx.Value(cloudLANBrowseKey{}).(*cloudLANBrowse).done
	if ctx.Err() != nil {
		t.Fatal("browse completion canceled the connection context")
	}
	got := localCloudCandidates(ctx, asset, cloudDiscoveryIdentity(auth, asset))
	if !reflect.DeepEqual(got, []string{"192.168.100.50:50052", "192.168.100.50:50053"}) || calls != 1 {
		t.Fatalf("candidates = %v, browses = %d; want selected device and no second browse", got, calls)
	}
}

func TestCloudLANBrowseOverlapsRosterLookup(t *testing.T) {
	const tenant = "8a53be77-2a69-464f-8f73-83643fe0beaa"
	const assetID = "7791d76e-a942-4a8e-b582-063d063b5623"
	auth := config.AuthConfig{CloudGRPC: "cloud.example:443", Certificates: []config.CertificateInfo{{
		PrincipalURI: "spiffe://wendy.sh/tenant/" + tenant + "/operator/test",
	}}}
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{auth}})
	oldStream, oldFetch, oldConnect := cloudLANStream, fetchDefaultCloudAssetsV2Fn, connectDefaultCloudAssetV2Fn
	t.Cleanup(func() {
		cloudLANStream, fetchDefaultCloudAssetsV2Fn, connectDefaultCloudAssetV2Fn = oldStream, oldFetch, oldConnect
	})
	rosterStarted := make(chan struct{})
	cloudLANStream = func(ctx context.Context, _ discovery.StreamOptions) <-chan discovery.LANEvent {
		out := make(chan discovery.LANEvent)
		go func() {
			defer close(out)
			select {
			case <-rosterStarted:
			case <-ctx.Done():
			}
		}()
		return out
	}
	fetchDefaultCloudAssetsV2Fn = func(ctx context.Context, _ *config.AuthConfig, _ bool) ([]*cloudpbv2.Asset, error) {
		close(rosterStarted)
		browse, ok := ctx.Value(cloudLANBrowseKey{}).(*cloudLANBrowse)
		if !ok {
			t.Fatal("roster lookup started without a parallel LAN browse")
		}
		select {
		case <-browse.done:
		case <-time.After(time.Second):
			t.Fatal("LAN browse could not finish while roster lookup was running")
		}
		return []*cloudpbv2.Asset{{Id: assetID}}, nil
	}
	connectDefaultCloudAssetV2Fn = func(ctx context.Context, _ *config.AuthConfig, _ *cloudpbv2.Asset, _ string) (*grpcclient.AgentConnection, error) {
		if ctx.Err() != nil || ctx.Value(cloudLANBrowseKey{}) == nil {
			t.Error("connection lost the browse results or inherited its cancellation")
		}
		return &grpcclient.AgentConnection{}, nil
	}
	selected, err := connectCloudDeviceSelector(context.Background(), cloudDeviceSelector{Endpoint: auth.CloudGRPC, TenantUUID: tenant, AssetUUID: assetID})
	if err != nil {
		t.Fatal(err)
	}
	selected.Close()
}

func TestCloudLANBrowseUsesMatchBeforeScanFinishes(t *testing.T) {
	browse := &cloudLANBrowse{
		done: make(chan struct{}), updated: make(chan struct{}, 1),
		devices: []models.LANDevice{{ID: "selected"}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	found := false
	browse.visit(ctx, func(d models.LANDevice) bool { found = d.ID == "selected"; return found })
	if !found || ctx.Err() != nil {
		t.Fatal("matching sighting waited for the entire browse")
	}
}

func TestCloudLANBrowseSkippedForTunnelOnlyConnections(t *testing.T) {
	previous := cloudLANStream
	t.Cleanup(func() { cloudLANStream = previous })
	cloudLANStream = func(context.Context, discovery.StreamOptions) <-chan discovery.LANEvent {
		t.Error("tunnel-only connection browsed the LAN")
		out := make(chan discovery.LANEvent)
		close(out)
		return out
	}
	ctx, cancel := startCloudLANBrowse(context.WithValue(context.Background(), cloudTunnelOnlyKey{}, true))
	defer cancel()
	if ctx.Value(cloudLANBrowseKey{}) != nil {
		t.Fatal("tunnel-only connection carries LAN browse results")
	}
}

func TestCloudLANBrowseCancellation(t *testing.T) {
	previous := cloudLANStream
	t.Cleanup(func() { cloudLANStream = previous })
	cloudLANStream = func(ctx context.Context, _ discovery.StreamOptions) <-chan discovery.LANEvent {
		out := make(chan discovery.LANEvent)
		go func() { <-ctx.Done(); close(out) }()
		return out
	}
	ctx, cancel := startCloudLANBrowse(context.Background())
	cancel() // Includes a failed Cloud roster lookup or identity mismatch.
	select {
	case <-ctx.Value(cloudLANBrowseKey{}).(*cloudLANBrowse).done:
	case <-time.After(time.Second):
		t.Fatal("canceled browse did not stop")
	}
	if ctx.Err() != nil {
		t.Fatal("canceling the browse canceled the parent connection")
	}
}
