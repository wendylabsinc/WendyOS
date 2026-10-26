package services

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
)

func TestResetWaitsForEnrollmentResourcePublication(t *testing.T) {
	svc, dir := newTestProvisioningService(t)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var resourceEnrolled atomic.Bool
	svc.OnProvisioned = func(_, _ string, key []byte) {
		defer clear(key)
		// Production callbacks reenter read APIs while publishing resources.
		svc.ProvisioningInfo()
		close(entered)
		<-release
		resourceEnrolled.Store(true)
	}
	svc.BeforeUnprovisioned = func() error {
		svc.ProvisioningInfo()
		resourceEnrolled.Store(false)
		return nil
	}
	enrolled := make(chan error, 1)
	go func() {
		_, err := svc.StartProvisioning(context.Background(), &agentpb.StartProvisioningRequest{
			OrganizationId: 1, AssetId: 10, CloudHost: "cloud.wendy.io",
		})
		enrolled <- err
	}()
	select {
	case <-entered:
	case err := <-enrolled:
		t.Fatalf("enrollment did not reach callback: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("enrollment callback did not start")
	}
	reset := make(chan error, 1)
	go func() {
		_, err := svc.Unprovision(context.Background(), &agentpb.UnprovisionRequest{})
		reset <- err
	}()
	select {
	case err := <-reset:
		unblock()
		<-enrolled
		t.Fatalf("reset completed while an old enrollment could still publish resources: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	if err := <-enrolled; err != nil {
		t.Fatal(err)
	}
	if err := <-reset; err != nil {
		t.Fatal(err)
	}
	if resourceEnrolled.Load() {
		t.Fatal("late enrollment callback undid reset resource cleanup")
	}
}

func TestResetSuppressesUnpublishedEnrollmentCallback(t *testing.T) {
	svc := NewProvisioningService(zap.NewNop(), t.TempDir())
	svc.OnProvisioned = func(string, string, []byte) {
		t.Fatal("erased enrollment published its resources after reset")
	}
	svc.mu.Lock()
	complete, err := svc.persistProvisioning(&provisioningState{
		Enrolled: true, CloudHost: "cloud.wendy.io", OrgID: 1, AssetID: 10,
		CertPEM: "test-certificate", ChainPEM: "test-chain",
	}, []byte("test-key"))
	svc.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Unprovision(context.Background(), &agentpb.UnprovisionRequest{}); err != nil {
		t.Fatal(err)
	}
	// Exercise the scheduling gap after persistence releases mu and before
	// the callback acquires the lifecycle lock.
	complete()
}
