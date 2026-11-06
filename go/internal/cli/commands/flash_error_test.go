package commands

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestIsDeviceFlashFailure_Table pins which fast-path failures skip the futile
// dd fallback (device-level → true) versus which keep falling back (bmap
// integrity / unknown → false). The device cases include the real WDY-1841
// incident string and the canonical errno text the elevated __bmap-write child
// relays over stderr.
func TestIsDeviceFlashFailure_Table(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		// Not device-level — the silent fallback exists to recover integrity
		// failures, and unknown causes fall back too (historical behavior).
		{"nil", nil, false},
		{"checksum mismatch text", errors.New("bmap: checksum mismatch for blocks 0-1 (got a, want b)"), false},
		{"seekable size mismatch", errors.New("seekable image size 100 != bmap image size 200"), false},
		{"bmap parse failure", errors.New("parsing bmap: XML syntax error"), false},
		{"opaque exit status", errors.New("writing image: exit status 1"), false},
		{"unrelated error", errors.New("some other failure"), false},

		// Device-level failures — the fallback writes to the same broken device.
		{"real incident (dd permission denied)", errors.New("writing image: exit status 1\ndd: /dev/rdisk12: Permission denied"), true},
		{"os.ErrPermission wrapped", fmt.Errorf("opening device /dev/rdisk12: %w", os.ErrPermission), true},
		{"operation not permitted", errors.New("opening device: operation not permitted"), true},
		{"read-only file system", errors.New("writing at 4096: read-only file system"), true},
		{"no space left", errors.New("writing at 4096: no space left on device"), true},
		{"device not configured (darwin ENXIO)", errors.New("writing at 4096: device not configured"), true},
		{"no such device or address (linux ENXIO)", errors.New("writing at 4096: no such device or address"), true},
		{"input/output error", errors.New("writing at 8192: input/output error"), true},
		{"short write guard", errors.New("short write at offset 512: wrote 256 of 512 bytes"), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDeviceFlashFailure(tt.err); got != tt.want {
				t.Fatalf("isDeviceFlashFailure(%q) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestDescribeFlashOffset covers the three offset-rendering modes: known total
// (percentage + bytes), unknown total (bytes only), and nothing written.
func TestDescribeFlashOffset(t *testing.T) {
	tests := []struct {
		name            string
		written, total  int64
		wantContains    []string
		wantNotContains []string
	}{
		{"known total", 16_300_000_000, 16_400_000_000, []string{"99.", "%", "GiB /"}, nil},
		{"unknown total", 500 << 20, 0, []string{"after", "MiB"}, []string{"%"}},
		{"nothing written", 0, 16_400_000_000, []string{"before any bytes"}, []string{"%"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := describeFlashOffset(tt.written, tt.total)
			for _, sub := range tt.wantContains {
				if !strings.Contains(got, sub) {
					t.Errorf("describeFlashOffset(%d,%d) = %q; want substring %q", tt.written, tt.total, got, sub)
				}
			}
			for _, sub := range tt.wantNotContains {
				if strings.Contains(got, sub) {
					t.Errorf("describeFlashOffset(%d,%d) = %q; must not contain %q", tt.written, tt.total, got, sub)
				}
			}
		})
	}
}

// TestFramePrimaryFlashError_PreservesCauseAndOffset asserts the primary error
// keeps the original cause in its chain (errors.Is) and names the path + offset.
func TestFramePrimaryFlashError_PreservesCauseAndOffset(t *testing.T) {
	cause := errors.New("dd: /dev/rdisk12: Permission denied")
	framed := framePrimaryFlashError("seekable block-map write", 16_300_000_000, 16_400_000_000, cause)

	if !errors.Is(framed, cause) {
		t.Fatalf("framed error dropped the primary cause: %v", framed)
	}
	msg := framed.Error()
	for _, sub := range []string{"seekable block-map write failed", "99.", "%", "Permission denied"} {
		if !strings.Contains(msg, sub) {
			t.Errorf("framed error %q missing substring %q", msg, sub)
		}
	}
	// The framed error must still read as device-level so the caller skips the
	// fallback — framing must not mask the underlying cause.
	if !isDeviceFlashFailure(framed) {
		t.Errorf("framed device error no longer classifies as device-level: %v", framed)
	}
}

// TestCombineFlashFailure_BothErrorsPresent is the core WDY-1841 regression: the
// combined error must contain BOTH the primary (real) error and the fallback's,
// clearly labeled, with the primary preserved in the errors.Is chain.
func TestCombineFlashFailure_BothErrorsPresent(t *testing.T) {
	primaryCause := errors.New("seekable write: input/output error")
	primary := framePrimaryFlashError("seekable block-map write", 16_300_000_000, 16_400_000_000, primaryCause)
	fallback := errors.New("writing image: exit status 1\ndd: /dev/rdisk12: Permission denied")

	combined := combineFlashFailure(primary, fallback)
	msg := combined.Error()

	if !errors.Is(combined, primaryCause) {
		t.Fatalf("combined error dropped the primary cause: %v", combined)
	}
	// The fallback must stay in the chain too — a context.Canceled from a
	// cancelled fallback has to remain errors.Is-matchable upstream.
	if !errors.Is(combined, fallback) {
		t.Fatalf("combined error dropped the fallback cause: %v", combined)
	}
	// Primary (real failure) present, with offset.
	if !strings.Contains(msg, "seekable block-map write failed") || !strings.Contains(msg, "input/output error") {
		t.Errorf("combined error %q missing the primary failure", msg)
	}
	// Fallback present and clearly labeled as the fallback.
	if !strings.Contains(msg, "full-image fallback also failed") || !strings.Contains(msg, "Permission denied") {
		t.Errorf("combined error %q missing the labeled fallback failure", msg)
	}
	// Offset survives into the final message.
	if !strings.Contains(msg, "%") {
		t.Errorf("combined error %q lost the failure offset", msg)
	}
}

// A fallback that succeeds must not hide why the fast write failed: the
// warning carries the framed error, helper output included.
func TestAnnounceFastWriteFallback_ShowsTheReason(t *testing.T) {
	// Helper stderr ends with a newline, as the bmap writers capture it.
	cause := errors.New("bmap write failed: exit status 1\n✗ opening seekable image: won't read /c/image.img.zst: owned by uid 0, not by uid 1000 who ran sudo\n")
	primary := framePrimaryFlashError("seekable block-map write", 0, 1_900_000_000, cause)

	var out bytes.Buffer
	announceFastWriteFallback(&out, primary)
	for _, sub := range []string{"seekable block-map write failed", "owned by uid 0", "full-image write"} {
		if !strings.Contains(out.String(), sub) {
			t.Errorf("fallback warning %q missing %q", out.String(), sub)
		}
	}
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			t.Errorf("fallback warning %q has a blank line", out.String())
		}
	}
}
