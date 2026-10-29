package commands

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	litepb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

type fakeLiteUnenrollmentDevice struct {
	identity                                     *liteclient.DeviceIdentity
	err                                          error
	timeout                                      time.Duration
	closed                                       bool
	challenge                                    *litepb.WendyComEnrollmentChallenge
	challengeErr, resetErr, verifyErr, rebootErr error
	stillEnrolled                                bool
	remainingIdentity                            bool
	calls                                        []string
}

func (d *fakeLiteUnenrollmentDevice) GetDeviceIdentity(timeout time.Duration) (*liteclient.DeviceIdentity, error) {
	d.timeout = timeout
	return d.identity, d.err
}

func (d *fakeLiteUnenrollmentDevice) Close() error {
	d.closed = true
	return nil
}

func (d *fakeLiteUnenrollmentDevice) EnrollmentChallenge(statusOnly bool) (*litepb.WendyComEnrollmentChallenge, error) {
	if statusOnly && len(d.calls) > 0 {
		d.calls = append(d.calls, "verify")
		state := &litepb.WendyComEnrollmentChallenge{Enrolled: d.stillEnrolled}
		if d.remainingIdentity {
			state.DeviceId = "44444444-4444-4444-8444-444444444444"
		}
		return state, d.verifyErr
	}
	if statusOnly {
		d.calls = append(d.calls, "preflight")
	} else {
		d.calls = append(d.calls, "challenge")
	}
	if d.challenge != nil || d.challengeErr != nil {
		return d.challenge, d.challengeErr
	}
	return &litepb.WendyComEnrollmentChallenge{
		Enrolled: true, EnrollmentResetSupported: true, UuidEnrollmentSupported: true,
		DeviceId: liteUnenrollDeviceID, TenantId: liteUnenrollTenant, NonceHex: strings.Repeat("12", 32),
	}, nil
}

// The identity a fake enrolled board reports.
const (
	liteUnenrollDeviceID = "44444444-4444-4444-8444-444444444444"
	liteUnenrollTenant   = "11111111-1111-4111-8111-111111111111"
)

func (d *fakeLiteUnenrollmentDevice) ResetEnrollment(id, nonce string) error {
	if id != d.identity.ID || nonce != strings.Repeat("12", 32) {
		return errors.New("reset binding changed")
	}
	d.calls = append(d.calls, "reset")
	return d.resetErr
}

func (d *fakeLiteUnenrollmentDevice) ResetTargetDevice(autoStart bool, delay time.Duration) error {
	if !autoStart || delay != 0 {
		return errors.New("changed app startup behavior")
	}
	d.calls = append(d.calls, "reboot")
	return d.rebootErr
}

func (d *fakeLiteUnenrollmentDevice) WaitReboot(context.Context) {
	if d.closed {
		panic("waited for reboot after close")
	}
	d.calls = append(d.calls, "wait")
}

func liteUnenrollUSBTarget() models.ExternalDevice {
	return models.ExternalDevice{ID: "wendy-lite:discovery-id", ProviderKey: "wendy-lite", ConnectionInfo: map[string]string{"type": "USB"}}
}

func preserveLiteUnenrollmentHooks(t *testing.T) {
	t.Helper()
	connect, unenroll := connectLiteUnenrollmentFn, unenrollLiteCloudFn
	t.Cleanup(func() {
		connectLiteUnenrollmentFn, unenrollLiteCloudFn = connect, unenroll
	})
}

func TestLiteUnenrollResetsAfterCloudCleanup(t *testing.T) {
	preserveLiteUnenrollmentHooks(t)
	for _, mode := range []string{"success", "cancel", "cloud failure"} {
		t.Run(mode, func(t *testing.T) {
			target := liteUnenrollUSBTarget()
			device := &fakeLiteUnenrollmentDevice{identity: &liteclient.DeviceIdentity{ID: "441bf6804824", Name: "old-name"}}
			connectLiteUnenrollmentFn = func(got models.ExternalDevice) (liteUnenrollmentDevice, error) {
				if !reflect.DeepEqual(got, target) {
					t.Fatalf("connected to %v, want %v", got, target)
				}
				return device, nil
			}
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			called := false
			wantErr := errors.New("cloud unenrollment result")
			unenrollLiteCloudFn = func(gotCmd *cobra.Command, cloudGRPC, deviceID, tenantID, hardwareID string, assumeYes bool, reset func() error) error {
				called = true
				if device.closed {
					t.Fatal("device connection closed before reset")
				}
				if gotCmd != cmd || cloudGRPC != "cloud.example:443" || deviceID != liteUnenrollDeviceID || tenantID != liteUnenrollTenant || hardwareID != "" || !assumeYes {
					t.Fatal("wrong cloud unenrollment arguments")
				}
				device.calls = append(device.calls, "cloud")
				if mode == "cloud failure" {
					return wantErr
				}
				if mode == "cancel" {
					return nil
				}
				return reset()
			}
			err := runLiteDeviceUnenroll(cmd, target, "cloud.example:443", true)
			if !called || (mode == "cloud failure" && !errors.Is(err, wantErr)) || (mode != "cloud failure" && err != nil) {
				t.Fatalf("cloud result lost: called=%v err=%v", called, err)
			}
			want := "preflight,cloud"
			if mode == "success" {
				want += ",challenge,reset,verify,reboot,wait"
			}
			if strings.Join(device.calls, ",") != want || !device.closed {
				t.Fatalf("reset ordering or close: %v, closed=%v", device.calls, device.closed)
			}
			if device.timeout != 5*time.Second {
				t.Fatalf("unbounded or changed identity timeout: %v", device.timeout)
			}
		})
	}
}

func TestLiteUnenrollUsesReportedUUIDAndKeepsHardwareResetBinding(t *testing.T) {
	preserveLiteUnenrollmentHooks(t)
	const deviceID = "44444444-4444-4444-8444-444444444444"
	d := &fakeLiteUnenrollmentDevice{
		identity: &liteclient.DeviceIdentity{ID: "80456b2d29b4"},
		challenge: &litepb.WendyComEnrollmentChallenge{
			Enrolled: true, EnrollmentResetSupported: true, UuidEnrollmentSupported: true,
			DeviceId: deviceID, TenantId: "11111111-1111-4111-8111-111111111111", NonceHex: strings.Repeat("12", 32),
		},
	}
	connectLiteUnenrollmentFn = func(models.ExternalDevice) (liteUnenrollmentDevice, error) { return d, nil }
	called := false
	unenrollLiteCloudFn = func(_ *cobra.Command, _, id, tenantID, hardwareID string, _ bool, reset func() error) error {
		called = true
		if id != deviceID {
			t.Fatalf("cloud identity = %q", id)
		}
		return reset()
	}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runLiteDeviceUnenroll(cmd, liteUnenrollUSBTarget(), "", true); err != nil {
		t.Fatal(err)
	}
	if !called || strings.Join(d.calls, ",") != "preflight,challenge,reset,verify,reboot,wait" {
		t.Fatalf("incomplete UUID unenrollment: %v", d.calls)
	}
	d.calls = nil
	d.challenge.DeviceId = ""
	called = false
	if err := runLiteDeviceUnenroll(cmd, liteUnenrollUSBTarget(), "", true); err == nil || called {
		t.Fatal("UUID-capable enrolled board without identity reached cloud cleanup")
	}
}

func TestLiteUnenrollRequiresUsableIdentity(t *testing.T) {
	preserveLiteUnenrollmentHooks(t)
	wantErr := errors.New("firmware unavailable")
	for _, tc := range []struct {
		name        string
		connectErr  error
		identity    *liteclient.DeviceIdentity
		identityErr error
		want        string
	}{
		{name: "connection failed", connectErr: wantErr, want: "connecting to Wendy Lite device"},
		{name: "identity failed", identityErr: wantErr, want: "reading Wendy Lite device identity"},
		{name: "missing identity", want: "returned no device ID"},
		{name: "empty device ID", identity: &liteclient.DeviceIdentity{}, want: "returned no device ID"},
		{name: "blank device ID", identity: &liteclient.DeviceIdentity{ID: " \t"}, want: "returned no device ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			device := &fakeLiteUnenrollmentDevice{identity: tc.identity, err: tc.identityErr}
			connectLiteUnenrollmentFn = func(models.ExternalDevice) (liteUnenrollmentDevice, error) {
				if tc.connectErr != nil {
					return nil, tc.connectErr
				}
				return device, nil
			}
			unenrollLiteCloudFn = func(*cobra.Command, string, string, string, string, bool, func() error) error {
				t.Fatal("attempted cloud removal without the device identity")
				return nil
			}
			err := runLiteDeviceUnenroll(&cobra.Command{}, liteUnenrollUSBTarget(), "", false)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "--cloud-only --device <asset-id-or-name>") {
				t.Fatalf("missing error or cloud recovery instructions: %v", err)
			}
			if (tc.connectErr != nil || tc.identityErr != nil) && !errors.Is(err, wantErr) {
				t.Fatalf("underlying error lost: %v", err)
			}
			if tc.connectErr == nil && !device.closed {
				t.Fatal("device connection was not closed")
			}
		})
	}
}

func TestLiteUnenrollPreservesCloudConfirmation(t *testing.T) {
	preserveLiteUnenrollmentHooks(t)
	connectLiteUnenrollmentFn = func(models.ExternalDevice) (liteUnenrollmentDevice, error) {
		return &fakeLiteUnenrollmentDevice{identity: &liteclient.DeviceIdentity{ID: "441bf6804824"}}, nil
	}
	called := false
	unenrollLiteCloudFn = func(_ *cobra.Command, cloudGRPC, deviceID, tenantID, hardwareID string, assumeYes bool, _ func() error) error {
		called = true
		if assumeYes || cloudGRPC != "" {
			t.Fatal("local Lite path bypassed confirmation or changed the selected cloud")
		}
		return nil
	}
	if err := runLiteDeviceUnenroll(&cobra.Command{}, liteUnenrollUSBTarget(), "", false); err != nil || !called {
		t.Fatalf("cloud removal failed: called=%v err=%v", called, err)
	}
}

func TestLiteUnenrollPreflightPreservesCloud(t *testing.T) {
	for _, tc := range []struct {
		name, transport string
		challenge       *litepb.WendyComEnrollmentChallenge
		challengeErr    error
	}{
		{name: "LAN", transport: "LAN"},
		{name: "Bluetooth", transport: "BLE"},
		{name: "old firmware", transport: "USB", challenge: &litepb.WendyComEnrollmentChallenge{}},
		{name: "challenge failed", transport: "USB", challengeErr: errors.New("unsupported")},
		{name: "missing UUID tenant", transport: "USB", challenge: &litepb.WendyComEnrollmentChallenge{EnrollmentResetSupported: true, UuidEnrollmentSupported: true, DeviceId: "44444444-4444-4444-8444-444444444444"}},
		{name: "invalid tenant", transport: "USB", challenge: &litepb.WendyComEnrollmentChallenge{EnrollmentResetSupported: true, UuidEnrollmentSupported: true, DeviceId: "44444444-4444-4444-8444-444444444444", TenantId: "bad"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			preserveLiteUnenrollmentHooks(t)
			device := &fakeLiteUnenrollmentDevice{identity: &liteclient.DeviceIdentity{ID: "441bf6804824"}, challenge: tc.challenge, challengeErr: tc.challengeErr}
			connectLiteUnenrollmentFn = func(models.ExternalDevice) (liteUnenrollmentDevice, error) {
				if tc.transport != "USB" {
					t.Fatal("non-USB reset connected")
				}
				return device, nil
			}
			unenrollLiteCloudFn = func(*cobra.Command, string, string, string, string, bool, func() error) error {
				t.Fatal("preflight failure removed the cloud enrollment")
				return nil
			}
			target := liteUnenrollUSBTarget()
			target.ConnectionInfo["type"] = tc.transport
			if err := runLiteDeviceUnenroll(&cobra.Command{}, target, "", true); err == nil {
				t.Fatal("unsupported reset succeeded")
			}
		})
	}
}

func TestLiteUnenrollChecksResetAndReboot(t *testing.T) {
	for _, mode := range []string{"bad nonce", "reset", "verification", "still enrolled", "pending identity remains", "reboot"} {
		t.Run(mode, func(t *testing.T) {
			preserveLiteUnenrollmentHooks(t)
			device := &fakeLiteUnenrollmentDevice{identity: &liteclient.DeviceIdentity{ID: "441bf6804824"}}
			failure := errors.New("failed")
			wantCalls := "preflight,challenge,reset"
			switch mode {
			case "bad nonce":
				device.challenge = &litepb.WendyComEnrollmentChallenge{EnrollmentResetSupported: true, UuidEnrollmentSupported: true, NonceHex: "invalid"}
				wantCalls = "preflight,challenge"
			case "reset":
				device.resetErr = failure
			case "verification":
				device.verifyErr = failure
				wantCalls += ",verify"
			case "still enrolled":
				device.stillEnrolled = true
				wantCalls += ",verify"
			case "pending identity remains":
				device.remainingIdentity = true
				wantCalls += ",verify"
			case "reboot":
				device.rebootErr = failure
				wantCalls += ",verify,reboot"
			}
			connectLiteUnenrollmentFn = func(models.ExternalDevice) (liteUnenrollmentDevice, error) { return device, nil }
			unenrollLiteCloudFn = func(_ *cobra.Command, _, _, _, _ string, _ bool, reset func() error) error { return reset() }
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			if err := runLiteDeviceUnenroll(cmd, liteUnenrollUSBTarget(), "", true); err == nil {
				t.Fatal("partial reset reported success")
			}
			if strings.Join(device.calls, ",") != wantCalls || !device.closed {
				t.Fatalf("continued after failure: %v", device.calls)
			}
		})
	}
}

func TestLiteUnenrollChecksConfiguredTenantBeforeAbsentAssetReset(t *testing.T) {
	for _, mode := range []string{"wrong tenant", "same tenant", "same tenant asset", "unconfigured"} {
		t.Run(mode, func(t *testing.T) {
			preserveLiteUnenrollmentHooks(t)
			preserveCloudUnenrollmentHooks(t)
			service := &unenrollConfirmationServer{} // Authenticated empty tenant inventory.
			auth := startUnenrollResetServer(t, service)
			if mode == "same tenant asset" {
				service.asset = unenrollTestCloudDevice("44444444-4444-4444-8444-444444444444", "desk", "44444444-4444-4444-8444-444444444444").v2
			}
			tenant := auth.Certificates[0].TenantUUID()
			challenge := &litepb.WendyComEnrollmentChallenge{
				UuidEnrollmentSupported: true, EnrollmentResetSupported: true,
				DeviceId: "44444444-4444-4444-8444-444444444444", TenantId: tenant,
				NonceHex: strings.Repeat("12", 32),
			}
			if mode == "wrong tenant" {
				challenge.TenantId = "22222222-2222-4222-8222-222222222222"
			} else if mode == "unconfigured" {
				challenge.DeviceId, challenge.TenantId = "", ""
			}
			d := &fakeLiteUnenrollmentDevice{identity: &liteclient.DeviceIdentity{ID: "80456b2d29b4"}, challenge: challenge}
			connectLiteUnenrollmentFn = func(models.ExternalDevice) (liteUnenrollmentDevice, error) { return d, nil }
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			err := runLiteDeviceUnenroll(cmd, liteUnenrollUSBTarget(), "", true)
			if mode == "wrong tenant" {
				if err == nil || !strings.Contains(err.Error(), "tenant") || strings.Join(d.calls, ",") != "preflight" {
					t.Fatalf("other organization's absence authorized reset: err=%v calls=%v", err, d.calls)
				}
			} else if err != nil || strings.Join(d.calls, ",") != "preflight,challenge,reset,verify,reboot,wait" {
				t.Fatalf("same-tenant/unconfigured recovery failed: err=%v calls=%v", err, d.calls)
			}
			var wantDeletes int32
			if mode == "same tenant asset" {
				wantDeletes = 1
			}
			// The server answers as Cloud does for a member: the board is reset
			// after DeleteAsset alone, with no separate certificate revocation.
			if service.revokes.Load() != 0 || service.deletes.Load() != wantDeletes {
				t.Fatalf("cloud cleanup did not match the configured tenant inventory: revokes=%d deletes=%d", service.revokes.Load(), service.deletes.Load())
			}
		})
	}
}

func TestLiteUnenrollRejectsChangedUUIDAfterCloudCleanup(t *testing.T) {
	preserveLiteUnenrollmentHooks(t)
	d := &fakeLiteUnenrollmentDevice{
		identity: &liteclient.DeviceIdentity{ID: "80456b2d29b4"},
		challenge: &litepb.WendyComEnrollmentChallenge{
			UuidEnrollmentSupported: true, EnrollmentResetSupported: true,
			DeviceId: "44444444-4444-4444-8444-444444444444", TenantId: "11111111-1111-4111-8111-111111111111",
			NonceHex: strings.Repeat("12", 32),
		},
	}
	connectLiteUnenrollmentFn = func(models.ExternalDevice) (liteUnenrollmentDevice, error) { return d, nil }
	unenrollLiteCloudFn = func(_ *cobra.Command, _, _, _, _ string, _ bool, reset func() error) error {
		d.challenge.DeviceId = "55555555-5555-4555-8555-555555555555"
		return reset()
	}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runLiteDeviceUnenroll(cmd, liteUnenrollUSBTarget(), "", true); err == nil || !strings.Contains(err.Error(), "identity changed") || strings.Join(d.calls, ",") != "preflight,challenge" {
		t.Fatalf("reset a replacement enrollment: err=%v calls=%v", err, d.calls)
	}
}

func TestLiteUnenrollRefusesFirmwareWithoutUUIDIdentity(t *testing.T) {
	preserveLiteUnenrollmentHooks(t)
	// Such firmware reports no configured identity; never derive lite-<hw>.
	device := &fakeLiteUnenrollmentDevice{identity: &liteclient.DeviceIdentity{ID: "441bf6804824"}, challenge: &litepb.WendyComEnrollmentChallenge{Enrolled: true, EnrollmentResetSupported: true}}
	connectLiteUnenrollmentFn = func(models.ExternalDevice) (liteUnenrollmentDevice, error) { return device, nil }
	unenrollLiteCloudFn = func(*cobra.Command, string, string, string, string, bool, func() error) error {
		t.Fatal("firmware without a UUID identity reached Cloud")
		return nil
	}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	err := runLiteDeviceUnenroll(cmd, liteUnenrollUSBTarget(), "", true)
	if err == nil || !strings.Contains(err.Error(), "update the firmware") || strings.Join(device.calls, ",") != "preflight" {
		t.Fatalf("want an update-firmware refusal before any change, got %v (calls %v)", err, device.calls)
	}
}

func TestLiteUnenrollFindsAnUnconfiguredBoardBySerial(t *testing.T) {
	preserveLiteUnenrollmentHooks(t)
	device := &fakeLiteUnenrollmentDevice{identity: &liteclient.DeviceIdentity{ID: "441bf6804824"}, challenge: &litepb.WendyComEnrollmentChallenge{
		EnrollmentResetSupported: true, UuidEnrollmentSupported: true, NonceHex: strings.Repeat("12", 32),
	}}
	connectLiteUnenrollmentFn = func(models.ExternalDevice) (liteUnenrollmentDevice, error) { return device, nil }
	called := false
	unenrollLiteCloudFn = func(_ *cobra.Command, _, deviceID, tenantID, hardwareID string, _ bool, reset func() error) error {
		called = true
		if deviceID != "" || tenantID != "" || hardwareID != "441bf6804824" {
			t.Fatalf("want lookup by serial 441bf6804824, got device %q tenant %q serial %q", deviceID, tenantID, hardwareID)
		}
		return reset()
	}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runLiteDeviceUnenroll(cmd, liteUnenrollUSBTarget(), "", true); err != nil || !called {
		t.Fatalf("unenroll: called=%v err=%v", called, err)
	}
}
