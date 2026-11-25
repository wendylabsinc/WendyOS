//go:build linux

package containerd

import (
	"context"
	"os"
	"reflect"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// The unit round-trip models containerd's empty-label deletion. This test
// checks the real metadata store through a fresh connection, as restoration
// after an agent restart would see it.
func TestIntegrationEntitlementsSurviveMetadataReload(t *testing.T) {
	c, ctx := newIntegrationClient(t)
	want := []appconfig.Entitlement{
		{Type: appconfig.EntitlementCamera},
		{Type: appconfig.EntitlementEpisodeWrite},
		{Type: appconfig.EntitlementNotifications},
		{Type: appconfig.EntitlementBluetooth},
		{Type: appconfig.EntitlementGPU},
		{Type: appconfig.EntitlementNPU},
		{Type: appconfig.EntitlementSerial, Device: "ttyACM0"},
	}
	labels := wendyLabels("sh.wendy.restore", "", "1", nil, want, "", nil)
	labels["sh.wendy/test-empty"] = ""
	ctr, err := c.client.NewContainer(ctx, "restore-entitlements", containerd.WithNewSpec(), containerd.WithContainerLabels(labels))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctr.Delete(context.WithoutCancel(ctx)) })
	reopened, err := containerd.New(os.Getenv(containerdIntegrationEnv))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reloaded, err := reopened.LoadContainer(ctx, ctr.ID())
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := reloaded.Labels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := persisted["sh.wendy/test-empty"]; exists {
		t.Fatal("fixture did not exercise containerd's empty-label deletion")
	}
	got := parseEntitlementsFromAnnotations(persisted)
	if len(got) != len(want) {
		t.Fatalf("restored %d entitlements, want %d: %+v", len(got), len(want), got)
	}
	for _, entitlement := range want {
		restored, ok := entitlementOfType(got, entitlement.Type)
		if !ok || !reflect.DeepEqual(restored, entitlement) {
			t.Errorf("restored %+v, want %+v", restored, entitlement)
		}
	}
}
