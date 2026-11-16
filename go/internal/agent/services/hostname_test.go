package services

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidHostname(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"simple", "wendy", true},
		{"with prefix", "wendyos-living-room", true},
		{"single letter", "a", true},
		{"letter then digit", "a1", true},
		{"hyphens and digits", "x-1-y-2", true},
		{"max length 63", strings.Repeat("a", 63), true},

		{"empty", "", false},
		{"too long 64", strings.Repeat("a", 64), false},
		{"starts with digit", "1wendy", false},
		{"starts with hyphen", "-wendy", false},
		{"ends with hyphen", "wendy-", false},
		{"uppercase", "Wendy", false},
		{"underscore", "wendy_os", false},
		{"dot", "wendy.os", false},
		{"space", "wendy os", false},
		{"trailing newline", "wendy\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validHostname(tt.in); got != tt.want {
				t.Errorf("validHostname(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestDeviceName walks the sources the device's name can come from, in the
// order the mDNS advertisement's `name` record uses: a rename, then the name
// given at install, then the host's own name on a machine that has neither.
func TestDeviceName(t *testing.T) {
	const renameFile, installFile = "hostname", "device-name"
	hostname := func(name string) func() (string, error) {
		return func() (string, error) { return name, nil }
	}
	tests := []struct {
		name     string
		files    map[string]string // the name files present on the device
		hostname func() (string, error)
		want     string
	}{
		{
			name:     "name given at install",
			files:    map[string]string{installFile: "orin-nano-pr319\n"},
			hostname: hostname("wendyos-orin-nano-pr319"),
			want:     "orin-nano-pr319",
		},
		{
			name:     "a rename outranks the install-time name",
			files:    map[string]string{renameFile: "kitchen-pi\n", installFile: "orin-nano-pr319\n"},
			hostname: hostname("kitchen-pi"),
			want:     "kitchen-pi",
		},
		{
			name:     "neither file: the host's own name, as on plain Linux",
			hostname: hostname("lab-ubuntu"),
			want:     "lab-ubuntu",
		},
		{
			name:     "the host's name without the mDNS domain",
			hostname: hostname("Ethans-MacBook-Pro.local"),
			want:     "Ethans-MacBook-Pro",
		},
		{
			name:     "an unusable rename file falls through to the install-time name",
			files:    map[string]string{renameFile: "Not A Hostname\n", installFile: "orin-nano-pr319\n"},
			hostname: hostname("wendyos-orin-nano-pr319"),
			want:     "orin-nano-pr319",
		},
		{
			name:     "empty name files fall through to the host's name",
			files:    map[string]string{renameFile: "\n", installFile: "  \n"},
			hostname: hostname("wendyos-1a2b3c4d"),
			want:     "wendyos-1a2b3c4d",
		},
		{
			name:     "a name file carrying a second line is not a name",
			files:    map[string]string{installFile: "robot\nLD_PRELOAD=/tmp/evil.so\n"},
			hostname: hostname("wendyos-robot"),
			want:     "wendyos-robot",
		},
		{
			name:     "no name at all",
			hostname: func() (string, error) { return "", errors.New("no hostname") },
			want:     "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for file, contents := range tt.files {
				if err := os.WriteFile(filepath.Join(dir, file), []byte(contents), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got := deviceName(filepath.Join(dir, renameFile), filepath.Join(dir, installFile), tt.hostname)
			if got != tt.want {
				t.Errorf("deviceName() = %q, want %q", got, tt.want)
			}
		})
	}
}
