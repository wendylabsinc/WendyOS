package main

import "testing"

func TestProvisioningIdentityTrackerRequiresRestartOnLiveRotation(t *testing.T) {
	var tracker provisioningIdentityTracker
	if tracker.Changed("cert", "chain", "secret key", 64, 445) {
		t.Fatal("first enrollment requested restart")
	}
	if tracker.Changed("cert", "chain", "secret key", 64, 445) {
		t.Fatal("identical provisioning requested restart")
	}
	for _, tc := range []struct {
		cert, chain, key string
		org, asset       int32
	}{
		{"new cert", "chain", "secret key", 64, 445},
		{"cert", "new chain", "secret key", 64, 445},
		{"cert", "chain", "new secret key", 64, 445},
		{"cert", "chain", "secret key", 65, 445},
		{"cert", "chain", "secret key", 64, 446},
	} {
		if !tracker.Changed(tc.cert, tc.chain, tc.key, tc.org, tc.asset) {
			t.Fatalf("changed identity did not request restart: %+v", tc)
		}
	}
	if tracker.Changed("cert", "chain", "secret key", 64, 445) {
		t.Fatal("changed identity overwrote active digest")
	}
}
