//go:build darwin || windows

package commands

import "fmt"

func serialPortGroup(_ string) string {
	return ""
}

func portPermissionError(portPath, group string, _ error) error {
	return fmt.Errorf("Permission denied to access USB device %s. To have access, you need to be part of the user group '%s'.", portPath, group)
}
