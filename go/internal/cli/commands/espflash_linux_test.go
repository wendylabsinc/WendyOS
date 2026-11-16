//go:build linux

package commands

import (
	"errors"
	"strings"
	"testing"
)

func TestPermissionMessageNamesTheTrueCase(t *testing.T) {
	cause := errors.New("open failed")
	for _, tc := range []struct {
		name           string
		active, listed bool
		want           []string
	}{
		{"listed but session predates it", false, true, []string{"started before that", "newgrp uucp"}},
		{"not in the group", false, false, []string{"not in group 'uucp'", "sudo usermod -aG uucp sem"}},
		{"group active", true, true, []string{"although this session has group 'uucp'", "open failed"}},
	} {
		err := permissionMessage("/dev/ttyACM0", "uucp", "sem", tc.active, tc.listed, cause)
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: %q lacks %q", tc.name, err, w)
			}
		}
	}
}
