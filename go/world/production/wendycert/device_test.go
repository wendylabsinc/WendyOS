package wendycert_test

import (
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

func TestDeviceVerifierDoesNotWidenOperatorVerification(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	principal := "spiffe://wendy.sh/tenant/" + testidentity.Tenant + "/device/22222222-2222-4222-8222-222222222222"
	f := testidentity.NewWith(t, now, principal)
	device, err := wendycert.NewDevice(wendycert.Config{Tenant: f.Tenant, Roots: [][]byte{f.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	id, err := device.Verify(f.ChainDER, now.Add(-time.Second), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if id.Principal != principal || id.Tenant != f.Tenant || id.SigningKeySHA256 == "" || id.CertificateSHA256 == "" {
		t.Fatal("device identity missing exact enrollment binding")
	}
	operator, err := wendycert.New(wendycert.Config{Tenant: f.Tenant, Roots: [][]byte{f.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = operator.Verify(f.ChainDER, now, now); err == nil {
		t.Fatal("operator verifier admitted a device")
	}
	if _, err = device.Verify(f.ChainDER, f.NotBefore.Add(-time.Nanosecond), now); err == nil {
		t.Fatal("device NotBefore allowance")
	}
	if _, err = device.Verify(f.ChainDER, now, f.NotAfter.Add(time.Nanosecond)); err == nil {
		t.Fatal("device expiry allowance")
	}
	if err = (wendycert.DeviceIdentity{}).VerifySignature([]byte("a"), []byte("b")); err == nil {
		t.Fatal("manufactured identity verified")
	}
}

func TestDeviceVerifierRejectsOtherActorTypesAndNoncanonicalIDs(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	for _, suffix := range []string{"operator/test-only", "service/asset-12", "device/not-a-uuid", "device/22222222222242228222222222222222", "device/22222222-2222-4222-8222-222222222222/path"} {
		t.Run(suffix, func(t *testing.T) {
			f := testidentity.NewWith(t, now, "spiffe://wendy.sh/tenant/"+testidentity.Tenant+"/"+suffix)
			v, err := wendycert.NewDevice(wendycert.Config{Tenant: f.Tenant, Roots: [][]byte{f.RootDER}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = v.Verify(f.ChainDER, now, now); err == nil {
				t.Fatal("accepted a noncanonical/nondevice actor")
			}
		})
	}
}
