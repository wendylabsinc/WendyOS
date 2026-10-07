package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// No network or credential writes: exercise the actual recovery sequence and
// assert its side effects, latest error and user-visible account of the result.
func isolateConnectionRecovery(t *testing.T) {
	t.Helper()
	oldBroadcast, oldWait, oldAttempted := broadcastTimeFn, waitForTimeProofFn, clockSkewSyncAttempted
	oldInteractive, oldJSON, oldPrompt, oldRefresh := isInteractiveTerminalFn, jsonOutput, confirmFn, refreshAllCertsFn
	t.Cleanup(func() {
		broadcastTimeFn, waitForTimeProofFn, clockSkewSyncAttempted = oldBroadcast, oldWait, oldAttempted
		isInteractiveTerminalFn, jsonOutput, confirmFn, refreshAllCertsFn = oldInteractive, oldJSON, oldPrompt, oldRefresh
	})
	clockSkewSyncAttempted = false
	jsonOutput = false
	isInteractiveTerminalFn = func() bool { return true }
	waitForTimeProofFn = func(ctx context.Context) error { return ctx.Err() }
	broadcastTimeFn = func(context.Context) error { t.Fatal("unexpected time broadcast"); return nil }
	confirmFn = func(string) bool { t.Fatal("unexpected refresh prompt"); return false }
	refreshAllCertsFn = func(context.Context) error { t.Fatal("unexpected certificate refresh"); return nil }
}

func TestConnectionClockRecoveryOutcomes(t *testing.T) {
	original := newTLSHandshakeRejectedError(errors.New("remote error: tls: bad certificate"))
	refusal := &certs.IdentityMismatchError{WantOrg: 2, WantAsset: "344", GotOrg: 76, GotAsset: "472"}
	broadcastFailure := errors.New("no active multicast-capable network interfaces")
	for _, tc := range []struct {
		name                     string
		broadcastErr, retryErr   error
		wantBroadcast, wantRetry int
		wantText                 string
	}{
		{"send fails", broadcastFailure, nil, 1, 0, "Could not broadcast a signed time proof"},
		{"still rejected", nil, original, 1, 1, "clock change remains unconfirmed"},
		{"retry reveals changed identity", nil, refusal, 1, 1, "clock change remains unconfirmed"},
		{"connected", nil, nil, 1, 1, "Connected after broadcasting"},
	} {
		for _, json := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/text", true: "/json"}[json], func(t *testing.T) {
				isolateConnectionRecovery(t)
				jsonOutput = json
				broadcasts, retries := 0, 0
				broadcastTimeFn = func(context.Context) error { broadcasts++; return tc.broadcastErr }
				wantConn := &grpcclient.AgentConnection{Host: "expected-device"}
				var gotErr error
				var gotConn *grpcclient.AgentConnection
				stdout, stderr := captureBoth(t, func() {
					gotConn, gotErr = recoverAgentConnection(context.Background(), false, original, func() (*grpcclient.AgentConnection, error) {
						retries++
						if tc.retryErr != nil {
							return nil, tc.retryErr
						}
						return wantConn, nil
					})
				})
				if stdout != "" {
					t.Fatalf("recovery contaminated stdout: %q", stdout)
				}
				if broadcasts != tc.wantBroadcast || retries != tc.wantRetry {
					t.Fatalf("broadcasts=%d retries=%d", broadcasts, retries)
				}
				if tc.broadcastErr != nil || tc.retryErr != nil {
					wantErr := tc.retryErr
					if tc.broadcastErr != nil {
						wantErr = original
					}
					if gotConn != nil || !errors.Is(gotErr, wantErr) {
						t.Fatalf("connection=%v error=%v, want cause %v", gotConn, gotErr, wantErr)
					}
					stderr += gotErr.Error()
				} else if gotErr != nil || gotConn != wantConn {
					t.Fatalf("connection=%v error=%v", gotConn, gotErr)
				}
				if !strings.Contains(stderr, tc.wantText) {
					t.Fatalf("missing outcome %q: %s", tc.wantText, stderr)
				}
				for _, forbidden := range []string{"auth refresh-certs", "syncing device time", "ssh "} {
					if strings.Contains(stderr, forbidden) {
						t.Fatalf("misleading recovery %q: %s", forbidden, stderr)
					}
				}
				// A second failure in this invocation does not broadcast again.
				_, _ = recoverAgentConnection(context.Background(), true, original, func() (*grpcclient.AgentConnection, error) { t.Fatal("second clock retry"); return nil, nil })
				if broadcasts != 1 {
					t.Fatalf("broadcasts=%d after second failure", broadcasts)
				}
			})
		}
	}
}

func TestConnectionRecoveryOnlyRefreshesProvenClientProblems(t *testing.T) {
	now := time.Now()
	expired := config.CertificateInfo{OrganizationID: 2, PemCertificate: certPEM(t, now.Add(-time.Hour))}
	valid := config.CertificateInfo{OrganizationID: 2, PemCertificate: certPEM(t, now.Add(time.Hour))}
	otherOrg := valid
	otherOrg.OrganizationID = 76
	bad := errors.New("remote error: tls: bad certificate")
	for _, tc := range []struct {
		name        string
		cause       error
		wantRefresh bool
	}{
		{"timeout", newProvisionedAgentUnauthorizedError(errors.New("i/o timeout")), false},
		{"inferred TLS rejection from timeout", chooseRejectionError(context.Background(), 2, []config.CertificateInfo{expired}, errors.New("i/o timeout")), false},
		{"generic TLS rejection", newTLSHandshakeRejectedError(bad), false},
		{"peer claims expiry", newTLSHandshakeRejectedError(errors.New("remote error: tls: expired certificate")), false},
		{"clientAuth EKU", newTLSHandshakeRejectedError(errors.New("certificate is not valid for client authentication")), true},
		{"expired local certificate", chooseRejectionError(context.Background(), 2, []config.CertificateInfo{expired}, bad), true},
		{"expired relevant certificate alongside unrelated valid", chooseRejectionError(context.Background(), 2, []config.CertificateInfo{expired, otherOrg}, bad), true},
		{"valid alternative", chooseRejectionError(context.Background(), 2, []config.CertificateInfo{expired, valid}, bad), false},
		{"unknown certificate", chooseRejectionError(context.Background(), 2, []config.CertificateInfo{{OrganizationID: 2, PemCertificate: "invalid"}}, bad), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateConnectionRecovery(t)
			// A previous clock attempt failed; ensure this cannot turn ambiguous
			// rejection into permission to refresh.
			clockSkewSyncAttempted = true
			prompts, refreshes, retries := 0, 0, 0
			confirmFn = func(string) bool { prompts++; return true }
			refreshAllCertsFn = func(context.Context) error { refreshes++; return nil }
			wantConn := &grpcclient.AgentConnection{Host: "expected-device"}
			conn, err := recoverAgentConnection(context.Background(), false, tc.cause, func() (*grpcclient.AgentConnection, error) { retries++; return wantConn, nil })
			if tc.wantRefresh {
				if conn != wantConn || err != nil || prompts != 1 || refreshes != 1 || retries != 1 {
					t.Fatalf("conn=%v err=%v prompt/refresh/retry=%d/%d/%d", conn, err, prompts, refreshes, retries)
				}
				if !strings.Contains(tc.cause.Error(), "auth refresh-certs") {
					t.Fatalf("missing noninteractive guidance: %v", tc.cause)
				}
			} else if conn != nil || !errors.Is(err, tc.cause) || prompts+refreshes+retries != 0 {
				t.Fatalf("speculative recovery: conn=%v err=%v prompt/refresh/retry=%d/%d/%d", conn, err, prompts, refreshes, retries)
			}
		})
	}
}

func TestConnectionRecoveryGuardsAndCancellation(t *testing.T) {
	for _, cause := range []error{errDeviceIdentityRefused, errors.Join(orgMismatchDeviceError{deviceOrg: 76}), context.Canceled} {
		t.Run(cause.Error(), func(t *testing.T) {
			isolateConnectionRecovery(t)
			_, err := recoverAgentConnection(context.Background(), false, cause, func() (*grpcclient.AgentConnection, error) { t.Fatal("retry after terminal refusal"); return nil, nil })
			if !errors.Is(err, cause) {
				t.Fatalf("cause lost: %v", err)
			}
		})
	}
	t.Run("cancel during delivery wait", func(t *testing.T) {
		wait := waitForTimeProofFn
		isolateConnectionRecovery(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		broadcastTimeFn = func(context.Context) error { return nil }
		waitForTimeProofFn = func(ctx context.Context) error { cancel(); return wait(ctx) }
		_, err := recoverAgentConnection(ctx, false, newTLSHandshakeRejectedError(errors.New("remote error: tls: bad certificate")), func() (*grpcclient.AgentConnection, error) { t.Fatal("retry after cancellation"); return nil, nil })
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("lost cancellation: %v", err)
		}
	})
	for _, mode := range []string{"declined", "noninteractive", "json", "no terminal", "refresh failed", "retry refused"} {
		t.Run(mode, func(t *testing.T) {
			isolateConnectionRecovery(t)
			cause := newTLSHandshakeRejectedError(errors.New("certificate is not valid for client authentication"))
			jsonOutput = mode == "json"
			isInteractiveTerminalFn = func() bool { return mode != "no terminal" }
			confirmFn = func(string) bool {
				if mode == "noninteractive" || mode == "json" || mode == "no terminal" {
					t.Fatal("unexpected prompt")
				}
				return mode != "declined"
			}
			if mode == "refresh failed" {
				refreshAllCertsFn = func(context.Context) error { return errors.New("cloud unavailable") }
			}
			if mode == "retry refused" {
				refreshAllCertsFn = func(context.Context) error { return nil }
			}
			_, err := recoverAgentConnection(context.Background(), mode == "noninteractive", cause, func() (*grpcclient.AgentConnection, error) {
				if mode != "retry refused" {
					t.Fatal("unexpected retry")
				}
				return nil, errDeviceIdentityRefused
			})
			want := cause
			if mode == "retry refused" {
				want = errDeviceIdentityRefused
			}
			if !errors.Is(err, want) {
				t.Fatalf("lost latest failure: %v", err)
			}
		})
	}
}

// Exercise both production callers, not just the sequence helper: a new pin
// refusal after either recovery attempt must survive with the same pin key.
func TestConnectionEntryPointsKeepRecoveryFailure(t *testing.T) {
	for _, entry := range []string{"connectToAgent", "resolveTarget"} {
		for _, recovery := range []string{"clock", "certificates"} {
			t.Run(entry+"/"+recovery, func(t *testing.T) {
				isolateConnectionRecovery(t)
				setTempConfig(t, &config.Config{})
				t.Setenv("WENDY_AGENT_SOCKET", "")
				oldFlag, oldDial, oldDiscover := deviceFlag, dialAgentLadderFn, discoverLANDevices
				t.Cleanup(func() { deviceFlag, dialAgentLadderFn, discoverLANDevices = oldFlag, oldDial, oldDiscover })
				deviceFlag = "192.0.2.1:50051"
				discoverLANDevices = func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil }
				cause := newTLSHandshakeRejectedError(errors.New("remote error: tls: bad certificate"))
				actions := 0
				if recovery == "clock" {
					broadcastTimeFn = func(context.Context) error { actions++; return nil }
				} else {
					expired := config.CertificateInfo{OrganizationID: 2, PemCertificate: certPEM(t, time.Now().Add(-time.Hour))}
					cause = chooseRejectionError(context.Background(), 2, []config.CertificateInfo{expired}, errors.New("remote error: tls: bad certificate"))
					confirmFn = func(string) bool { return true }
					refreshAllCertsFn = func(context.Context) error { actions++; return nil }
				}
				calls := 0
				dialAgentLadderFn = func(_ context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
					calls++
					if target.PinKey != "192.0.2.1" || target.Addr != deviceFlag {
						t.Fatalf("retry changed target: %+v", target)
					}
					if calls == 1 {
						return nil, nil, cause
					}
					return nil, nil, errDeviceIdentityRefused
				}
				var err error
				stderr := captureStderr(t, func() {
					if entry == "connectToAgent" {
						_, err = connectToAgent(context.Background())
					} else {
						_, err = resolveTarget(context.Background())
					}
				})
				if calls != 2 || actions != 1 || !errors.Is(err, errDeviceIdentityRefused) || !strings.Contains(stderr, "retry failed") {
					t.Fatalf("calls=%d actions=%d err=%v", calls, actions, err)
				}
				if got := ErrorClass(err); got != "device_identity_mismatch" {
					t.Fatalf("lost failure class: %s", got)
				}
			})
		}
	}
}
