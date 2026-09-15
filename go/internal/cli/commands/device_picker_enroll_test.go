package commands

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func restorePickerEnrollmentFunctions(t *testing.T) {
	t.Helper()
	connect, wifi, enroll := connectPickerEnrollmentFn, promptPickerEnrollmentWifiFn, runPickerEnrollmentFn
	t.Cleanup(func() {
		connectPickerEnrollmentFn, promptPickerEnrollmentWifiFn, runPickerEnrollmentFn = connect, wifi, enroll
	})
}

func TestEnrollLocalPickerDeviceUsesHighlightedDevice(t *testing.T) {
	for _, enrollmentErr := range []error{nil, errors.New("enrollment failed")} {
		name := "success"
		if enrollmentErr != nil {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			restorePickerEnrollmentFunctions(t)
			oldDevice, oldJSON := deviceFlag, jsonOutput
			deviceFlag, jsonOutput = "another-device.local", false
			t.Cleanup(func() { deviceFlag, jsonOutput = oldDevice, oldJSON })

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			item := &tui.PickerItem{Name: "Highlighted device", Address: "192.0.2.10"}
			auth := &config.AuthConfig{
				CloudGRPC:    "selected-cloud.example:443",
				Certificates: []config.CertificateInfo{{OrganizationID: 27}},
			}
			closed := false
			conn := &grpcclient.AgentConnection{
				Host:         item.Address,
				ExtraClosers: []io.Closer{closeTracker{closed: &closed}},
			}
			var calls []string
			connectPickerEnrollmentFn = func(gotCtx context.Context, gotItem *tui.PickerItem, suppressUpdateCheck bool) (*SelectedDevice, error) {
				if gotCtx != ctx || gotItem != item || !suppressUpdateCheck {
					t.Fatalf("connect received context=%v item=%p suppressUpdateCheck=%v", gotCtx == ctx, gotItem, suppressUpdateCheck)
				}
				calls = append(calls, "connect")
				return &SelectedDevice{Agent: conn}, nil
			}
			promptPickerEnrollmentWifiFn = func(gotCtx context.Context, gotConn *grpcclient.AgentConnection) {
				if gotCtx != ctx || gotConn != conn || closed {
					t.Fatal("WiFi prompt did not receive the live highlighted connection")
				}
				calls = append(calls, "wifi")
			}
			runPickerEnrollmentFn = func(gotCtx context.Context, gotConn *grpcclient.AgentConnection, gotAuth *config.AuthConfig, name string, orgOverride int32) error {
				if gotCtx != ctx || gotConn != conn || gotAuth != auth || name != "" || orgOverride != 0 || closed {
					t.Fatal("enrollment did not receive the live highlighted connection and selected cloud session with normal name/org prompts")
				}
				calls = append(calls, "enroll")
				return enrollmentErr
			}

			stderr := captureStderr(t, func() {
				if err := enrollLocalPickerDevice(ctx, item, auth, true); !errors.Is(err, enrollmentErr) {
					t.Fatalf("enrollLocalPickerDevice() error = %v, want %v", err, enrollmentErr)
				}
			})
			if stderr != "" {
				t.Errorf("enrollment printed an unnecessary provisioning hint: %q", stderr)
			}
			if !reflect.DeepEqual(calls, []string{"connect", "wifi", "enroll"}) {
				t.Errorf("workflow calls = %v", calls)
			}
			if !closed {
				t.Error("enrollment left the device connection open")
			}
			if deviceFlag != "another-device.local" {
				t.Errorf("enrollment changed --device to %q", deviceFlag)
			}
		})
	}
}

func TestEnrollLocalPickerDeviceRequiresCloudAuthBeforeConnecting(t *testing.T) {
	restorePickerEnrollmentFunctions(t)
	connectPickerEnrollmentFn = func(context.Context, *tui.PickerItem, bool) (*SelectedDevice, error) {
		t.Fatal("connected without a usable cloud account")
		return nil, nil
	}
	for _, auth := range []*config.AuthConfig{nil, {CloudGRPC: "cloud.example:443"}} {
		if err := enrollLocalPickerDevice(context.Background(), nil, auth, false); err == nil {
			t.Error("expected an authentication error")
		}
	}
}

func TestEnrollLocalPickerDeviceRejectsBluetoothFallback(t *testing.T) {
	restorePickerEnrollmentFunctions(t)
	connectPickerEnrollmentFn = func(context.Context, *tui.PickerItem, bool) (*SelectedDevice, error) {
		return &SelectedDevice{Bluetooth: &models.BluetoothDevice{DisplayName: "Highlighted device"}}, nil
	}
	promptPickerEnrollmentWifiFn = func(context.Context, *grpcclient.AgentConnection) {
		t.Fatal("prompted for WiFi on an unsupported enrollment transport")
	}
	runPickerEnrollmentFn = func(context.Context, *grpcclient.AgentConnection, *config.AuthConfig, string, int32) error {
		t.Fatal("attempted enrollment on an unsupported transport")
		return nil
	}
	auth := &config.AuthConfig{Certificates: []config.CertificateInfo{{OrganizationID: 27}}}
	err := enrollLocalPickerDevice(context.Background(), &tui.PickerItem{Name: "Highlighted device"}, auth, false)
	if err == nil || !strings.Contains(err.Error(), "requires a LAN connection") {
		t.Fatalf("enrollLocalPickerDevice() error = %v, want a LAN requirement", err)
	}
}
