package commands

import (
	"testing"
)

func TestParseTunnelArg(t *testing.T) {
	tests := []struct {
		arg        string
		wantLocal  uint32
		wantRemote uint32
		wantErr    bool
	}{
		{"8080", 8080, 8080, false},
		{"3000:8080", 3000, 8080, false},
		{"0", 0, 0, true},
		{"99999", 0, 0, true},
		{"abc", 0, 0, true},
		{"8080:abc", 0, 0, true},
		{"65535", 65535, 65535, false},
		{"1:65535", 1, 65535, false},
	}

	for _, tt := range tests {
		t.Run(tt.arg, func(t *testing.T) {
			local, remote, err := parseTunnelArg(tt.arg)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseTunnelArg(%q) expected error, got none", tt.arg)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTunnelArg(%q) unexpected error: %v", tt.arg, err)
			}
			if local != tt.wantLocal || remote != tt.wantRemote {
				t.Errorf("parseTunnelArg(%q) = (%d, %d), want (%d, %d)", tt.arg, local, remote, tt.wantLocal, tt.wantRemote)
			}
		})
	}
}
