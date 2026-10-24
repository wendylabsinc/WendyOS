package devices

import (
	"context"
	"errors"
	"testing"

	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
)

func TestExpectedIdentityKeepsCompletePrincipalThroughDial(t *testing.T) {
	const prefix = "spiffe://wendy.sh/tenant/22f35004-d638-4b5c-869f-b815974d6eae/"
	device := ExpectedIdentityForPin(DevicePin{Principal: prefix + "device/42"})
	service := ExpectedIdentityForPin(DevicePin{Principal: prefix + "service/asset-42"})
	if device == nil || service == nil || device.TenantUUID != service.TenantUUID || device.EntityType != service.EntityType || device.EntityID != service.EntityID {
		t.Fatal("expected equivalent legacy projections")
	}
	if device.Principal != prefix+"device/42" || service.Principal != prefix+"service/asset-42" {
		t.Fatal("full principal was lost")
	}
	for _, expected := range []*platform.DeviceIdentity{device, service} {
		t.Run(expected.Principal, func(t *testing.T) {
			calls := 0
			connector := fakeConnector(func(_ context.Context, request platform.DeviceConnectRequest) (platform.DeviceConnection, error) {
				calls++
				if !request.TLS || request.Expected == nil || request.Expected.Principal != expected.Principal {
					t.Fatalf("principal constraint lost: %+v", request)
				}
				// This fake host has verified a device/42 peer. It rejects the
				// service/asset-42 expectation despite identical legacy fields.
				if request.Expected.Principal != device.Principal {
					return nil, &platform.DevicePeerRejectedError{Cause: errors.New("principal mismatch")}
				}
				return workingConnection(), nil
			})
			result := Dial(context.Background(), connector, Target{Addr: "pi:50051", Expected: expected}, []Credential{{Reference: "operator"}}, DialOptions{})
			if expected == device {
				if result.Err != nil || result.Connection == nil {
					t.Fatalf("matching principal failed: %+v", result)
				}
				result.Connection.Close()
			} else if !errors.Is(result.Err, ErrIdentityRefused) || result.Connection != nil {
				t.Fatalf("different principal accepted: %+v", result)
			}
			if calls != 1 {
				t.Fatalf("unexpected fallback after principal verdict: %d", calls)
			}
		})
	}
}

func TestExpectedIdentityRetainsLegacyFallback(t *testing.T) {
	for _, principal := range []string{"", "spiffe://wendy.sh/tenant/INVALID/device/42"} {
		id := ExpectedIdentityForPin(DevicePin{OrgID: 7, AssetID: "42", Principal: principal})
		if id == nil || id.OrgID != 7 || id.EntityType != "asset" || id.EntityID != "42" || id.Principal != "" {
			t.Fatalf("legacy fallback changed: %+v", id)
		}
		if id := ExpectedIdentityForPin(DevicePin{Principal: principal}); id != nil {
			t.Fatalf("assetless legacy pin gained an identity: %+v", id)
		}
	}
}
