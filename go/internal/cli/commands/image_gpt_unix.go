//go:build darwin || linux

package commands

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// bmapWriteArgs are the __bmap-write flags that pass the write limit to the
// helper.
func (f imageFit) bmapWriteArgs() []string {
	if f.writeLimit <= 0 {
		return nil
	}
	return []string{"--write-limit", strconv.FormatInt(f.writeLimit, 10)}
}

// finishImageWrite runs after an image write succeeds: it rewrites the GPT
// for the drive's size when fit asks for it.
func finishImageWrite(d drive, fit imageFit) error {
	if !fit.relocateGPT {
		return nil
	}
	if err := relocateDriveGPT(d); err != nil {
		return &gptRelocationError{drive: d.DevicePath, err: err}
	}
	return nil
}

// relocateDriveGPT rewrites d's GPT for the drive's size (see relocateGPT).
// Raw writes need root, so like the block-map write it re-execs this binary as
// `sudo wendy __gpt-relocate`, after unmounting whatever the OS mounted from
// the freshly written partition table.
func relocateDriveGPT(d drive) error {
	if err := unmountDisk(d.DevicePath); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating wendy binary: %w", err)
	}
	cmd := exec.Command("sudo", self, "__gpt-relocate", "--device", d.RawPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%w\n%s", err, msg)
		}
		return err
	}
	exec.Command("sync").Run() //nolint:errcheck
	return nil
}
