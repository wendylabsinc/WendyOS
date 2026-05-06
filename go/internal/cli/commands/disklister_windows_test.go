//go:build windows

package commands

import "testing"

func TestParseDiskNumber(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    int
		wantErr bool
	}{
		{name: "single digit", input: `\\.\PhysicalDrive1`, want: 1},
		{name: "multi digit", input: `\\.\PhysicalDrive42`, want: 42},
		{name: "zero", input: `\\.\PhysicalDrive0`, want: 0},
		{name: "trailing junk rejected", input: `\\.\PhysicalDrive1abc`, wantErr: true},
		{name: "trailing space rejected", input: `\\.\PhysicalDrive1 `, wantErr: true},
		{name: "missing prefix", input: `PhysicalDrive1`, wantErr: true},
		{name: "wrong prefix case", input: `\\.\physicaldrive1`, wantErr: true},
		{name: "empty", input: ``, wantErr: true},
		{name: "no digits", input: `\\.\PhysicalDrive`, wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseDiskNumber(c.input)
			if c.wantErr {
				if err == nil {
					t.Fatalf("parseDiskNumber(%q) = %d, want error", c.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDiskNumber(%q) error: %v", c.input, err)
			}
			if got != c.want {
				t.Fatalf("parseDiskNumber(%q) = %d, want %d", c.input, got, c.want)
			}
		})
	}
}

func TestResolvePowershellExe(t *testing.T) {
	got := resolvePowershellExe()
	if got == "" {
		t.Fatal("resolvePowershellExe() returned empty string")
	}
	// The fallback "powershell" or an absolute path are both valid; we just
	// require a non-empty string so misconfigured callers can't end up
	// invoking exec.Command("") and getting a confusing error.
}
