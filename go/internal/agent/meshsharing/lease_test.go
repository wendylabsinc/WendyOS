package meshsharing

import "testing"

func TestGatewayLeasesDoNotTruncateIdentities(t *testing.T) {
	first := mustPolicyDevice(t, policyTenant, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaa1234")
	second := mustPolicyDevice(t, policyTenant, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbb1234")
	pool := &leasePool{}
	a, err := pool.allocate(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := pool.allocate(second)
	if err != nil {
		t.Fatal(err)
	}
	if a.address4() == b.address4() || a.address6() == b.address6() {
		t.Fatal("different devices received overlapping leases")
	}
	if _, err := pool.allocate(first); err == nil {
		t.Fatal("duplicate live device session accepted")
	}
	pool.release(second, a)
	if _, err := pool.allocate(first); err == nil {
		t.Fatal("foreign release freed a lease")
	}
	pool.release(first, a)
	if _, err := pool.allocate(first); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayLeaseReuseRejectsStaleRelease(t *testing.T) {
	device := policySelf(t)
	pool := &leasePool{}
	old, err := pool.allocate(device)
	if err != nil {
		t.Fatal(err)
	}
	pool.release(device, old)
	pool.next = 65534 // wrap to the old slot
	current, err := pool.allocate(device)
	if err != nil || current.Slot != old.Slot || current.generation == old.generation {
		t.Fatalf("bad slot reuse: %+v %v", current, err)
	}
	pool.release(device, old)
	if _, err := pool.allocate(device); err == nil {
		t.Fatal("stale session released a new lease")
	}
	pool.release(device, current)
	if _, err := pool.allocate(device); err != nil {
		t.Fatal(err)
	}
}
