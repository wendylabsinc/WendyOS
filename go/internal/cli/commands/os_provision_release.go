package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Pacing for releasing the media after provisioning; vars so tests need not wait.
var (
	unmountAttempts   = 5
	unmountRetryDelay = 500 * time.Millisecond
	ejectAttempts     = 3
	ejectRetryDelay   = 2 * time.Second
)

// commandRunner runs one command and returns its combined output.
type commandRunner func(args ...string) ([]byte, error)

// configStillMountedError is a config partition this CLI mounted and could not
// unmount. Returned on its own, it means every file was written.
type configStillMountedError struct {
	dir   string
	cause error
}

func (e *configStillMountedError) Error() string {
	return fmt.Sprintf("the config partition is still mounted at %s: run 'sudo umount %s' before removing the drive: %v",
		e.dir, e.dir, e.cause)
}

func (e *configStillMountedError) Unwrap() error { return e.cause }

// splitStillMounted separates a written but still mounted config partition
// from requested provisioning that did not land.
func splitStillMounted(err error) (notApplied, stillMounted error) {
	if _, ok := err.(*configStillMountedError); ok {
		return nil, err
	}
	return err, nil
}

// releaseMount unmounts a mount point this CLI made, retrying while something
// holds the fresh volume. Remove, never RemoveAll: if the volume were still
// mounted there, RemoveAll would delete the partition's files.
func releaseMount(dir string, unmount func() ([]byte, error), sleep func(time.Duration)) error {
	for attempt := 1; ; attempt++ {
		out, err := unmount()
		if err == nil {
			break
		}
		if attempt >= unmountAttempts {
			return &configStillMountedError{dir: dir, cause: fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)}
		}
		sleep(unmountRetryDelay)
	}
	os.Remove(dir) //nolint:errcheck // an empty temp dir left behind is harmless
	return nil
}

// releaseMountBeforeEject is releaseMount for a drive that is ejected next: the
// eject also takes down a mount that refused to go, and fails if it cannot.
func releaseMountBeforeEject(dir string, unmount func() ([]byte, error), sleep func(time.Duration), w io.Writer) error {
	err := releaseMount(dir, unmount, sleep)
	var stillMounted *configStillMountedError
	if errors.As(err, &stillMounted) {
		fmt.Fprintf(w, "Could not unmount %s yet (%v); ejecting the drive will release it.\n", dir, stillMounted.cause)
		return nil
	}
	return err
}

// ejectWithDiskutil ejects dev through diskutil. When eject keeps failing,
// unmounting every volume still makes removal safe, so that counts as done.
func ejectWithDiskutil(dev string, diskutil commandRunner, sleep func(time.Duration), present func() bool, w io.Writer) error {
	var out []byte
	var err error
	delay := ejectRetryDelay
	for attempt := 1; attempt <= ejectAttempts; attempt++ {
		if out, err = diskutil("eject", dev); err == nil {
			return nil
		}
		if attempt < ejectAttempts {
			sleep(delay)
			delay *= 2
		}
	}
	ejectErr := fmt.Errorf("diskutil eject %s: %s: %w", dev, strings.TrimSpace(string(out)), err)
	if !present() {
		return fmt.Errorf("%s was disconnected before it could be ejected: %w", dev, ejectErr)
	}
	for _, args := range [][]string{{"unmountDisk", dev}, {"unmountDisk", "force", dev}} {
		if _, err := diskutil(args...); err == nil {
			fmt.Fprintf(w, "Could not eject %s, but all its volumes are unmounted and it is safe to remove.\n", dev)
			return nil
		}
	}
	return fmt.Errorf("%s is still mounted: eject it in Finder before removing it: %w", dev, ejectErr)
}
