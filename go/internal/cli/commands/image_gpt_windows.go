//go:build windows

package commands

// finishLockedImageWrite runs after an image write succeeds: it rewrites the
// GPT for the drive's size when fit asks for it. It works through the writer's
// own locked handle, before the disk is released: reopening it would clear
// its partitions (openLockedDisk runs Clear-Disk), and Windows rescans the
// partition table on release, so it should find the final one.
func finishLockedImageWrite(ld *lockedDisk, d drive, fit imageFit) error {
	if !fit.relocateGPT {
		return nil
	}
	if err := relocateGPTOnDevice(ld.diskFile); err != nil {
		return &gptRelocationError{drive: d.DevicePath, err: err}
	}
	return nil
}
