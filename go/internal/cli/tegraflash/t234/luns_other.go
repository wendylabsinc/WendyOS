//go:build darwin || windows

package t234

// These platforms withdraw their whole-disk provider on medium eject.
func listUMSLUNs() ([]UMSDisk, error) { return listUMSDisks() }

func ejectLegacyUMSDisk(d UMSDisk) error { return ejectUMSDisk(d) }
