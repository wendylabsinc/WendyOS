package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	wendy "github.com/wendylabsinc/wendy-go"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestLadderRaceKeepsPrimaryCertificateRejection(t *testing.T) {
	for _, backupOutcome := range []string{"completed", "canceled", "deadline", "identity refusal during cancellation"} {
		for _, entryPoint := range []string{"ladder", "connect address"} {
			t.Run(backupOutcome+"/"+entryPoint, func(t *testing.T) {
				storedCerts := []config.CertificateInfo{{OrganizationID: 7}}
				setTempConfig(t, &config.Config{Auth: []config.AuthConfig{{Certificates: storedCerts}}})
				setPinCache(t)
				budget := 3 * time.Second
				if backupOutcome == "deadline" {
					budget = 100 * time.Millisecond
				}
				ctx, cancel := context.WithTimeout(context.Background(), budget)
				defer cancel()
				primaryErr := errors.New("remote error: tls: bad certificate")
				stubSDKConnector(t, func(workerCtx context.Context, request wendy.DeviceConnectRequest) (wendy.DeviceConnection, error) {
					if !request.TLS {
						t.Error("plaintext after certificate failure")
						return nil, errors.New("unexpected plaintext")
					}
					if strings.HasPrefix(request.Address, "192.0.2.1:") {
						return failedSDKPeer(primaryErr, 7), nil
					}
					if backupOutcome == "completed" {
						return nil, errors.New("connection refused")
					}
					peer := failedSDKPeer(context.Canceled, 0)
					peer.probe = func(probeCtx context.Context) (wendy.RPCResponse, error) {
						if backupOutcome != "deadline" {
							cancel()
						}
						<-probeCtx.Done()
						if backupOutcome == "identity refusal during cancellation" {
							peer.evidence.IdentityMismatch = &certs.IdentityMismatchError{WantOrg: 7, WantAsset: "42", GotOrg: 7, GotAsset: "43"}
						}
						return wendy.RPCResponse{}, probeCtx.Err()
					}
					return peer, nil
				})
				originalPlaintext := plaintextConnectFn
				t.Cleanup(func() { plaintextConnectFn = originalPlaintext })
				plaintextConnectFn = func(context.Context, string) (*grpcclient.AgentConnection, error) {
					t.Error("certificate rejection or cancellation must not offer plaintext")
					return grpcclient.NewFromConn(nil), nil
				}
				target := dialTarget{Addr: "192.0.2.1:50051", Candidates: []string{"192.0.2.1:50051", "[fd00::1]:50051"}}
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
	var calls int
	stubSDKConnector(t, func(_ context.Context, request wendy.DeviceConnectRequest) (wendy.DeviceConnection, error) {
		if !request.TLS {
			t.Fatal("plaintext with governing pin")
		}
		calls++
		peer := failedSDKPeer(errors.New("connection refused"), 0)
		peer.evidence.TCPDialError = errors.New("connection refused")
		return peer, nil
	})
	all := make([]config.CertificateInfo, 7)
	_, _, err := dialAgentLadderWithCerts(context.Background(), dialTarget{Addr: "127.0.0.1:50051", PinnedKey: "pi"}, all)
	if !errors.Is(err, errNoAuthenticatedEndpoint) || calls != 2 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}
