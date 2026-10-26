package commands

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	litepb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/protobuf/proto"
)

type enrollingLiteDevice struct {
	liteEnrollmentDevice
	conf     *litepb.WendyConf
	enrolled bool
}

func (*enrollingLiteDevice) GetDeviceIdentity(time.Duration) (*liteclient.DeviceIdentity, error) {
	return &liteclient.DeviceIdentity{ID: "441bf6804824", Name: "camera"}, nil
}

func (d *enrollingLiteDevice) EnrollmentChallenge(bool) (*litepb.WendyComEnrollmentChallenge, error) {
	return &litepb.WendyComEnrollmentChallenge{
		Enrolled: d.enrolled, NonceHex: strings.Repeat("12", 32),
		BuiltinRootsReady: true, BuiltinTsaRootsReady: true, UuidEnrollmentSupported: true,
	}, nil
}

func (d *enrollingLiteDevice) PushConf(conf *litepb.WendyConf, mode liteclient.ConfPushMode, _ func(uint32, uint32)) error {
	d.conf = proto.Clone(conf).(*litepb.WendyConf)
	return nil
}

func (d *enrollingLiteDevice) ResetTargetDevice(bool, time.Duration) error {
	d.enrolled = true
	return nil
}

func (*enrollingLiteDevice) Close() error { return nil }

func TestLiteEnrollmentUsesFreshUUIDThroughout(t *testing.T) {
	oldConnect, oldHTTP := connectLiteEnrollmentFn, http.DefaultClient
	t.Cleanup(func() { connectLiteEnrollmentFn, http.DefaultClient = oldConnect, oldHTTP })
	// This tests identity propagation; timestamp verification belongs to the board.
	tsa := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("device-verified-timestamp"))
	}))
	t.Cleanup(tsa.Close)
	http.DefaultClient = tsa.Client()
	cloud := &oidcEnrollmentServer{response: &cloudpb.EnrollDeviceResponse{
		CredentialKind: "enrollment_token", TokenValue: "single-use-test",
		ExpiresAt: time.Now().Add(4 * time.Minute).Format(time.RFC3339),
	}}
	_, endpoint := enrollmentServers(t, cloud, &acmeProvisioningServer{})
	auth := oidcEnrollmentAuth(t)
	auth.CloudGRPC = endpoint
	seen := map[string]bool{}
	// Enrolling the same hardware twice must mint distinct identities.
	for i := 0; i < 2; i++ {
		device := &enrollingLiteDevice{}
		connectLiteEnrollmentFn = func(models.ExternalDevice) (liteEnrollmentDevice, error) { return device, nil }
		cmd := &cobra.Command{}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd.SetContext(ctx)
		var out strings.Builder
		cmd.SetOut(&out)
		err := runEnrollLiteDevice(cmd, liteUnenrollUSBTarget(), auth, "camera", liteEnrollmentOptions{
			timeURL: tsa.URL, brokerHost: "broker.example", brokerPort: 5055,
		})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		id := cloud.req.GetDeviceId()
		parsed, err := uuid.Parse(id)
		if err != nil || parsed.String() != id || seen[id] {
			t.Fatalf("expected a fresh canonical UUID, got %q", id)
		}
		seen[id] = true
		if !strings.Contains(out.String(), "Enrolled camera as asset "+id+".") {
			t.Fatalf("Cloud asset is not the minted UUID: %q", out.String())
		}
		if device.conf.GetEnrollment().GetDeviceId() != id || device.conf.GetEnrollment().GetToken() != "single-use-test" {
			t.Fatal("USB configuration did not preserve the Cloud identity and credential")
		}
		if cloud.req.GetHardware().GetSerialNumber() != "441bf6804824" || cloud.req.GetName() != "camera" || cloud.req.GetDeviceClass() != cloudpb.DeviceClass_DEVICE_CLASS_C {
			t.Fatal("hardware serial, name or device class lost")
		}
		artifact := verifyEnrollmentJWS(t, string(cloud.req.GetEnrollmentRequestJws()))
		if artifact["device_id"] != id || artifact["device_class"] != "C" || artifact["tenant"] != device.conf.GetEnrollment().GetTenantId() {
			t.Fatal("signed enrollment claims differ from the Cloud request and USB configuration")
		}
	}
}

type alreadyEnrolledLiteDevice struct {
	liteEnrollmentDevice // Any provisioning operation would panic in this test.
	closed               bool
}

func TestLiteEnrollmentRejectsNetworkTransportsBeforeConnecting(t *testing.T) {
	old := connectLiteEnrollmentFn
	t.Cleanup(func() { connectLiteEnrollmentFn = old })
	connectLiteEnrollmentFn = func(models.ExternalDevice) (liteEnrollmentDevice, error) {
		t.Fatal("enrollment must reject network transports before opening a client")
		return nil, nil
	}
	for _, transport := range []string{"LAN", "BLE", "Cloud", "", "usb", "UART"} {
		t.Run(transport, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			target := models.ExternalDevice{
				ProviderKey: "wendy-lite",
				ConnectionInfo: map[string]string{
					"type": transport,
					// A serial path must not override the selected transport.
					"serialPort": "/dev/cu.usbmodem-test",
				},
			}
			err := runEnrollLiteDevice(cmd, target, nil, "", liteEnrollmentOptions{})
			if err == nil || !strings.Contains(err.Error(), "requires a USB connection") {
				t.Fatalf("expected USB requirement, got %v", err)
			}
		})
	}
}

func (*alreadyEnrolledLiteDevice) GetDeviceIdentity(time.Duration) (*liteclient.DeviceIdentity, error) {
	return &liteclient.DeviceIdentity{ID: "441bf6804824", Name: "joannis-home"}, nil
}

func (*alreadyEnrolledLiteDevice) EnrollmentChallenge(bool) (*litepb.WendyComEnrollmentChallenge, error) {
	return &litepb.WendyComEnrollmentChallenge{Enrolled: true}, nil
}

func (d *alreadyEnrolledLiteDevice) Close() error { d.closed = true; return nil }

func TestLiteEnrollmentReportsRemainingIdentityBeforeNameOrTrustSetup(t *testing.T) {
	old := connectLiteEnrollmentFn
	t.Cleanup(func() { connectLiteEnrollmentFn = old })
	device := &alreadyEnrolledLiteDevice{}
	connectLiteEnrollmentFn = func(models.ExternalDevice) (liteEnrollmentDevice, error) { return device, nil }
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	// Invalid name, auth and trust options must not hide the existing identity
	// or trigger another name prompt after a cloud-only unenrollment.
	err := runEnrollLiteDevice(cmd, liteUnenrollUSBTarget(), &config.AuthConfig{}, "\x1b[A", liteEnrollmentOptions{deviceRoots: "/missing.pem"})
	if err == nil || !strings.Contains(err.Error(), "still holds an issued identity") || !strings.Contains(err.Error(), "wendy device unenroll") || !strings.Contains(err.Error(), "USB") {
		t.Fatalf("missing concrete identity recovery instructions: %v", err)
	}
	if !device.closed {
		t.Fatal("enrollment connection leaked")
	}
}

type unsupportedLiteEnrollmentDevice struct {
	alreadyEnrolledLiteDevice
	supported *bool
	uuid      bool
}

func (d *unsupportedLiteEnrollmentDevice) EnrollmentChallenge(statusOnly bool) (*litepb.WendyComEnrollmentChallenge, error) {
	if !statusOnly {
		panic("unsupported firmware must not start an enrollment challenge")
	}
	return &litepb.WendyComEnrollmentChallenge{EnrollmentResetSupported: true, CloudEnrollmentSupported: d.supported, UuidEnrollmentSupported: d.uuid}, nil
}

func TestLiteEnrollmentRejectsUnsupportedFirmwareBeforeSetup(t *testing.T) {
	old := connectLiteEnrollmentFn
	t.Cleanup(func() { connectLiteEnrollmentFn = old })
	device := &unsupportedLiteEnrollmentDevice{supported: proto.Bool(false)}
	connectLiteEnrollmentFn = func(models.ExternalDevice) (liteEnrollmentDevice, error) { return device, nil }
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	// Reset support alone does not mean the firmware can request a certificate.
	// Reject it before prompting for a name, accessing trust or reserving an asset.
	err := runEnrollLiteDevice(cmd, liteUnenrollUSBTarget(), &config.AuthConfig{}, "\x1b[A", liteEnrollmentOptions{deviceRoots: "/missing.pem"})
	if err == nil || !strings.Contains(err.Error(), "was built without cloud enrollment support") {
		t.Fatalf("expected firmware capability error before enrollment setup, got %v", err)
	}
	if !device.closed {
		t.Fatal("enrollment connection leaked")
	}
}

func TestLiteEnrollmentRequiresUUIDFirmware(t *testing.T) {
	old := connectLiteEnrollmentFn
	t.Cleanup(func() { connectLiteEnrollmentFn = old })
	for _, tc := range []struct {
		name      string
		supported *bool
		uuid      bool
		want      string
	}{
		// Firmware that predates UUID identities is refused before a name,
		// trust or Cloud asset; it is never enrolled under its hardware id.
		{name: "no UUID support", supported: proto.Bool(true), want: "cannot enroll with a device UUID"},
		{name: "no capability reports", want: "cannot enroll with a device UUID"},
		// Reaching name validation proves UUID firmware may enroll; the
		// invalid name keeps this test from contacting Cloud.
		{name: "UUID firmware without enrollment report", uuid: true, want: "name may only contain"},
		{name: "UUID firmware", supported: proto.Bool(true), uuid: true, want: "name may only contain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			device := &unsupportedLiteEnrollmentDevice{supported: tc.supported, uuid: tc.uuid}
			connectLiteEnrollmentFn = func(models.ExternalDevice) (liteEnrollmentDevice, error) { return device, nil }
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			err := runEnrollLiteDevice(cmd, liteUnenrollUSBTarget(), &config.AuthConfig{}, "\x1b[A", liteEnrollmentOptions{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if !device.closed {
				t.Fatal("enrollment connection leaked")
			}
		})
	}
}

type pendingLiteEnrollmentDevice struct{ alreadyEnrolledLiteDevice }

func (*pendingLiteEnrollmentDevice) EnrollmentChallenge(bool) (*litepb.WendyComEnrollmentChallenge, error) {
	return &litepb.WendyComEnrollmentChallenge{
		UuidEnrollmentSupported: true,
		DeviceId:                "44444444-4444-4444-8444-444444444444",
		TenantId:                "11111111-1111-4111-8111-111111111111",
	}, nil
}

func TestLiteEnrollmentPreservesPendingUUIDBeforeSetup(t *testing.T) {
	old := connectLiteEnrollmentFn
	t.Cleanup(func() { connectLiteEnrollmentFn = old })
	device := &pendingLiteEnrollmentDevice{}
	connectLiteEnrollmentFn = func(models.ExternalDevice) (liteEnrollmentDevice, error) { return device, nil }
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	// A failed first enrollment may still hold its token and UUID. Stop
	// before even validating a new name, loading trust or reserving an asset.
	err := runEnrollLiteDevice(cmd, liteUnenrollUSBTarget(), &config.AuthConfig{}, "\x1b[A", liteEnrollmentOptions{deviceRoots: "/missing.pem"})
	if err == nil || !strings.Contains(err.Error(), "pending enrollment identity") || !strings.Contains(err.Error(), "wendy device unenroll") {
		t.Fatalf("missing pending enrollment recovery instructions: %v", err)
	}
	if !device.closed {
		t.Fatal("enrollment connection leaked")
	}
}
