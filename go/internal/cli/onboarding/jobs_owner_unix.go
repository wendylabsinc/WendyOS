//go:build darwin || linux

package onboarding

import (
	"os"
	"syscall"
)

// Elevated workers preserve the directory owner's ability to inspect checkpoints.
func preserveJobOwner(f *os.File, root string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return f.Chown(int(st.Uid), int(st.Gid))
}
