//go:build !linux

package data

func defaultDataRoot() (string, error) { return DefaultRoot, nil }
