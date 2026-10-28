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
	oldBroadcast, oldWait, oldAttempted, oldCached := broadcastTimeFn, waitForTimeProofFn, clockSkewSyncAttempted, proofFromCacheFn
	oldInteractive, oldJSON, oldPrompt, oldRefresh := isInteractiveTerminalFn, jsonOutput, confirmFn, refreshCurrentCertsFn
	t.Cleanup(func() {
		broadcastTimeFn, waitForTimeProofFn, clockSkewSyncAttempted, proofFromCacheFn = oldBroadcast, oldWait, oldAttempted, oldCached
		isInteractiveTerminalFn, jsonOutput, confirmFn, refreshCurrentCertsFn = oldInteractive, oldJSON, oldPrompt, oldRefresh
	})
	clockSkewSyncAttempted = false
	jsonOutput = false
	isInteractiveTerminalFn = func() bool { return true }
	waitForTimeProofFn = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	proofFromCacheFn = func() (bool, time.Duration) { return false, 0 }
	broadcastTimeFn = func(context.Context) error { t.Fatal("unexpected time broadcast"); return nil }
	confirmFn = func(string) bool { t.Fatal("unexpected refresh prompt"); return false }
	refreshCurrentCertsFn = func(context.Context) error { t.Fatal("unexpected certificate refresh"); return nil }
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
		// Every retry is preceded by a send, and the outcome names the next step.
		{"still rejected", nil, original, 3, 3, "still rejected the connection after the time proof was sent 3 times, so its clock is probably still wrong.\n  Next: connect the device to the internet"},
		{"retry reveals changed identity", nil, refusal, 1, 1, "Connection retry failed after broadcasting the time proof."},
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
				if broadcasts != tc.wantBroadcast {
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
			refreshCurrentCertsFn = func(context.Context) error { refreshes++; return nil }
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
		waitForTimeProofFn = func(ctx context.Context, d time.Duration) error { cancel(); return wait(ctx, d) }
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
				refreshCurrentCertsFn = func(context.Context) error { return errors.New("cloud unavailable") }
			}
			if mode == "retry refused" {
				refreshCurrentCertsFn = func(context.Context) error { return nil }
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
					refreshCurrentCertsFn = func(context.Context) error { actions++; return nil }
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

// TestConnectionRecoveryNeverRefreshesAnotherContextsCert pins WDY-3864: when
// the rejected certificate for the device's org belongs to a context other than
// the current one, recovery must not renew the current context (that spends its
// refresh token and cannot fix the dial); it says which context to switch to.
func TestConnectionRecoveryNeverRefreshesAnotherContextsCert(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "file")
	expired := config.CertificateInfo{OrganizationID: 2, PemCertificate: certPEM(t, time.Now().Add(-time.Hour))}
	current := config.CertificateInfo{OrganizationID: 1, PemCertificate: certPEM(t, time.Now().Add(time.Hour))}
	if err := config.Save(&config.Config{
		CurrentContext: "acme",
		Auth: []config.AuthConfig{
			{Name: "acme", CloudGRPC: "acme:443", Certificates: []config.CertificateInfo{current}},
			{Name: "wendy", CloudGRPC: "wendy:443", Certificates: []config.CertificateInfo{expired}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	isolateConnectionRecovery(t) // fails on any refresh prompt or refresh
	cause := chooseRejectionError(context.Background(), 2, []config.CertificateInfo{current, expired}, errors.New("remote error: tls: bad certificate"))
	_, err := recoverAgentConnection(context.Background(), false, cause, func() (*grpcclient.AgentConnection, error) {
		t.Fatal("retry without a usable certificate")
		return nil, nil
	})
	if !errors.Is(err, cause) {
		t.Fatalf("cause lost: %v", err)
	}
	if !strings.Contains(err.Error(), "wendy auth use wendy") {
		t.Fatalf("missing switch-context guidance: %v", err)
	}

	// The same org held by the current context keeps the plain advice.
	if err := config.Save(&config.Config{
		CurrentContext: "wendy",
		Auth: []config.AuthConfig{
			{Name: "acme", CloudGRPC: "acme:443", Certificates: []config.CertificateInfo{current}},
			{Name: "wendy", CloudGRPC: "wendy:443", Certificates: []config.CertificateInfo{expired}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	cause = chooseRejectionError(context.Background(), 2, []config.CertificateInfo{current, expired}, errors.New("remote error: tls: bad certificate"))
	if strings.Contains(cause.Error(), "wendy auth use") || !strings.Contains(cause.Error(), "wendy auth refresh-certs") {
		t.Fatalf("current context holds the cert, want plain refresh advice: %v", cause)
	}
}

// TestConnectionRecoveryNamesTheContextToSwitchTo pins the WDY-3864 review:
// a legacy and an operator session holding the device's org name the operator
// one, and an unknown device org with several contexts names them all instead
// of renewing the current context.
func TestConnectionRecoveryNamesTheContextToSwitchTo(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "file")
	expired := func(org int) config.CertificateInfo {
		return config.CertificateInfo{OrganizationID: org, PemCertificate: certPEM(t, time.Now().Add(-time.Hour))}
	}
	if err := config.Save(&config.Config{
		CurrentContext: "acme",
		Auth: []config.AuthConfig{
			{Name: "acme", CloudGRPC: "acme:443", Certificates: []config.CertificateInfo{expired(1)}},
			{Name: "legacy", CloudGRPC: "wendy:443", Certificates: []config.CertificateInfo{expired(2)}},
			{Name: "operator", CloudGRPC: "wendy:443", OAuthIssuer: "https://auth.wendy.dev", Certificates: []config.CertificateInfo{expired(2)}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if got := switchContextStep(2); !strings.Contains(got, "wendy auth use operator") {
		t.Fatalf("want the operator session, got %q", got)
	}

	isolateConnectionRecovery(t) // fails on any refresh prompt or refresh
	all := []config.CertificateInfo{expired(1), expired(2), expired(2)}
	cause := chooseRejectionError(context.Background(), 0, all, errors.New("remote error: tls: bad certificate"))
	_, err := recoverAgentConnection(context.Background(), false, cause, func() (*grpcclient.AgentConnection, error) {
		t.Fatal("retry without a refresh")
		return nil, nil
	})
	if !errors.Is(err, cause) {
		t.Fatalf("cause lost: %v", err)
	}
	if !strings.Contains(err.Error(), "organization is unknown") || !strings.Contains(err.Error(), "acme (current), legacy, operator") {
		t.Fatalf("want the candidate contexts named: %v", err)
	}
}

// A multicast proof is unacknowledged, so a single datagram and a single retry
// 1.5s later (the 2026-10-28 mesh-1 report) gave up on a proof that was lost or
// arrived before the agent had joined the group on that link. Recovery now sends
// the proof before every retry and keeps retrying across the agent's 5s
// membership refresh, and only the device accepting the handshake counts as
// confirmation.
func TestClockRecoveryRetriesUntilTheDeviceAcceptsTheProof(t *testing.T) {
	isolateConnectionRecovery(t)
	original := newTLSHandshakeRejectedError(errors.New("remote error: tls: bad certificate"))
	broadcasts, retries := 0, 0
	var waits []time.Duration
	broadcastTimeFn = func(context.Context) error { broadcasts++; return nil }
	waitForTimeProofFn = func(ctx context.Context, d time.Duration) error {
		if broadcasts != len(waits)+1 {
			t.Fatalf("retry %d waited without sending the proof first (broadcasts=%d)", len(waits)+1, broadcasts)
		}
		waits = append(waits, d)
		return ctx.Err()
	}
	wantConn := &grpcclient.AgentConnection{Host: "expected-device"}
	var conn *grpcclient.AgentConnection
	var err error
	stderr := captureStderr(t, func() {
		conn, err = recoverAgentConnection(context.Background(), false, original, func() (*grpcclient.AgentConnection, error) {
			retries++
			if retries < 3 {
				return nil, original
			}
			return wantConn, nil
		})
	})
	if err != nil || conn != wantConn {
		t.Fatalf("conn=%v err=%v", conn, err)
	}
	if broadcasts != 3 || retries != 3 {
		t.Fatalf("broadcasts=%d retries=%d, want 3/3", broadcasts, retries)
	}
	var total time.Duration
	for i, d := range waits {
		if d != clockSkewRetryDelays[i] {
			t.Fatalf("wait %d = %s, want %s", i, d, clockSkewRetryDelays[i])
		}
		total += d
	}
	// agent/timesync.multicastRefreshInterval: the agent joins the group on a
	// newly usable interface at most this often.
	if agentGroupRefresh := 5 * time.Second; total <= agentGroupRefresh {
		t.Fatalf("retries span %s, not longer than the agent's %s group refresh", total, agentGroupRefresh)
	}
	if !strings.Contains(stderr, "Retrying the connection for about 10.5s") || !strings.Contains(stderr, "Connected after broadcasting the time proof.") {
		t.Fatalf("stderr does not account for the retries: %s", stderr)
	}
	if strings.Contains(stderr, "still rejected") {
		t.Fatalf("reported failure after connecting: %s", stderr)
	}
}

// A retry that fails for a reason other than a certificate rejection (the
// device went away, or now answers as another device) ends recovery at once
// with that newer error.
func TestClockRecoveryStopsWhenTheDeviceFailsDifferently(t *testing.T) {
	for name, next := range map[string]error{
		"unreachable":      errors.New("dial tcp 10.0.0.9:50052: connect: connection refused"),
		"identity refused": errDeviceIdentityRefused,
	} {
		t.Run(name, func(t *testing.T) {
			isolateConnectionRecovery(t)
			broadcasts, retries := 0, 0
			broadcastTimeFn = func(context.Context) error { broadcasts++; return nil }
			var err error
			stderr := captureStderr(t, func() {
				_, err = recoverAgentConnection(context.Background(), false, newTLSHandshakeRejectedError(errors.New("remote error: tls: bad certificate")), func() (*grpcclient.AgentConnection, error) {
					retries++
					return nil, next
				})
			})
			if !errors.Is(err, next) || broadcasts != 1 || retries != 1 {
				t.Fatalf("err=%v broadcasts=%d retries=%d", err, broadcasts, retries)
			}
			if !strings.Contains(stderr, "Connection retry failed after broadcasting the time proof.") || strings.Contains(stderr, "still rejected") {
				t.Fatalf("misreported outcome: %s", stderr)
			}
		})
	}
}

// When no time server answers, the CLI sends a proof saved by an earlier run.
// It can only move the device clock up to that saved time, so the user is told
// it was a saved proof and that this computer needs the internet.
func TestClockRecoveryNamesASavedProofAndItsRemedy(t *testing.T) {
	isolateConnectionRecovery(t)
	proofFromCacheFn = func() (bool, time.Duration) { return true, 3 * time.Hour }
	sends := 0
	broadcastTimeFn = func(context.Context) error {
		sends++
		if sends == 2 {
			return errors.New("broadcasting time proof: no active multicast-capable network interfaces")
		}
		return nil
	}
	original := newTLSHandshakeRejectedError(errors.New("remote error: tls: bad certificate"))
	var err error
	stderr := captureStderr(t, func() {
		_, err = recoverAgentConnection(context.Background(), false, original, func() (*grpcclient.AgentConnection, error) { return nil, original })
	})
	if !errors.Is(err, errTLSHandshakeRejected) {
		t.Fatalf("lost the rejection: %v", err)
	}
	for _, want := range []string{
		"proof saved 3h0m0s ago",
		// A failed resend is not counted as sent.
		"sent 2 times,",
		"Next: connect this computer to the internet so it can fetch a fresh time proof, then retry.",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("missing %q: %s", want, stderr)
		}
	}
	if strings.Contains(stderr, "connect the device to the internet") {
		t.Fatalf("gave the device-side remedy for a stale host proof: %s", stderr)
	}
}

// Q4 of the mesh-1 report: after `wendy device unset-default` the picker chose
// the device, and the rejection went straight to "TLS authentication failed"
// without any time proof. A picked device now gets the same clock recovery, on
// the same endpoint, and its pin is still enforced on the recovered connection.
func TestPickedDeviceGetsClockRecovery(t *testing.T) {
	isolateConnectionRecovery(t)
	stubNonInteractive(t)
	writePinTestConfig(t, map[string]config.DevicePin{
		"wendyos-mesh-1": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"},
	})
	setPinCache(t)
	lan := &models.LANDevice{
		DisplayName: "Mesh 1",
		Hostname:    "wendyos-mesh-1.local",
		IPAddress:   "10.0.0.9",
		Port:        50051,
		IsMTLS:      true,
	}
	rejected := newTLSHandshakeRejectedError(errors.New("remote error: tls: bad certificate"))

	for _, tc := range []struct {
		name          string
		first         error
		wantBroadcast int
	}{
		{"clock rejection is recovered", rejected, 1},
		// A pinned device that rejected our certificate at any candidate
		// ends in the pinned refusal; that is the clock case too.
		{"pinned certificate rejection is recovered", &noAuthenticatedEndpointError{msg: "pinned, rejected", certSeen: true}, 1},
		// A refusal is never retried with a clock change, or handed to BLE.
		{"identity refusal is not", errDeviceIdentityRefused, 0},
		{"pinned device that never answered is not", &noAuthenticatedEndpointError{msg: "pinned, silent"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clockSkewSyncAttempted = false
			broadcasts := 0
			broadcastTimeFn = func(context.Context) error { broadcasts++; return nil }
			var targets []dialTarget
			origLadder := dialAgentLadderFn
			dialAgentLadderFn = func(_ context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
				targets = append(targets, target)
				if len(targets) == 1 {
					return nil, nil, tc.first
				}
				return &grpcclient.AgentConnection{IsMTLS: true, CertInfo: &config.CertificateInfo{OrganizationID: 7}}, nil, nil
			}
			t.Cleanup(func() { dialAgentLadderFn = origLadder })

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			picked := &models.DiscoveredDevice{
				DisplayName: lan.DisplayName,
				LAN:         lan,
				Bluetooth:   &models.BluetoothDevice{DisplayName: "Mesh 1"},
			}
			var sel *SelectedDevice
			var err error
			captureStderr(t, func() { sel, err = connectPickedLANDevice(ctx, picked, preferredLANAddress(*lan), true) })
			if broadcasts != tc.wantBroadcast {
				t.Fatalf("broadcasts=%d, want %d", broadcasts, tc.wantBroadcast)
			}
			if tc.wantBroadcast == 0 {
				if !errors.Is(err, tc.first) || sel != nil || len(targets) != 1 {
					t.Fatalf("sel=%+v err=%v dials=%d", sel, err, len(targets))
				}
				return
			}
			if err != nil || sel == nil || sel.Agent == nil || sel.PinKey != "wendyos-mesh-1.local" {
				t.Fatalf("sel=%+v err=%v", sel, err)
			}
			if len(targets) != 2 || targets[1].PinKey != targets[0].PinKey || targets[1].Addr != targets[0].Addr {
				t.Fatalf("retry changed target: %+v", targets)
			}
		})
	}
}
