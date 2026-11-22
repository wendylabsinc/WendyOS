package vm

import "golang.org/x/sys/cpu"

// hostX86V3 reports whether the host CPU has the x86-64-v3 features the
// x86-64 WendyOS image is built for. KVM passes the host CPU through, so a
// guest on an older CPU would fault on its first AVX2 instruction.
func hostX86V3() bool {
	x := cpu.X86
	return x.HasAVX && x.HasAVX2 && x.HasBMI1 && x.HasBMI2 && x.HasF16C &&
		x.HasFMA && x.HasLZCNT && x.HasMOVBE && x.HasOSXSAVE
}
