//go:build !linux

package services

func hostRootMedium() (medium, disk string, err error) { return "", "", nil }
