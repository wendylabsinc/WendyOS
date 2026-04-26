//go:build !linux

package services

import "fmt"

func rebootSystem() error {
	return fmt.Errorf("reboot not supported on this platform")
}
