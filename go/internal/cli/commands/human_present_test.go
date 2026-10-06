package commands

import "testing"

// stubHumanPresent forces humanPresentFn for the duration of the test.
func stubHumanPresent(t *testing.T, present bool) {
	t.Helper()
	prev := humanPresentFn
	humanPresentFn = func() bool { return present }
	t.Cleanup(func() { humanPresentFn = prev })
}
