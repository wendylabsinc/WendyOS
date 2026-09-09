package bluez

import "testing"

func TestFriendlyError(t *testing.T) {
	tests := []struct {
		name     string
		errName  string
		message  string
		wantOK   bool
		notFound bool
		contains string
	}{
		{
			name:     "unknown object means rescan",
			errName:  "org.freedesktop.DBus.Error.UnknownObject",
			wantOK:   true,
			notFound: true,
			contains: "rescan",
		},
		{
			name:     "DoesNotExist also means rescan",
			errName:  "org.bluez.Error.DoesNotExist",
			wantOK:   true,
			notFound: true,
			contains: "rescan",
		},
		{
			name:     "Failed defers to the bearer reason",
			errName:  "org.bluez.Error.Failed",
			message:  "le-connection-timeout",
			wantOK:   true,
			contains: "did not respond",
		},
		{
			name:     "access denied names the group",
			errName:  "org.freedesktop.DBus.Error.AccessDenied",
			wantOK:   true,
			contains: "bluetooth` group",
		},
		{
			name:     "not permitted suggests pairing",
			errName:  "org.bluez.Error.NotPermitted",
			wantOK:   true,
			contains: "bluetoothctl",
		},
		{
			name:     "not authorized suggests pairing",
			errName:  "org.bluez.Error.NotAuthorized",
			wantOK:   true,
			contains: "bluetoothctl",
		},
		{
			name:     "not supported blames the characteristic",
			errName:  "org.bluez.Error.NotSupported",
			wantOK:   true,
			contains: "characteristic",
		},
		{
			name:     "not connected suggests reconnecting",
			errName:  "org.bluez.Error.NotConnected",
			wantOK:   true,
			contains: "reconnect",
		},
		{
			name:     "service unknown names bluetoothd",
			errName:  "org.freedesktop.DBus.Error.ServiceUnknown",
			wantOK:   true,
			contains: "bluetoothd",
		},
		{
			name:    "an unrecognized name is left to the caller",
			errName: "org.example.Whatever",
			wantOK:  false,
		},
		{
			name:    "an empty name is left to the caller",
			errName: "",
			wantOK:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			text, notFound, ok := FriendlyError(tc.errName, tc.message)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (text %q)", ok, tc.wantOK, text)
			}
			if !ok {
				if text != "" {
					t.Errorf("unrecognized name yielded text %q, want empty", text)
				}
				return
			}
			if notFound != tc.notFound {
				t.Errorf("notFound = %v, want %v", notFound, tc.notFound)
			}
			if !contains(text, tc.contains) {
				t.Errorf("text = %q, want it to contain %q", text, tc.contains)
			}
		})
	}
}

func TestIsTransientError(t *testing.T) {
	tests := []struct {
		errName string
		message string
		want    bool
	}{
		{"org.bluez.Error.InProgress", "", true},
		{"org.freedesktop.DBus.Error.NoReply", "", true},
		{"org.bluez.Error.Failed", "le-connection-unknown", true},
		{"org.bluez.Error.Failed", "br-connection-busy", true},
		{"org.bluez.Error.Failed", "le-connection-abort-by-local", true},
		// A refused or timed-out connection reflects a real condition; a bare
		// retry will not fix it.
		{"org.bluez.Error.Failed", "le-connection-refused", false},
		{"org.bluez.Error.Failed", "le-connection-timeout", false},
		{"org.bluez.Error.AuthenticationFailed", "", false},
		{"org.freedesktop.DBus.Error.AccessDenied", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		if got := IsTransientError(tc.errName, tc.message); got != tc.want {
			t.Errorf("IsTransientError(%q, %q) = %v, want %v", tc.errName, tc.message, got, tc.want)
		}
	}
}

func TestFriendlyBearerFailure(t *testing.T) {
	tests := []struct {
		message  string
		contains string
	}{
		{"le-connection-timeout", "did not respond"},
		{"br-connection-page-timeout", "did not respond"},
		{"le-connection-refused", "refused the connection"},
		{"le-connection-adapter-not-powered", "powered off"},
		{"br-connection-key-missing", "stale"},
		{"le-connection-abort-by-local", "canceled"},
		// An unclassified bearer reason still gets a hint, with the raw reason
		// embedded so a bug report carries it.
		{"le-connection-something-new", "le-connection-something-new"},
		// Older BlueZ puts plain strerror text here, which passes through.
		{"Input/output error", "Input/output error"},
	}
	for _, tc := range tests {
		if got := FriendlyBearerFailure(tc.message); !contains(got, tc.contains) {
			t.Errorf("FriendlyBearerFailure(%q) = %q, want it to contain %q", tc.message, got, tc.contains)
		}
	}
}

func contains(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
