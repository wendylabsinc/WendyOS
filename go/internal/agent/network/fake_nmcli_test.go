package network

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reply is one canned nmcli answer; args is matched against "$*", as a prefix
// when prefix is set.
type reply struct {
	args   string
	prefix bool
	out    string
	code   int
}

// writeFakeNMCLI writes an nmcli stand-in that answers with the first matching
// reply and logs every call, one per line, to the returned log file.
func writeFakeNMCLI(t *testing.T, replies ...reply) (path, callLog string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "nmcli")
	callLog = filepath.Join(dir, "calls.log")

	var b strings.Builder
	b.WriteString("#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shQuote(callLog) + "\ncase \"$*\" in\n")
	for _, r := range replies {
		pattern := shQuote(r.args)
		if r.prefix {
			pattern += "*"
		}
		fmt.Fprintf(&b, "%s)\n\tprintf '%%s' %s\n\texit %d ;;\n", pattern, shQuote(r.out), r.code)
	}
	b.WriteString("esac\necho \"unexpected nmcli call: $*\" >&2\nexit 1\n")

	if err := os.WriteFile(path, []byte(b.String()), 0o700); err != nil {
		t.Fatal(err)
	}
	return path, callLog
}

// writeNMCLIScript writes an nmcli stand-in from shell case arms, which can use
// $d for its directory; it logs calls like writeFakeNMCLI.
func writeNMCLIScript(t *testing.T, arms string) (path, callLog string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "nmcli")
	callLog = filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\nd=$(dirname \"$0\")\nprintf '%s\\n' \"$*\" >> " + shQuote(callLog) +
		"\ncase \"$*\" in\n" + arms + "esac\necho \"unexpected nmcli call: $*\" >&2\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path, callLog
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// readCalls returns the logged nmcli invocations.
func readCalls(t *testing.T, callLog string) []string {
	t.Helper()
	b, err := os.ReadFile(callLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// profileArgs is the argument string readWiFiProfile passes for uuid.
func profileArgs(uuid string) string {
	return "--escape no -t -f " + profileFields + " connection show " + uuid
}

// batchArgs is the argument string readWiFiProfiles passes for uuids.
func batchArgs(uuids ...string) string {
	return "--escape no -t -f " + uuidField + "," + profileFields + " connection show " + strings.Join(uuids, " ")
}

// batchRecord renders one record of the batched read, as profileOut does.
func batchRecord(uuid, keyMgmt, authAlg, ssid string) string {
	return uuidField + ":" + uuid + "\n" + profileOut(keyMgmt, authAlg, ssid)
}

// batchOut joins records the way nmcli does, with a blank line between them.
func batchOut(records ...string) string {
	return strings.Join(records, "\n")
}

// profileOut renders nmcli's output for one profile bound to no device; an
// empty keyMgmt means the profile has no security setting.
func profileOut(keyMgmt, authAlg, ssid string) string {
	return boundProfileOut("", keyMgmt, authAlg, ssid)
}

// boundProfileOut is profileOut for a profile bound to the device iface.
func boundProfileOut(iface, keyMgmt, authAlg, ssid string) string {
	out := ifaceField + ":" + iface + "\n" + modeField + ":infrastructure\n"
	if keyMgmt != "" {
		out += keyMgmtField + ":" + keyMgmt + "\n" + authAlgField + ":" + authAlg + "\n"
	}
	return out + ssidField + ":" + ssid + "\n"
}
