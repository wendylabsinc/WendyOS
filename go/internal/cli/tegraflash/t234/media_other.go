//go:build darwin || windows

package t234

// macOS and Windows poll removable media themselves and eject through the OS,
// so there is nothing to enable or check.

func mediaPollingMissing(string) bool { return false }

func enableMediaPolling(string) error { return nil }

func CheckHostTools() error { return nil }
