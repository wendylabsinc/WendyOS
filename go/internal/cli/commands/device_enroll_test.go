package commands

import (
	"regexp"
	"testing"

	"github.com/google/uuid"
)

// The device id is a v4 UUID (sem, WDY-2943). Cloud refuses any device id
// whose segments do not match ^[A-Za-z0-9._-]{1,64}$, and the id is irreversible
// once minted, so the shape is pinned here rather than assumed.
func TestDeviceIDIsALegalUUID(t *testing.T) {
	legalSegment := regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

	first := uuid.NewString()
	if !legalSegment.MatchString(first) {
		t.Errorf("device id %q is not a legal device id segment", first)
	}
	if _, err := uuid.Parse(first); err != nil {
		t.Errorf("device id %q does not parse as a UUID: %v", first, err)
	}
	if second := uuid.NewString(); second == first {
		t.Errorf("two enrollments minted the same device id %q", first)
	}
}

func TestACMEDirectoryURL(t *testing.T) {
	const tenant = "2558fd76-afc7-466e-9613-6b715296a526"

	if _, err := acmeDirectoryURL(tenant); err == nil {
		t.Errorf("acmeDirectoryURL with %s unset: want an error rather than a guessed host", acmeEndpointEnv)
	}

	t.Setenv(acmeEndpointEnv, "https://acme.pki.example")
	got, err := acmeDirectoryURL(tenant)
	if err != nil {
		t.Fatalf("acmeDirectoryURL: %v", err)
	}
	want := "https://acme.pki.example/" + tenant + "/acme/directory"
	if got != want {
		t.Errorf("acmeDirectoryURL = %q, want %q", got, want)
	}

	// A trailing slash must not double up: pki-core matches the directory path
	// exactly.
	t.Setenv(acmeEndpointEnv, "https://acme.pki.example/")
	if got, err = acmeDirectoryURL(tenant); err != nil || got != want {
		t.Errorf("acmeDirectoryURL with trailing slash = %q, %v; want %q", got, err, want)
	}

	t.Setenv(acmeEndpointEnv, "not-a-url")
	if _, err = acmeDirectoryURL(tenant); err == nil {
		t.Errorf("acmeDirectoryURL with a malformed endpoint: want an error")
	}
}

// The enrollment name is the tenant-scoped key devices are addressed by, and
// `wendy device rename` writes one string to both the cloud asset name and the
// mDNS hostname. So enroll has to accept exactly what rename accepts —
// including on the hostname-default path, which is where a non-label name
// would otherwise slip in without anyone typing it.
func TestResolveEnrollmentNameSharesRenamesRule(t *testing.T) {
	original := isInteractiveTerminalFn
	isInteractiveTerminalFn = func() bool { return false }
	t.Cleanup(func() { isInteractiveTerminalFn = original })

	tests := []struct {
		name    string
		host    string
		flag    string
		want    string
		wantErr bool
	}{
		{name: "explicit name", flag: "box-01", want: "box-01"},
		{name: "hostname default", host: "playful-reed.local", want: "playful-reed"},
		{name: "uppercase and spaces rejected", flag: "Fleet A Box 01", wantErr: true},
		{name: "trailing hyphen rejected", flag: "box-", wantErr: true},
		{name: "non-label hostname default rejected", host: "Wendy-Box.local", wantErr: true},
		{name: "bare IP with no name", host: "192.168.1.50", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveEnrollmentName(tt.host, tt.flag)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveEnrollmentName(%q, %q) = %q, want an error", tt.host, tt.flag, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveEnrollmentName(%q, %q): %v", tt.host, tt.flag, err)
			}
			if got != tt.want {
				t.Errorf("resolveEnrollmentName(%q, %q) = %q, want %q", tt.host, tt.flag, got, tt.want)
			}
			// One rule, not two: whatever enroll returns, rename must take.
			if err := validateHostnameArg(got); err != nil {
				t.Errorf("enroll accepted %q but rename rejects it: %v", got, err)
			}
		})
	}
}
