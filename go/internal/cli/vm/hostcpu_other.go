//go:build !amd64

package vm

// hostX86V3 is false on a host that is not x86-64: it cannot pass an x86-64
// CPU through to a guest.
func hostX86V3() bool { return false }
