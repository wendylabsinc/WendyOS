package meshsharing

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func TestPolicyOperatorInternetLeaseRetainsAuthenticatedInstallation(t *testing.T) {
	for _, wrongSource := range []bool{false, true} {
		t.Run(fmt.Sprint(wrongSource), func(t *testing.T) {
			factory, root := controlTestCredentialsWithRoot(t)
			client := factory(controlTestTenant, "operator/alice")
			original := factory(controlTestTenant, controlTestGateway)
			key, err := x509.MarshalECPrivateKey(original.Signer.(*ecdsa.PrivateKey))
			if err != nil {
				t.Fatal(err)
			}
			certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: original.Certificate.Certificate[0]}))
			clock := &controlPolicyClock{floor: time.Now(), uncertain: true}
			gateway, err := localmesh.NewCredentialsWithTimePolicy(certPEM, root,
				string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: key})), clock.policy())
			if err != nil {
				t.Fatal(err)
			}
			source := client.Device
			if wrongSource {
				source, _ = source.WithInstance(uuid.New())
			}
			backend := newControlBackend()
			stop, raw, done := controlPipe(t, gateway, client, backend, source)
			defer stop()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			conn, config, err := Borrow(ctx, client, gateway.Device, raw)
			if wrongSource {
				if conn != nil {
					conn.Close()
				}
				if err == nil || backend.grants.Load() != 0 {
					t.Fatal("policy control accepted a mismatched operator installation", err)
				}
				return
			}
			if err != nil {
				t.Fatal("policy rejected authenticated operator installation", err)
			}
			defer conn.Close()
			if !config.Valid() || <-backend.granted != client.Device {
				t.Fatal("operator lease lost authenticated installation")
			}
			// The revalidated principal must keep its original installation,
			// while an expired credential still closes and revokes the lease.
			clock.set(time.Now().Add(2*time.Hour), 0, true)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("policy expiry did not retire operator lease")
			}
			if <-backend.revoked != client.Device {
				t.Fatal("policy expiry revoked another installation")
			}
		})
	}
}
