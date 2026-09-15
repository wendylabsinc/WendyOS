//go:build windows

package commands

import (
	"errors"
	"io"
	"testing"
	"unsafe"

	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/winusb"
)

func TestUSBDriverSetupPreservesTargetAndVerifiesHelperResult(t *testing.T) {
	for _, tc := range []struct {
		name                                                                  string
		initialReady, elevated, helperRepairs, disconnect, decline, ambiguous bool
		wantErr                                                               bool
	}{
		{name: "compatible driver reused", initialReady: true},
		{name: "helper repairs selected device", helperRepairs: true},
		{name: "already elevated", elevated: true},
		{name: "helper success without binding", wantErr: true},
		{name: "selected device disconnected", helperRepairs: true, disconnect: true, wantErr: true},
		{name: "approval declined", decline: true, wantErr: true},
		{name: "ambiguous ID rejected before UAC", ambiguous: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := winusb.Device{InstanceID: `USB\VID_05C6&PID_9008\SELECTED`}
			var installed, elevated bool
			ops := usbDriverOperations{
				ready: func(d winusb.Device, _ winusb.DriverProfile) (bool, error) {
					if d.InstanceID != target.InstanceID {
						t.Fatal("substituted another device")
					}
					return tc.initialReady || (elevated && tc.helperRepairs), nil
				},
				validate: func(winusb.Device) error {
					if tc.ambiguous {
						return errors.New("disconnect other EDL device")
					}
					return nil
				},
				elevated: func() (bool, error) { return tc.elevated, nil },
				install: func(_ io.Writer, d winusb.Device, _ winusb.DriverProfile) error {
					if d.InstanceID != target.InstanceID {
						t.Fatal("installed another device")
					}
					installed = true
					return nil
				},
				elevate: func(instance string) error {
					if instance != target.InstanceID {
						t.Fatal("lost selection at UAC")
					}
					elevated = true
					if tc.decline {
						return errors.New("declined")
					}
					return nil
				},
				find: func(instance string) (winusb.Device, error) {
					if instance != target.InstanceID {
						t.Fatal("re-enumerated wrong target")
					}
					if tc.disconnect {
						return winusb.Device{}, errors.New("disconnected")
					}
					return target, nil
				},
			}
			err := ensureUSBDriverWith(io.Discard, target, winusb.DragonwingDriver(), ops)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v wantErr=%v", err, tc.wantErr)
			}
			if (tc.initialReady || tc.ambiguous) && (installed || elevated) {
				t.Fatal("unnecessary install/elevation")
			}
			if tc.elevated && (!installed || elevated) {
				t.Fatal("elevated parent should install directly")
			}
		})
	}
}

func TestUSBDriverHelperRegistration(t *testing.T) {
	root := NewRootCmd()
	cmd, _, err := root.Find([]string{"__usb-driver"})
	if err != nil || cmd.Name() != "__usb-driver" || !cmd.Hidden {
		t.Fatalf("helper missing or visible: %v", err)
	}
	if cmd.PersistentPreRunE == nil || cmd.PersistentPostRunE == nil {
		t.Fatal("helper inherits user configuration hooks")
	}
	if err := cmd.Args(cmd, nil); err == nil {
		t.Fatal("helper accepts missing identity")
	}
	if err := cmd.Args(cmd, []string{"a", "b"}); err == nil {
		t.Fatal("helper accepts multiple identities")
	}
	// ABI regression guard for amd64 and arm64 Windows ShellExecuteExW.
	var info shellExecuteInfo
	if unsafe.Sizeof(info) != 112 || unsafe.Offsetof(info.process) != 104 {
		t.Fatal("incorrect SHELLEXECUTEINFOW layout")
	}
}
