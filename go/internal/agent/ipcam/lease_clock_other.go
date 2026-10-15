//go:build !linux

package ipcam

import "time"

func leaseBootID() (string, error)          { return "", errLinkUnsupported }
func leaseBootTime() (time.Duration, error) { return 0, errLinkUnsupported }
