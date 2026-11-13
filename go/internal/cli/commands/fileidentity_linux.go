package commands

import (
	"io/fs"
	"syscall"
)

// fileIdentityOf returns info's identity, or false when info did not come
// from a stat of a local file.
func fileIdentityOf(info fs.FileInfo) (fileIdentity, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return fileIdentity{}, false
	}
	return fileIdentity{
		Size:    st.Size,
		MtimeNs: st.Mtim.Nano(),
		CtimeNs: st.Ctim.Nano(),
		Dev:     uint64(st.Dev),
		Ino:     uint64(st.Ino),
	}, true
}
