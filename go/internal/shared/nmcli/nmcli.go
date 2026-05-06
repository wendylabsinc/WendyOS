// Package nmcli centralises invocation and output parsing of the `nmcli` CLI.
//
// The two responsibilities it owns:
//
//  1. Build *exec.Cmd values that force `LC_ALL=C.UTF-8`. The agent runs under
//     systemd with no inherited locale, and on some nmcli builds an unset or C
//     locale causes SSID bytes outside printable ASCII (notably emoji) to be
//     rendered as literal `?` characters via iconv before they reach stdout.
//     Once that conversion happens the SSID can never round-trip back to a
//     scan match. Forcing C.UTF-8 keeps output byte-clean.
//
//  2. Parse the colon-delimited terse format. nmcli `-t` escapes embedded `:`
//     as `\:` and literal `\` as `\\`; older builds also escape `\n`, `\r`,
//     `\t`. UTF-8 multi-byte sequences never include `:` (0x3A) or `\` (0x5C),
//     so byte-wise scanning is safe for emoji as long as the escape rules
//     above are honoured.
package nmcli

import (
	"context"
	"os"
	"os/exec"
	"strings"
)

// LocaleEnv is the locale forced on every nmcli invocation. C.UTF-8 is part of
// glibc since 2.13 and present in musl-based images; on the rare host that
// lacks it nmcli falls back to its built-in default, which is also UTF-8 in
// terse mode.
const LocaleEnv = "LC_ALL=C.UTF-8"

// Command returns an *exec.Cmd that runs `nmcliPath` with the given args under
// a UTF-8 locale. The parent process's environment is preserved except for
// LC_ALL, which is overwritten.
func Command(ctx context.Context, nmcliPath string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, nmcliPath, args...)
	cmd.Env = withUTF8Locale(os.Environ())
	return cmd
}

// withUTF8Locale returns env with LC_ALL set/overridden to C.UTF-8.
func withUTF8Locale(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if strings.HasPrefix(e, "LC_ALL=") {
			continue
		}
		out = append(out, e)
	}
	return append(out, LocaleEnv)
}

// Split splits a single record from `nmcli -t` output into `fields` substrings,
// undoing the backslash escaping that nmcli applies (`\:`, `\\`, and the less
// common `\n`, `\r`, `\t`). The split honours escaping, so an SSID containing
// `\:` is kept intact instead of being broken in two.
func Split(line string, fields int) []string {
	out := make([]string, 0, fields)
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '\\' && i+1 < len(line) {
			cur.WriteByte(unescapeByte(line[i+1]))
			i++
			continue
		}
		if c == ':' && len(out) < fields-1 {
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	out = append(out, cur.String())
	return out
}

// Unescape reverses nmcli's terse-mode escaping for a value that has already
// been split into a single field (e.g. output from `nmcli -g <prop>`).
func Unescape(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			b.WriteByte(unescapeByte(s[i+1]))
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func unescapeByte(c byte) byte {
	switch c {
	case 'n':
		return '\n'
	case 'r':
		return '\r'
	case 't':
		return '\t'
	}
	return c
}
