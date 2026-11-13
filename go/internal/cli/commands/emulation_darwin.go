package commands

import "golang.org/x/sys/unix"

// rosettaTranslated reports whether this process runs under Rosetta 2. The
// sysctl is 1 for a translated process and 0 for a native one on Apple
// silicon; Intel Macs lack it, which reads as not translated.
func rosettaTranslated() bool {
	v, err := unix.SysctlUint32("sysctl.proc_translated")
	return err == nil && v == 1
}
