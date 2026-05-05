package nmcli

import (
	"strings"
	"testing"
)

func TestSplit(t *testing.T) {
	cases := []struct {
		name   string
		line   string
		fields int
		want   []string
	}{
		{
			name:   "ascii four-field",
			line:   " :HomeNet:70:WPA2",
			fields: 4,
			want:   []string{" ", "HomeNet", "70", "WPA2"},
		},
		{
			name:   "emoji passes through unmodified",
			line:   " :Read Only Internet \xf0\x9f\xab\xa5:70:WPA2",
			fields: 4,
			want:   []string{" ", "Read Only Internet \xf0\x9f\xab\xa5", "70", "WPA2"},
		},
		{
			name:   "literal colon escaped",
			line:   "*:cafe\\:1:80:WPA2",
			fields: 4,
			want:   []string{"*", "cafe:1", "80", "WPA2"},
		},
		{
			name:   "literal backslash escaped",
			line:   " :path\\\\name:60:WPA2",
			fields: 4,
			want:   []string{" ", "path\\name", "60", "WPA2"},
		},
		{
			name:   "newline escape decoded",
			line:   " :two\\nlines:50:WPA2",
			fields: 4,
			want:   []string{" ", "two\nlines", "50", "WPA2"},
		},
		{
			name:   "trailing backslash kept literal",
			line:   " :endsbackslash\\",
			fields: 2,
			want:   []string{" ", "endsbackslash\\"},
		},
		{
			name:   "fewer separators than fields",
			line:   "wifi:connected",
			fields: 3,
			want:   []string{"wifi", "connected"},
		},
		{
			name:   "emoji and escaped colon together",
			line:   " :evil\\:wifi \xf0\x9f\x98\x88:90:WPA3",
			fields: 4,
			want:   []string{" ", "evil:wifi \xf0\x9f\x98\x88", "90", "WPA3"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Split(tc.line, tc.fields)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d fields %q, want %d %q", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("field[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestUnescape(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"plain", "plain"},
		{"with\\:colon", "with:colon"},
		{"back\\\\slash", "back\\slash"},
		{"new\\nline", "new\nline"},
		{"tab\\there", "tab\there"},
		// Emoji must round-trip verbatim — its bytes (0xF0..0xF4 followed by
		// 0x80-0xBF continuation bytes) never overlap with `\` (0x5C).
		{"emoji \xf0\x9f\xab\xa5", "emoji \xf0\x9f\xab\xa5"},
	}
	for _, tc := range cases {
		got := Unescape(tc.in)
		if got != tc.want {
			t.Errorf("Unescape(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestWithUTF8Locale(t *testing.T) {
	in := []string{"PATH=/bin", "LC_ALL=POSIX", "FOO=bar"}
	out := withUTF8Locale(in)

	var locales []string
	hasFoo, hasPath := false, false
	for _, e := range out {
		if strings.HasPrefix(e, "LC_ALL=") {
			locales = append(locales, e)
		}
		if e == "FOO=bar" {
			hasFoo = true
		}
		if e == "PATH=/bin" {
			hasPath = true
		}
	}

	if len(locales) != 1 {
		t.Errorf("expected exactly one LC_ALL entry, got %d: %v", len(locales), locales)
	}
	if locales[0] != "LC_ALL=C.UTF-8" {
		t.Errorf("LC_ALL = %q, want LC_ALL=C.UTF-8", locales[0])
	}
	if !hasFoo || !hasPath {
		t.Errorf("non-locale env vars dropped: out=%v", out)
	}
}
