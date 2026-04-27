package env

import "testing"

func TestIsCI_NoEnvVars(t *testing.T) {
	for _, key := range ciEnvVars {
		t.Setenv(key, "")
	}
	if IsCI() {
		t.Error("IsCI should be false when no CI env vars are set")
	}
}

func TestIsCI_DetectsEachKnownVar(t *testing.T) {
	for _, key := range ciEnvVars {
		t.Run(key, func(t *testing.T) {
			for _, other := range ciEnvVars {
				t.Setenv(other, "")
			}
			t.Setenv(key, "1")
			if !IsCI() {
				t.Errorf("IsCI should be true when %s is set", key)
			}
		})
	}
}

func TestIsCI_IgnoresWhitespaceOnlyValues(t *testing.T) {
	for _, key := range ciEnvVars {
		t.Setenv(key, "")
	}
	t.Setenv("CI", "   ")
	if IsCI() {
		t.Error("IsCI should be false for whitespace-only CI value")
	}
}
