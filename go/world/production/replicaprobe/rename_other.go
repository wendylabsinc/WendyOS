//go:build !linux && !darwin

package replicaprobe

import "golang.org/x/sys/unix"

func renameNoReplace(int, string, string) error { return unix.ENOTSUP }
