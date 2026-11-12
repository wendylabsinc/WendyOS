package devicename

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name, want string
	}{
		{"nano-lab", ""},
		{"abc", ""},
		{"a1-2", ""},
		{strings.Repeat("a", MaxLen), ""},
		{"ab", "must be 3–55 characters"},
		{strings.Repeat("a", MaxLen+1), "must be 3–55 characters"},
		{"1abc", "must start with a lowercase letter"},
		{"-abc", "must start with a lowercase letter"},
		{"Nano", "may only contain lowercase letters, digits, and hyphens"},
		{"nano lab", "may only contain lowercase letters, digits, and hyphens"},
		{"nano\nlab", "may only contain lowercase letters, digits, and hyphens"},
		{"nano-", "must not end with a hyphen"},
	} {
		err := Validate(tc.name)
		if tc.want == "" {
			if err != nil {
				t.Errorf("Validate(%q) = %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Validate(%q) = %v, want %q", tc.name, err, tc.want)
		}
	}
}
