package commands

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func TestSelectExtension(t *testing.T) {
	exts := []extensionEntry{
		{Name: "wendyos-hello", KernelVersion: "6.12.87-v8-16k", Path: "images/x/0.17.0/wendyos-hello.raw"},
		{Name: "acme-npu", KernelVersion: "6.12.87-v8-16k", Path: "p1"},
		{Name: "acme-npu", KernelVersion: "6.6.0-other", Path: "p2"}, // same name, different kernel
		{Name: "no-kernel", KernelVersion: "", Path: "p3"},           // agent refuses these
	}
	dev := "6.12.87-v8-16k"

	tests := []struct {
		name, kernel string
		wantOK       bool
		wantPath     string
	}{
		{"wendyos-hello", dev, true, "images/x/0.17.0/wendyos-hello.raw"},
		{"acme-npu", dev, true, "p1"},                                    // picks the entry for THIS kernel
		{"acme-npu", "6.6.0-other", true, "p2"},                          // picks the other-kernel entry
		{"acme-npu", "9.9.9-nope", false, ""},                            // no entry for this kernel
		{"no-kernel", dev, false, ""},                                    // agent refuses an undeclared kernel
		{"missing", dev, false, ""},                                      // unknown name
		{"wendyos-hello", "", true, "images/x/0.17.0/wendyos-hello.raw"}, // empty kernel disables filter
	}
	for _, tt := range tests {
		got, ok := selectExtension(exts, tt.name, tt.kernel)
		if ok != tt.wantOK {
			t.Errorf("selectExtension(%q, %q) ok=%v, want %v", tt.name, tt.kernel, ok, tt.wantOK)
			continue
		}
		if ok && got.Path != tt.wantPath {
			t.Errorf("selectExtension(%q, %q) path=%q, want %q", tt.name, tt.kernel, got.Path, tt.wantPath)
		}
	}
}

func TestDriverStale(t *testing.T) {
	tests := []struct {
		desc       string
		addon      string
		running    string
		unreadable bool
		want       bool
	}{
		{"kernel matches", "6.12.87-v8-16k", "6.12.87-v8-16k", false, false},
		// The case an OTA creates: the image stays on /data but no longer merges.
		{"kernel differs", "6.12.87-v8-16k", "6.18.33-v8-16k", false, true},
		// No modules, so no kernel pinned: a udev/firmware add-on survives any OTA.
		{"add-on pins no kernel", "", "6.18.33-v8-16k", false, false},
		// Nothing to compare against: never guess that a driver is broken.
		{"device kernel unknown", "6.12.87-v8-16k", "", false, false},
		// The kernel is unknown because the image is corrupt, not because it pins
		// none - the one add-on that certainly will not load.
		{"image unreadable", "", "6.18.33-v8-16k", true, true},
	}
	for _, tt := range tests {
		d := &agentpbv2.InstalledDriver{Name: "x", KernelVersion: tt.addon, Unreadable: tt.unreadable}
		if got := driverStale(d, tt.running); got != tt.want {
			t.Errorf("%s: driverStale(%q, %q) = %v, want %v", tt.desc, tt.addon, tt.running, got, tt.want)
		}
	}
}

func TestDriverInstalledFor(t *testing.T) {
	installed := []*agentpbv2.InstalledDriver{
		{Name: "wendyos-hello", KernelVersion: "6.12.87-v8-16k"},
		{Name: "udev-rules", KernelVersion: ""},
		{Name: "corrupt", KernelVersion: "", Unreadable: true},
	}
	tests := []struct {
		desc string
		e    extensionEntry
		want bool
	}{
		{"same name and kernel", extensionEntry{Name: "wendyos-hello", KernelVersion: "6.12.87-v8-16k"}, true},
		// After an OTA the stale copy shares the name, hiding the needed rebuild.
		{"same name, rebuilt for a newer kernel", extensionEntry{Name: "wendyos-hello", KernelVersion: "6.18.33-v8-16k"}, false},
		{"an add-on pinning no kernel stays installed", extensionEntry{Name: "udev-rules", KernelVersion: "6.18.33-v8-16k"}, true},
		{"not installed at all", extensionEntry{Name: "acme-npu", KernelVersion: "6.12.87-v8-16k"}, false},
		// A corrupt copy shares the name but will not load, so the rebuild has to
		// stay on offer.
		{"installed copy is unreadable", extensionEntry{Name: "corrupt", KernelVersion: "6.12.87-v8-16k"}, false},
	}
	for _, tt := range tests {
		if got := driverInstalledFor(installed, tt.e); got != tt.want {
			t.Errorf("%s: driverInstalledFor(%+v) = %v, want %v", tt.desc, tt.e, got, tt.want)
		}
	}
}

// An unenrolled device answers Unimplemented because the driver service is
// mTLS-only, so the CLI's generic "update the agent" hint would misdirect.
func TestDriverServiceErr(t *testing.T) {
	got := driverServiceErr(status.Error(codes.Unimplemented, "unknown service"))
	for _, want := range []string{"enrolled device", "wendy device enroll"} {
		if !strings.Contains(got.Error(), want) {
			t.Errorf("message %q missing %q", got, want)
		}
	}
	// Every other failure has to reach the user unchanged.
	orig := status.Error(codes.Unavailable, "device is offline")
	if driverServiceErr(orig) != orig {
		t.Errorf("driverServiceErr rewrote a non-Unimplemented error")
	}
	if driverServiceErr(nil) != nil {
		t.Error("driverServiceErr(nil) should stay nil")
	}
}
