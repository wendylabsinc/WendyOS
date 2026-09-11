package central

import (
	"strings"
	"testing"
)

func TestFriendlyGATTError(t *testing.T) {
	tests := []struct {
		name     string
		errName  string
		message  string
		wantOK   bool
		contains string
		omits    string
	}{
		{
			// The override that matters most: on a device path this name means
			// "rescan", but a characteristic only disappears when the link
			// dropped, and telling the user to rescan would send them the wrong
			// way.
			name:     "missing characteristic blames the link, not the scan",
			errName:  "org.freedesktop.DBus.Error.UnknownObject",
			wantOK:   true,
			contains: "link dropped",
			omits:    "rescan",
		},
		{
			name:     "DoesNotExist gets the same treatment",
			errName:  "org.bluez.Error.DoesNotExist",
			wantOK:   true,
			contains: "reconnect",
			omits:    "rescan",
		},
		{
			// Failed on a characteristic carries an ATT error, which has none
			// of the bearer vocabulary the device-side table expects.
			name:     "Failed passes the ATT message through",
			errName:  "org.bluez.Error.Failed",
			message:  "Operation is not supported by the peer",
			wantOK:   true,
			contains: "not supported by the peer",
		},
		{
			name:     "Failed with no message still says something",
			errName:  "org.bluez.Error.Failed",
			wantOK:   true,
			contains: "rejected the operation",
		},
		{
			name:     "invalid value length names the MTU",
			errName:  "org.bluez.Error.InvalidValueLength",
			wantOK:   true,
			contains: "MTU",
		},
		{
			name:     "invalid arguments",
			errName:  "org.bluez.Error.InvalidArguments",
			wantOK:   true,
			contains: "arguments",
		},
		{
			// Delegated to bluez.FriendlyError, which means the same thing here.
			name:     "NotPermitted still suggests pairing",
			errName:  "org.bluez.Error.NotPermitted",
			wantOK:   true,
			contains: "bluetoothctl",
		},
		{
			name:     "AccessDenied still names the group",
			errName:  "org.freedesktop.DBus.Error.AccessDenied",
			wantOK:   true,
			contains: "bluetooth` group",
		},
		{
			name:     "NotConnected",
			errName:  "org.bluez.Error.NotConnected",
			wantOK:   true,
			contains: "reconnect",
		},
		{
			name:    "an unrecognized name is left to the caller",
			errName: "org.example.Whatever",
			wantOK:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			text, ok := friendlyGATTError(tc.errName, tc.message)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (text %q)", ok, tc.wantOK, text)
			}
			if !ok {
				if text != "" {
					t.Errorf("unrecognized name yielded text %q, want empty", text)
				}
				return
			}
			if !strings.Contains(text, tc.contains) {
				t.Errorf("text = %q, want it to contain %q", text, tc.contains)
			}
			if tc.omits != "" && strings.Contains(text, tc.omits) {
				t.Errorf("text = %q, want it NOT to contain %q", text, tc.omits)
			}
		})
	}
}
