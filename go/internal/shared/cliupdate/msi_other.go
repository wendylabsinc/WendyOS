//go:build !windows

package cliupdate

func ownsMSIExecutable(string) bool { return false }
