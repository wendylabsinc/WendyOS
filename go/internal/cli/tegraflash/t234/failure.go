//go:build darwin || linux || windows

package t234

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// DeviceFailure is what the device-side logs say about a flash the device
// reported as failed.
type DeviceFailure struct {
	// MissingStorage is the block device the initrd waited for and never saw,
	// such as "/dev/nvme0n1". It is empty when the logs do not show that.
	MissingStorage string
	// BootFirmwareStarted reports that the initrd began rewriting the boot
	// firmware (QSPI). Its command sequence starts that first and in the
	// background, and the initrd reports no status before it has ended.
	BootFirmwareStarted bool
	// Evidence is the last line of each log that reads as a failure or, when
	// none does, the last line of each log, as "<log>: <line>". It holds at
	// most maxFailureEvidence lines.
	Evidence []string
}

const (
	bootloaderLog       = "bootloader.log"
	maxFailureEvidence  = 3
	maxFailureLineRunes = 160
)

// missingStorageLine matches the timeout of wait_for_storage in init-flash.sh.
// It prints "Waiting for <device>..." once, a dot for each further second and
// "[FAIL]" after 15 tries, all on one line of export-<device>.log:
//
//	Waiting for /dev/nvme0n1.................[FAIL]
//
// Only the device path and a FAIL after it on the same line are required, so
// the spacing, the dots and the brackets may differ. A timeout worded as such
// counts too.
var missingStorageLine = regexp.MustCompile(`(?i)waiting for\s+(/dev/[a-z0-9_/-]+)[\s.…:]*\[?\s*(fail|timed out|timeout)`)

// bootFirmwareWriteLine matches program-boot-device announcing the erase of
// the boot device or a partition write, which it does before each one.
var bootFirmwareWriteLine = regexp.MustCompile(`^(Erasing|Writing) `)

var failureLine = regexp.MustCompile(`(?i)\b(fail(ed|ure)?|err(or)?|unrecognized|cannot|can't|could not|unable|not found|no such|timed out|timeout|denied|invalid|does not exist|not permitted)\b`)

// DiagnoseDeviceLogs reads the logs the device handed back with its status
// (FinalStatus.Logs) for what went wrong.
func DiagnoseDeviceLogs(logs map[string][]byte) DeviceFailure {
	names := make([]string, 0, len(logs))
	for name := range logs {
		names = append(names, name)
	}
	// The boot-firmware log is long and rarely where a flash stops: read it last.
	sort.Slice(names, func(i, j int) bool {
		if (names[i] == bootloaderLog) != (names[j] == bootloaderLog) {
			return names[j] == bootloaderLog
		}
		return names[i] < names[j]
	})
	var f DeviceFailure
	var failing, last []string
	for _, name := range names {
		lines := deviceLogLines(logs[name])
		failed := ""
		for _, line := range lines {
			if m := missingStorageLine.FindStringSubmatch(line); m != nil && f.MissingStorage == "" {
				f.MissingStorage = m[1]
			}
			if name == bootloaderLog && bootFirmwareWriteLine.MatchString(line) {
				f.BootFirmwareStarted = true
			}
			if failureLine.MatchString(line) {
				failed = line
			}
		}
		if failed != "" {
			failing = append(failing, name+": "+failed)
		}
		if len(lines) > 0 {
			last = append(last, name+": "+lines[len(lines)-1])
		}
	}
	f.Evidence = failing
	if len(f.Evidence) == 0 {
		f.Evidence = last
	}
	if len(f.Evidence) > maxFailureEvidence {
		f.Evidence = f.Evidence[:maxFailureEvidence]
	}
	return f
}

// deviceLogLines splits a device log into the lines worth showing a person:
// trimmed, free of control characters, cut to maxFailureLineRunes, and without
// blank lines, progress dots or the note of an extra script the image lacks.
// A carriage return ends a line too, as in the erase progress.
func deviceLogLines(log []byte) []string {
	var lines []string
	for _, raw := range strings.FieldsFunc(string(log), func(r rune) bool { return r == '\n' || r == '\r' }) {
		line := strings.TrimSpace(strings.Map(func(r rune) rune {
			if r == '\t' {
				return ' '
			}
			if unicode.IsControl(r) || r == unicode.ReplacementChar {
				return -1
			}
			return r
		}, raw))
		if !strings.ContainsFunc(line, unicode.IsLetter) || strings.HasSuffix(line, "was found, ignoring") {
			continue
		}
		if runes := []rune(line); len(runes) > maxFailureLineRunes {
			line = string(runes[:maxFailureLineRunes]) + "…"
		}
		lines = append(lines, line)
	}
	return lines
}
