package localmesh

import (
	"bytes"
	"crypto/mldsa"
	"testing"
	"time"
)

func TestMLDSADeviceManifestAdmission(t *testing.T) {
	for _, algorithm := range []string{"MLDSA65", "MLDSA87"} {
		t.Run(algorithm, func(t *testing.T) {
			params := mldsa.MLDSA65()
			if algorithm == "MLDSA87" {
				params = mldsa.MLDSA87()
			}
			key, err := mldsa.GenerateKey(params)
			if err != nil {
				t.Fatal(err)
			}
			sender := principalBoundCredentials(t, testPrincipal(testTenantUUID, testDeviceAID), key)
			receiver := testCredentialsFor(t, testPrincipal(testTenantUUID, testDeviceBID))
			now := time.Now()
			cache, err := OpenIdentityCache("", DefaultCacheLimits(), receiver.Verify, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cache.Put(sender.Certificate.Certificate, now); err != nil {
				t.Fatal(err)
			}
			directory, err := NewDirectory(receiver.Device.Tenant(), 8, cache)
			if err != nil {
				t.Fatal(err)
			}
			manifest := routeManifest(sender.Device, now)
			manifest.Name = "UNO Q"
			proof, err := SignManifest(manifest, sender.Certificate.Certificate, key, now)
			if err != nil {
				t.Fatal(err)
			}
			bad := proof
			bad.Signature = bytes.Clone(proof.Signature)
			bad.Signature[0] ^= 1
			if _, err := directory.Accept(bad, now); err == nil {
				t.Fatal("accepted altered ML-DSA signature")
			}
			bad = proof
			bad.Body = bytes.Replace(proof.Body, []byte("UNO Q"), []byte("forged"), 1)
			if _, err := directory.Accept(bad, now); err == nil {
				t.Fatal("accepted altered ML-DSA body")
			}
			bad = proof
			bad.Signature = make([]byte, 8193)
			if _, err := directory.Accept(bad, now); err == nil {
				t.Fatal("accepted oversized signature")
			}
			if changed, err := directory.Accept(proof, now); err != nil || !changed {
				t.Fatalf("accept: changed=%v err=%v", changed, err)
			}
			if changed, err := directory.Accept(proof, now); err != nil || changed {
				t.Fatalf("duplicate: changed=%v err=%v", changed, err)
			}
			if _, err := directory.Accept(proof, now.Add(3*time.Minute)); err == nil {
				t.Fatal("accepted expired ML-DSA manifest")
			}
		})
	}
}
