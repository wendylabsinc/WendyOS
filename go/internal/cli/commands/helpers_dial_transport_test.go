package commands

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestLadderRaceKeepsPrimaryCertificateRejection(t *testing.T) {
	for _, backupOutcome := range []string{"completed", "canceled", "deadline", "identity refusal during cancellation"} {
		for _, entryPoint := range []string{"ladder", "connect address"} {
			t.Run(backupOutcome+"/"+entryPoint, func(t *testing.T) {
				storedCerts := []config.CertificateInfo{{OrganizationID: 7}}
				cfg := &config.Config{Auth: []config.AuthConfig{{Certificates: storedCerts}}}
				cfg.SetDevicePin("pi.local", 7, "cloud.test:443", "42", "")
				setTempConfig(t, cfg)
				setPinCache(t)
				budget := 3 * time.Second
				if backupOutcome == "deadline" {
					budget = 100 * time.Millisecond
				}
				ctx, cancel := context.WithTimeout(context.Background(), budget)
				defer cancel()
				primaryErr := errors.New("remote error: tls: bad certificate")
				stubMTLSCandidate(t, func(workerCtx context.Context, worker *mtlsWalk, candidate string, isPrimary bool) (*grpcclient.AgentConnection, error) {
					if isPrimary {
						worker.recordMTLSErr(candidate, primaryErr, true)
						worker.primaryMTLSPortCertFails = 1
						worker.primaryObservedOrg = 7
						worker.anyCertRejection = true
						return nil, nil
					}
					if backupOutcome == "completed" {
						worker.recordMTLSErr(candidate, errors.New("connection refused"), false)
						return nil, nil
					}
					if backupOutcome != "deadline" {
						cancel()
					}
					<-workerCtx.Done()
					worker.recordMTLSErr(candidate, workerCtx.Err(), false)
					if backupOutcome == "identity refusal during cancellation" {
						return nil, identityRefusal("pi.local", &certs.IdentityMismatchError{WantOrg: 7, WantAsset: "42", GotOrg: 7, GotAsset: "43"})
					}
					return nil, workerCtx.Err()
				})
				originalPlaintext := plaintextConnectFn
				t.Cleanup(func() { plaintextConnectFn = originalPlaintext })
				plaintextConnectFn = func(context.Context, string) (*grpcclient.AgentConnection, error) {
					t.Error("certificate rejection or cancellation must not offer plaintext")
					return grpcclient.NewFromConn(nil), nil
				}
				target := dialTarget{Expected: &certs.WendyIdentity{OrgID: 7, EntityType: certs.EntityAsset, EntityID: "42"}, Addr: "192.0.2.1:50051", Candidates: []string{"192.0.2.1:50051", "[fd00::1]:50051"}}
				var conn *grpcclient.AgentConnection
				var mtlsErr, err error
				if entryPoint == "ladder" {
					conn, mtlsErr, err = dialAgentLadderWithCerts(ctx, target, storedCerts)
				} else {
					conn, err = connectAgentAtAddressKeyed(ctx, target.Addr, "pi.local", func() bool { return false }, target.Candidates...)
				}
				if conn != nil {
					conn.Close()
					t.Fatal("failed candidates returned a connection")
				}
				if backupOutcome == "identity refusal during cancellation" {
					if !errors.Is(err, errDeviceIdentityRefused) {
						t.Fatalf("error = %v, want identity refusal ahead of certificate rejection", err)
					}
					return
				}
				var rejected tlsHandshakeRejectedError
				if !errors.As(err, &rejected) || !errors.Is(err, primaryErr) {
					t.Fatalf("error = %v, want primary certificate rejection with its cause", err)
				}
				if entryPoint == "ladder" && !errors.Is(mtlsErr, primaryErr) {
					t.Fatalf("mTLS error = %v, want primary certificate rejection", mtlsErr)
				}
			})
		}
	}
}

func TestDialAddrDoesNotRetryCertificatesAfterTCPFailure(t *testing.T) {
	setTempConfig(t, &config.Config{})
	setPinCache(t)
	addr := deadAgentAddr(t)
	walk := &mtlsWalk{target: dialTarget{Addr: addr, Expected: &certs.WendyIdentity{OrgID: 7, EntityType: certs.EntityAsset, EntityID: "42"}}}
	for i := 0; i < 7; i++ {
		walk.allCerts = append(walk.allCerts, selfSignedCLICert(t, i+1))
		walk.probeOrder = append(walk.probeOrder, i)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := walk.dialAddr(ctx, addr, true)
	if conn != nil {
		conn.Close()
		t.Fatal("closed ports returned a connection")
	}
	if err != nil {
		t.Fatalf("transport failure should allow other addresses: %v", err)
	}
	if len(walk.attempts) != 2 {
		t.Fatalf("got %d attempts for seven certificates, want one per port", len(walk.attempts))
	}
	if walk.primaryRejectedOurCert() || walk.anyCertRejection {
		t.Fatal("a TCP failure was reported as a certificate rejection")
	}
	if walk.primaryMTLSPortNonCertFails != 1 {
		t.Fatalf("primary mTLS port failures = %d, want 1", walk.primaryMTLSPortNonCertFails)
	}
}
