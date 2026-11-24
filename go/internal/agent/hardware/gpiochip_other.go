//go:build !linux

package hardware

func gpioChipInfo(string) (string, uint32, bool) { return "", 0, false }
