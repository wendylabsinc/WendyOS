package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

// deviceMarkers are substrings that indicate a flash failure came from the
// target device or its file descriptor — the canonical errno strings the OS
// prints plus our own short-write guard. Text matching (not just errors.Is) is
// required because the elevated __bmap-write child's device errors reach the
// parent only as stderr text, and dd reports them as plain strings
// ("Permission denied").
var deviceMarkers = []string{
	"permission denied",         // EACCES
	"operation not permitted",   // EPERM
	"read-only file system",     // EROFS
	"no space left",             // ENOSPC
	"no such device or address", // ENXIO (Linux)
	"device not configured",     // ENXIO (macOS)
	"no such device",            // ENODEV
	"input/output error",        // EIO
	"short write",               // our own truncated-write guard
}

// isDeviceFlashFailure reports whether a fast-path (bmap/seekable) write failed
// because the target device rejected or lost the write — permissions, a
// read-only card lock, no space, the device vanished, an I/O error, a short
// write. The dd fallback writes to the same device and cannot succeed, so these
// failures skip it and fail fast with the real error (WDY-1841). Every other
// cause — a bmap checksum/size mismatch, or an unrecognized failure — still
// falls back, preserving the historical behavior.
func isDeviceFlashFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrPermission) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, m := range deviceMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// describeFlashOffset renders how far a write got before it failed, so the
// primary error tells the user (and us) where it stopped rather than only that
// it stopped. total is the fast path's total (mapped bytes for the seekable
// path, uncompressed size otherwise); written is the last progress value.
func describeFlashOffset(written, total int64) string {
	switch {
	case total > 0 && written > 0:
		pct := float64(written) / float64(total) * 100
		return fmt.Sprintf("at %.1f%% (%s / %s)", pct, tui.FormatBytes(written), tui.FormatBytes(total))
	case written > 0:
		return fmt.Sprintf("after %s", tui.FormatBytes(written))
	default:
		return "before any bytes were written"
	}
}

// framePrimaryFlashError wraps the fast-path (primary) write error with the
// path that failed and the offset it reached. The returned error keeps
// primaryErr in its chain (so errors.Is/As and isDeviceFlashFailure still see
// the original cause), and it is the error the fallback logic must never
// discard — dropping it is the WDY-1841 bug.
func framePrimaryFlashError(label string, written, total int64, primaryErr error) error {
	return fmt.Errorf("%s failed %s: %w", label, describeFlashOffset(written, total), primaryErr)
}

// combineFlashFailure builds the error returned when the fast path failed and
// the full-image dd fallback also failed: both errors, clearly labeled, and
// both preserved in the errors.Is chain (e.g. a context.Canceled from a
// cancelled fallback must stay matchable upstream).
func combineFlashFailure(primary, fallback error) error {
	return fmt.Errorf("%w; full-image fallback also failed: %w", primary, fallback)
}

// flashDeviceFailureHint is appended to a device-level flash failure. The dd
// fallback is skipped in this case, so point at the physical causes worth
// checking instead of leaving the user to wonder why nothing retried.
const flashDeviceFailureHint = "The target device rejected or lost the write, so retrying the full image would fail the same way. " +
	"Check the SD card's write-protect lock, the card's health, and the reader, cable, and USB port, " +
	"then re-list drives (`diskutil list` on macOS, `lsblk` on Linux) to confirm the device still appears."

// announceFastWriteFallback says why the fast write failed before the full
// write starts; once the fallback succeeds nothing else would report it.
func announceFastWriteFallback(w io.Writer, primary error) {
	fmt.Fprintln(w, tui.WarningMessage("Fast write failed; falling back to a full-image write, which takes longer:"))
	fmt.Fprintln(w, strings.TrimSpace(primary.Error()))
}
