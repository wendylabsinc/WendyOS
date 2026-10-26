package meshsharing

import (
	"context"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestDNSAuditTransitionsAndRecovery(t *testing.T) {
	var events []string
	policy := &fakePolicy{events: &events}
	c, err := NewWithDeps(policySelf(t), &fakeNode{events: &events}, policy, &fakeProbe{})
	if err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.InfoLevel)
	c.SetLogger(zap.New(core))
	ctx := context.Background()
	first := Decision{Mode: "roaming", GatewayDevice: policyDonorDevice(t), DNSLink: 1}
	second := Decision{Mode: "roaming", GatewayDevice: mustPolicyDevice(t, policyTenant, policyDonorLate), DNSLink: 2}
	for range 2 {
		if err := c.reconcile(ctx, first); err != nil {
			t.Fatal(err)
		}
	}
	if logs.Len() != 1 {
		t.Fatalf("unchanged policy emitted %d logs", logs.Len())
	}
	policy.failDNS = true
	if err := c.reconcile(ctx, second); err == nil {
		t.Fatal("missing DNS failure")
	}
	policy.failDNS = false
	if err := c.reconcile(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcile(ctx, Decision{Mode: "offline"}); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcile(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	entries := logs.All()
	if len(entries) != 7 {
		t.Fatalf("got %d audit entries", len(entries))
	}
	for i, want := range []struct {
		action, outcome, iface, resolver string
		gateway                          meshid.Device
	}{
		{"apply", "applied", "wlmp1", policyDonorDevice(t).MeshAddress().String(), policyDonorDevice(t)},
		{"apply", "failed", "wlmp2", second.GatewayDevice.MeshAddress().String(), second.GatewayDevice},
		{"remove", "applied", "wlmp1", first.GatewayDevice.MeshAddress().String(), first.GatewayDevice},
		{"apply", "applied", "wlmp2", second.GatewayDevice.MeshAddress().String(), second.GatewayDevice},
		{"remove", "applied", "wlmp2", second.GatewayDevice.MeshAddress().String(), second.GatewayDevice},
		{"apply", "applied", "wlmp1", policyDonorDevice(t).MeshAddress().String(), policyDonorDevice(t)},
		{"remove", "applied", "wlmp1", first.GatewayDevice.MeshAddress().String(), first.GatewayDevice},
	} {
		fields := entries[i].ContextMap()
		if fields["action"] != want.action || fields["outcome"] != want.outcome || fields["interface"] != want.iface || fields["resolver"] != want.resolver || fields["gateway_principal"] != want.gateway.Principal() || fields["dns_scope"] != "~." || fields["organization"] != policyTenant {
			t.Fatalf("event %d: %+v", i, fields)
		}
	}
	if entries[1].ContextMap()["error"] == nil || entries[1].ContextMap()["previous_gateway_principal"] != first.GatewayDevice.Principal() {
		t.Fatal("missing failure or previous donor context")
	}
}

func TestDNSAuditFailedRemovalIsRetried(t *testing.T) {
	var events []string
	policy := &fakePolicy{events: &events}
	c, err := NewWithDeps(policySelf(t), &fakeNode{events: &events}, policy, &fakeProbe{})
	if err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.InfoLevel)
	c.SetLogger(zap.New(core))
	ctx := context.Background()
	if err := c.reconcile(ctx, Decision{Mode: "roaming", GatewayDevice: policyDonorDevice(t), DNSLink: 1}); err != nil {
		t.Fatal(err)
	}
	policy.failDNSWithdrawal = true
	if err := c.reconcile(ctx, Decision{}); err == nil {
		t.Fatal("missing removal failure")
	}
	policy.failDNSWithdrawal = false
	if err := c.reconcile(ctx, Decision{}); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcile(ctx, Decision{}); err != nil {
		t.Fatal(err)
	}
	entries := logs.All()
	if len(entries) != 3 || entries[1].ContextMap()["outcome"] != "failed" || entries[2].ContextMap()["outcome"] != "applied" || entries[2].ContextMap()["gateway_principal"] != policyDonorDevice(t).Principal() {
		t.Fatalf("removal/recovery audit: %+v", entries)
	}
}

func TestDNSAuditCloseReportsSuccessfulCleanupRetry(t *testing.T) {
	var events []string
	policy := &fakePolicy{events: &events}
	c, err := NewWithDeps(policySelf(t), &fakeNode{events: &events}, policy, &fakeProbe{})
	if err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.InfoLevel)
	c.SetLogger(zap.New(core))
	if err := c.reconcile(context.Background(), Decision{Mode: "roaming", GatewayDevice: policyDonorDevice(t), DNSLink: 1}); err != nil {
		t.Fatal(err)
	}
	policy.failDNSWithdrawal = true // explicit removal fails; policy.Close succeeds
	if err := c.Close(); err == nil {
		t.Fatal("transient removal failure was hidden")
	}
	entries := logs.All()
	if len(entries) != 3 || entries[1].ContextMap()["outcome"] != "failed" || entries[2].ContextMap()["outcome"] != "applied" || entries[2].ContextMap()["action"] != "remove" {
		t.Fatalf("cleanup retry audit: %+v", entries)
	}
}
