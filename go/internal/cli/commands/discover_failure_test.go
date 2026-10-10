package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDiscoveryFailureSurfaces(t *testing.T) {
	for _, tc := range []struct {
		name, code, phrase string
		err                error
	}{
		{"refused", "connection_refused", "connection refused", errors.New("rpc error: code = Unavailable desc = dial tcp: connection refused")},
		{"timeout", "timeout", "probe deadline", context.DeadlineExceeded},
		{"grpc timeout", "timeout", "probe deadline", status.Error(codes.DeadlineExceeded, "context deadline exceeded")},
		{"identity", "device_identity_mismatch", "saved pin", identityRefusal("board.local", &certs.IdentityMismatchError{WantOrg: 2, WantAsset: "344", GotOrg: 76, GotAsset: "472"})},
		{"key changed", "device_identity_mismatch", "saved pin", &devicepin.PinMismatchError{Key: "board"}},
		{"wrong org", "device_org_mismatch", "organization", orgMismatchDeviceError{deviceOrg: 76}},
		{"TLS alert", "tls_rejected", "wrong device clock", newTLSHandshakeRejectedError(errors.New("remote error: tls: bad certificate"))},
		{"TLS timeout", "timeout", "probe deadline", newTLSHandshakeRejectedError(context.DeadlineExceeded)},
		{"missing credentials", "credentials_missing", "no client certificates", errProvisionedAgentUnauthorized},
		{"unknown", "probe_failed", "could not verify", errors.New("secret-token\x1b[31m")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setTempConfig(t, &config.Config{Auth: []config.AuthConfig{{Certificates: certsForOrgs(1)}}})
			old := getAgentVersionAtAddress
			t.Cleanup(func() { getAgentVersionAtAddress = old })
			getAgentVersionAtAddress = func(context.Context, string) (bool, *agentpb.GetAgentVersionResponse, error) {
				return false, nil, tc.err
			}
			dev := models.LANDevice{ID: "board", DisplayName: "board", IPAddress: "192.0.2.1", Port: 50052, IsMTLS: true, AgentVersion: "cached-version"}
			failed, err := lanProber(context.Background(), dev)
			if err == nil || failed.ProbeFailure == nil || failed.ProbeFailure.Code != tc.code {
				t.Fatalf("diagnostic = %+v, error = %v", failed.ProbeFailure, err)
			}
			event := discovery.LANEvent{Kind: discovery.LANUpdated, Device: failed, ProbeFailed: true}
			m := newDiscoverModel(context.Background(), defaultOpts(), true)
			updated, _ := m.Update(lanEventMsg{ev: event})
			dm := updated.(discoverModel)
			pickerEvent := lanPickerEventMsg(event).(tui.PickerAddMsg)
			picker := pickerEvent.Items[0]
			pm, _ := tui.NewPicker().Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			pm, _ = pm.(tui.PickerModel).Update(pickerEvent)
			collection := &models.DevicesCollection{LANDevices: []models.LANDevice{failed}}
			data, err := json.Marshal(collection)
			if err != nil {
				t.Fatal(err)
			}
			clipboard, err := json.Marshal(dm.tableItems[0].info)
			if err != nil {
				t.Fatal(err)
			}
			for surface, output := range map[string]string{"TUI": ansi.Strip(dm.View()), "text": ansi.Strip(renderDeviceTable(collection)), "picker": ansi.Strip(pm.(tui.PickerModel).View()), "JSON": string(data), "clipboard": string(clipboard)} {
				if !strings.Contains(strings.Join(strings.Fields(output), " "), tc.phrase) {
					t.Errorf("%s missing %q: %s", surface, tc.phrase, output)
				}
				if strings.Contains(output, "secret-token") || strings.Contains(output, "ssh wendy") {
					t.Errorf("%s leaked raw error/advice: %s", surface, output)
				}
				if tc.code != "credentials_missing" && tc.code != "device_org_mismatch" && strings.Contains(output, "auth login") {
					t.Errorf("%s incorrectly suggests login: %s", surface, output)
				}
			}
			if !strings.Contains(string(data), `"code":"`+tc.code+`"`) {
				t.Fatalf("missing JSON failure code: %s", data)
			}
			// The same target recovering removes stale diagnostics even if passed back
			// into the prober with its previous failure attached.
			getAgentVersionAtAddress = func(context.Context, string) (bool, *agentpb.GetAgentVersionResponse, error) {
				return true, &agentpb.GetAgentVersionResponse{Version: "1.2.3"}, nil
			}
			recovered, err := lanProber(context.Background(), failed)
			if err != nil || recovered.ProbeFailure != nil {
				t.Fatalf("recovery retained failure: %+v / %v", recovered, err)
			}
			updated, _ = dm.Update(lanEventMsg{ev: discovery.LANEvent{Kind: discovery.LANUpdated, Device: recovered, Probed: true}})
			if updated.(discoverModel).selectedHint() != "" {
				t.Fatal("recovery retained hint")
			}
			mergePickerItem(&picker, lanPickerItem(recovered, false, tui.ProbeOK))
			if picker.Hint != "" {
				t.Fatal("picker retained hint after recovery")
			}
			data, _ = json.Marshal(recovered)
			if strings.Contains(string(data), "probeFailure") {
				t.Fatalf("recovered JSON has diagnostic: %s", data)
			}
		})
	}
}

func TestDiscoveryAdvertisementAloneDoesNotDiagnoseAccess(t *testing.T) {
	dev := models.LANDevice{DisplayName: "board", IsMTLS: true}
	if hint := lanProbeHint(&dev); hint != "" {
		t.Fatalf("mDNS alone produced hint: %s", hint)
	}
	if output := renderDeviceTable(&models.DevicesCollection{LANDevices: []models.LANDevice{dev}}); strings.Contains(output, "auth login") {
		t.Fatal(output)
	}
}

func TestDiscoveryCredentialEvidence(t *testing.T) {
	dev := models.LANDevice{IsMTLS: true}
	rejected := newTLSHandshakeRejectedError(errors.New("remote error: tls: bad certificate"))
	expired := config.CertificateInfo{PemCertificate: certPEM(t, time.Now().Add(-time.Hour))}
	valid := config.CertificateInfo{PemCertificate: certPEM(t, time.Now().Add(time.Hour))}
	for _, tc := range []struct {
		name  string
		certs []config.CertificateInfo
		err   error
		want  string
	}{
		{"no cert for secure endpoint", nil, context.DeadlineExceeded, "credentials_missing"},
		{"expired", []config.CertificateInfo{expired}, rejected, "credentials_expired"},
		{"valid alternative", []config.CertificateInfo{expired, valid}, rejected, "tls_rejected"},
		{"expired does not explain refusal", []config.CertificateInfo{expired}, errors.New("connection refused"), "connection_refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := lanProbeFailure(dev, tc.err, tc.certs)
			if got.Code != tc.want {
				t.Fatalf("got %+v, want %s", got, tc.want)
			}
		})
	}
}

func TestDiscoveryMultipleAddressFailures(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	old := getAgentVersionAtAddress
	t.Cleanup(func() { getAgentVersionAtAddress = old })
	dev := models.LANDevice{IPAddress: "192.0.2.1", Hostname: "board.local", Port: 50052, IsMTLS: true}
	rejection := newTLSHandshakeRejectedError(errors.New("remote error: tls: bad certificate"))
	for _, tc := range []struct {
		name   string
		first  error
		second error
		code   string
		calls  int
	}{
		{"TLS then timeout", rejection, context.DeadlineExceeded, "tls_rejected", 2},
		{"refusal then timeout", errors.New("connection refused"), context.DeadlineExceeded, "connection_refused", 2},
		{"identity must stop", errDeviceIdentityRefused, nil, "device_identity_mismatch", 1},
		{"second address recovers", rejection, nil, "", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			getAgentVersionAtAddress = func(context.Context, string) (bool, *agentpb.GetAgentVersionResponse, error) {
				calls++
				err := tc.first
				if calls > 1 {
					err = tc.second
				}
				return true, &agentpb.GetAgentVersionResponse{Version: "1.2.3"}, err
			}
			_, _, _, err := resolveLANAgentVersion(context.Background(), dev)
			if calls != tc.calls {
				t.Fatalf("calls = %d, want %d", calls, tc.calls)
			}
			if tc.code == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if failure := classifyLANProbeFailure(err); failure == nil || failure.Code != tc.code {
				t.Fatalf("got %+v, want %s", failure, tc.code)
			}
		})
	}
}

func TestDiscoveryKeepsMTLSFailureWhenPlaintextProbeFails(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	t.Setenv("WENDY_AGENT_SOCKET", "")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer() // Answers TCP/gRPC, but does not serve the agent RPC.
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	old := dialAgentLadderFn
	t.Cleanup(func() { dialAgentLadderFn = old })
	for _, tc := range []struct {
		name  string
		cause error
		code  string
	}{
		{"refused", errors.New("connection refused"), "connection_refused"},
		{"rejected", errors.New("remote error: tls: bad certificate"), "tls_rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dialAgentLadderFn = func(ctx context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
				conn, err := grpcclient.Connect(ctx, target.Addr)
				return conn, tc.cause, err
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _, err := getAgentVersionAtAddress(ctx, listener.Addr().String())
			if !errors.Is(err, tc.cause) {
				t.Fatalf("plaintext RPC swallowed mTLS failure: %v", err)
			}
			if failure := classifyLANProbeFailure(err); failure == nil || failure.Code != tc.code {
				t.Fatalf("got %+v, want %s", failure, tc.code)
			}
		})
	}
}

// Exercise the actual sized views: checking PickerItem.Hint alone cannot catch
// a renderer cropping the recovery command after the first terminal line.
func TestDiscoveryRecoveryAdviceFitsTerminal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		advice string
	}{
		{"TLS", newTLSHandshakeRejectedError(errors.New("remote error: tls: bad certificate")), "wendy device sync-time"},
		{"identity", identityRefusal("saved-board.local", &certs.IdentityMismatchError{WantOrg: 2, WantAsset: "344", GotOrg: 76, GotAsset: "472"}), "wendy device unpin saved-board.local"},
	} {
		for _, width := range []int{80, 120} {
			for _, count := range []int{1, 30} {
				t.Run(fmt.Sprintf("%s/%dcols/%ddevices", tc.name, width, count), func(t *testing.T) {
					dm, _ := newDiscoverModel(context.Background(), defaultOpts(), true).Update(tea.WindowSizeMsg{Width: width, Height: 24})
					pm, _ := tui.NewPicker().Update(tea.WindowSizeMsg{Width: width, Height: 24})
					failure := classifyLANProbeFailure(tc.err)
					for i := 0; i < count; i++ {
						name := fmt.Sprintf("board-%02d", i)
						dev := models.LANDevice{ID: name, DisplayName: name, Hostname: name + ".local", Port: 50052, IsMTLS: true, ProbeFailure: failure}
						if i%3 == 2 {
							dev.ProbeFailure = nil
						}
						event := discovery.LANEvent{Kind: discovery.LANUpdated, Device: dev, ProbeFailed: dev.ProbeFailure != nil, Probed: dev.ProbeFailure == nil}
						dm, _ = dm.(discoverModel).Update(lanEventMsg{ev: event})
						pm, _ = pm.(tui.PickerModel).Update(lanPickerEventMsg(event))
					}
					assertView := func(selected, height int) {
						t.Helper()
						for surface, view := range map[string]string{"discover": dm.(discoverModel).View(), "picker": pm.(tui.PickerModel).View()} {
							plain := ansi.Strip(view)
							text := strings.Join(strings.Fields(plain), " ")
							wantText := append([]string{failure.Message, tc.advice}, failure.NextSteps...)
							if selected%3 == 2 {
								wantText = nil
							}
							for _, want := range wantText {
								if !strings.Contains(text, strings.Join(strings.Fields(want), " ")) {
									t.Errorf("%s hides %q:\n%s", surface, want, plain)
								}
							}
							lines := strings.Split(strings.TrimSuffix(plain, "\n"), "\n")
							if len(lines) > height {
								t.Errorf("%s exceeds terminal height (%d):\n%s", surface, len(lines), plain)
							}
							for _, line := range lines {
								if ansi.StringWidth(line) > width {
									t.Errorf("%s exceeds terminal width: %q", surface, line)
								}
							}
							if !strings.Contains(plain, fmt.Sprintf("board-%02d", selected)) {
								t.Errorf("%s hides selected device:\n%s", surface, plain)
							}
						}
					}
					// Check every intermediate selection, in both directions. Rendering only
					// the final row missed a viewport that hid the cursor midway down a list.
					for i := 0; i < count; i++ {
						assertView(i, 24)
						if i == count/2 {
							for _, height := range []int{18, 24} {
								dm, _ = dm.(discoverModel).Update(tea.WindowSizeMsg{Width: width, Height: height})
								pm, _ = pm.(tui.PickerModel).Update(tea.WindowSizeMsg{Width: width, Height: height})
								assertView(i, height)
							}
						}
						if i+1 < count {
							dm, _ = dm.(discoverModel).Update(tea.KeyMsg{Type: tea.KeyDown})
							pm, _ = pm.(tui.PickerModel).Update(tea.KeyMsg{Type: tea.KeyDown})
						}
					}
					for i := count - 1; i >= 0; i-- {
						assertView(i, 24)
						dm, _ = dm.(discoverModel).Update(tea.KeyMsg{Type: tea.KeyUp})
						pm, _ = pm.(tui.PickerModel).Update(tea.KeyMsg{Type: tea.KeyUp})
					}
				})
			}
		}
	}
}

func TestDiscoveryExplicitTLSAlertPrecedesTransportFailure(t *testing.T) {
	for _, alert := range []string{"bad certificate", "unknown certificate authority", "handshake failure"} {
		for _, tc := range []struct {
			name string
			err  error
		}{
			{"deadline wrapper", newTLSHandshakeRejectedError(status.Error(codes.DeadlineExceeded, `context deadline exceeded: transport: authentication handshake failed: remote error: tls: `+alert))},
			{"other address refused", errors.Join(newTLSHandshakeRejectedError(errors.New("remote error: tls: "+alert)), errors.New("connection refused"))},
		} {
			t.Run(alert+"/"+tc.name, func(t *testing.T) {
				if got := classifyLANProbeFailure(tc.err); got.Code != "tls_rejected" {
					t.Fatalf("explicit TLS alert became %+v", got)
				}
			})
		}
	}
}

func TestDiscoveryIdentityRecoveryUsesGoverningPin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		want    []string
		command string
	}{
		{"alias", identityRefusal("lab's board.local", &certs.IdentityMismatchError{WantOrg: 2, WantAsset: "344", GotOrg: 76, GotAsset: "472"}), []string{"asset 344 in organization 2", "asset 472 in organization 76"}, `wendy device unpin 'lab'\''s board.local'`},
		{"SPKI store identity", spkiRefusal("192.0.2.1", &devicepin.PinMismatchError{Key: "urn:wendy:org:2:asset:344", Want: "sha256:old", Got: "sha256:new"}), []string{"sha256:old", "sha256:new"}, "wendy device unpin urn:wendy:org:2:asset:344"},
		{"unknown key", &certs.IdentityMismatchError{WantOrg: 2, WantAsset: "344", GotOrg: 76, GotAsset: "472"}, []string{"asset 344 in organization 2", "asset 472 in organization 76"}, ""},
		{"control in key", identityRefusal("board\x1b.local", &certs.IdentityMismatchError{}), nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := classifyLANProbeFailure(tc.err)
			text := failure.Message + " " + strings.Join(failure.NextSteps, " ")
			for _, want := range append(tc.want, "confirmed an intentional identity change") {
				if !strings.Contains(text, want) {
					t.Errorf("missing %q: %s", want, text)
				}
			}
			if tc.command == "" {
				if strings.Contains(text, "wendy device unpin") {
					t.Errorf("invented recovery target: %s", text)
				}
			} else if !strings.HasSuffix(text, tc.command) {
				t.Errorf("expected exact command %q, got %s", tc.command, text)
			}
			if text != tui.StripControl(text) {
				t.Errorf("unsafe diagnostic: %q", text)
			}
		})
	}
}

// Exercise the real two-port ladder, not a stub returning a preselected error.
// A TLS-only endpoint can reject our cert while its port+1 is simply closed.
func TestDiscoveryLadderPreservesTLSRejectionBeforePortRefusal(t *testing.T) {
	cert := selfSignedCLICert(t, 7)
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{{Certificates: []config.CertificateInfo{cert}}}})
	setPinCache(t)
	t.Setenv("WENDY_AGENT_SOCKET", "")
	listener, err := net.Listen("tcp", deadAgentAddr(t))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			// Consume a complete ClientHello record before sending a fatal TLS
			// bad_certificate alert. No server certificate or external PKI is needed.
			header := make([]byte, 5)
			if _, err := io.ReadFull(conn, header); err == nil {
				length := int64(header[3])<<8 | int64(header[4])
				if _, err := io.CopyN(io.Discard, conn, length); err == nil {
					_, _ = conn.Write([]byte{21, 3, 3, 0, 2, 2, 42})
				}
			}
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done })
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	dev := models.LANDevice{ID: "board", DisplayName: "board", IPAddress: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port}
	failed, err := lanProber(ctx, dev)
	if !errors.Is(err, errTLSHandshakeRejected) {
		t.Fatalf("expected the ladder to refuse TLS: %v", err)
	}
	if got := failed.ProbeFailure; got == nil || got.Code != "tls_rejected" || !strings.Contains(lanProbeHint(&failed), "wendy device sync-time") {
		t.Fatalf("TLS rejection was replaced by later port refusal: %+v / %v", got, err)
	}
}
