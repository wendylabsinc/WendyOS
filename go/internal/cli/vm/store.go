package vm

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// vmNameRe constrains a VM name to something that is safe as a directory
// component and usable as a hostname: lowercase, no separators, no traversal.
var vmNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// ValidName reports whether name may be used for a VM.
func ValidName(name string) error {
	if !vmNameRe.MatchString(name) {
		return fmt.Errorf("invalid VM name %q: use 1-32 lowercase letters, digits or dashes, starting and ending with a letter or digit", name)
	}
	return nil
}

// Store is where the VMs this CLI manages live: ~/.wendy/vms/<name>/.
type Store struct {
	Root string
}

// NewStore returns the store rooted in the CLI's own config directory, so VM
// disks sit beside the config that refers to them and inherit its 0700 mode.
func NewStore() (*Store, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return nil, err
	}
	return &Store{Root: filepath.Join(dir, "vms")}, nil
}

func (s *Store) Dir(name string) string      { return filepath.Join(s.Root, name) }
func (s *Store) DiskPath(name string) string { return filepath.Join(s.Dir(name), "disk.img") }
func (s *Store) VarsPath(name string) string { return filepath.Join(s.Dir(name), "efivars.fd") }

// CheckCreatable reports whether a VM of this name could be created now, so a
// caller can reject a bad or duplicate name before spending a download on it.
// Advisory only: CreateFrom re-checks.
func (s *Store) CheckCreatable(name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	if _, err := os.Stat(s.Dir(name)); err == nil {
		return fmt.Errorf("VM %q already exists; remove it with 'wendy vm rm %s'", name, name)
	}
	return nil
}

// CreateFrom provisions a new VM from an open image stream: a private, resizable
// copy of the image plus an empty UEFI variable store.
//
// A stream rather than a path because the published image is compressed:
// decompressing to a temporary file only to copy it again would double both the
// disk cost and the wait.
//
// imageSize is the image's uncompressed size, used to reject a disk too small to
// hold it. It may overstate the stream's length, but must never understate it.
//
// meta records where the image came from. It is written last, inside the same
// rollback guard, so a VM directory never outlives a failed create.
func (s *Store) CreateFrom(name string, image io.Reader, imageSize, diskBytes int64, meta Meta) (retErr error) {
	if err := ValidName(name); err != nil {
		return err
	}
	// A disk truncated below the image loses the last partition and the GPT
	// backup header that sits at the very end.
	if diskBytes < imageSize {
		return fmt.Errorf("disk size %d is smaller than the image (%d bytes)", diskBytes, imageSize)
	}
	// Each creation gets a fresh identity, even when its name matches a VM on
	// another host. Persist it with the disk so restarts preserve DHCP leases.
	mac, err := newMAC()
	if err != nil {
		return fmt.Errorf("allocating VM MAC: %w", err)
	}
	meta.MAC = mac
	lifecycle, err := s.acquireLifecycleLock(name)
	if err != nil {
		return err
	}
	// Registered before rollback and file closes: ownership lasts until all
	// cleanup has finished, including cleanup after a close/writeback error.
	defer lifecycle.Close()

	dir := s.Dir(name)
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return fmt.Errorf("creating VM store: %w", err)
	}
	// Mkdir, not MkdirAll: only the process that exclusively creates this
	// directory owns its rollback. A losing concurrent create must never
	// remove the winning creator's disk after its O_EXCL open fails.
	if err := os.Mkdir(dir, 0o700); err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("VM %q already exists; remove it with 'wendy vm rm %s'", name, name)
		}
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	// The likeliest failure below is running out of room, and a half-written
	// disk left behind would both hold that space and make the retry report
	// "already exists". Registered before the Close defers so it runs after
	// them, and so covers a failure they report. Only ever removes a directory
	// this call created.
	defer func() {
		if retErr == nil {
			return
		}
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not clean up %s: %v; "+
				"remove it before retrying\n", dir, rmErr)
		}
	}()

	// Defense in depth: never open a VM disk for truncation.
	disk, err := os.OpenFile(s.DiskPath(name), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating disk: %w", err)
	}
	// A close error on a file this large can be the first report of a failed
	// writeback, which would otherwise leave a corrupt disk behind a success.
	defer func() {
		if cerr := disk.Close(); cerr != nil && retErr == nil {
			retErr = fmt.Errorf("closing disk: %w", cerr)
		}
	}()

	// Bounded one byte past the disk: a compressed source reports size 0, so the
	// up-front guard cannot fire and an over-large image would otherwise be
	// written in full before the check below rejects it.
	written, err := io.Copy(disk, io.LimitReader(image, diskBytes+1))
	if err != nil {
		return fmt.Errorf("writing image: %w", err)
	}
	// The up-front guard cannot fire when imageSize is 0, as a compressed source
	// reports until its size is known. Without this the Truncate below would
	// silently cut the image short.
	if diskBytes < written {
		return fmt.Errorf("disk size %d is smaller than the image (%d bytes)", diskBytes, written)
	}
	// Grow, not preallocate: the image is sparse-extended so /data can be grown
	// into the space on first boot without writing gigabytes now.
	if err := disk.Truncate(diskBytes); err != nil {
		return fmt.Errorf("sizing disk: %w", err)
	}

	vars, err := os.OpenFile(s.VarsPath(name), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating UEFI variable store: %w", err)
	}
	defer func() {
		if cerr := vars.Close(); cerr != nil && retErr == nil {
			retErr = fmt.Errorf("closing UEFI variable store: %w", cerr)
		}
	}()
	if err := vars.Truncate(pflashBytes); err != nil {
		return fmt.Errorf("sizing UEFI variable store: %w", err)
	}

	meta.Name = name
	meta.DiskBytes = diskBytes
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = time.Now().UTC()
	}
	if err := s.WriteMeta(meta); err != nil {
		return fmt.Errorf("recording VM metadata: %w", err)
	}
	return nil
}

// List returns the names of every VM in the store, sorted.
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", s.Root, err)
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() && ValidName(e.Name()) == nil {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// Remove deletes a VM and its disk, refusing while it is running.
//
// The check lives here rather than only in the command: deleting the image a
// live emulator has mapped corrupts it, and every caller deserves that
// guarantee. Take the run lock to make the check and the delete atomic -- a
// VM started between a separate Status call and the delete would otherwise be
// destroyed underneath QEMU.
func (s *Store) Remove(name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	lifecycle, err := s.acquireLifecycleLock(name)
	if err != nil {
		return err
	}
	defer lifecycle.Close()
	if _, err := os.Stat(s.Dir(name)); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no VM named %q", name)
		}
		return err
	}
	lock, err := s.acquireRunLockUnderLifecycle(name)
	if err != nil {
		return err
	}
	// The disk goes first and under the lock. Releasing the lock and only then
	// deleting would leave a window where a racing start acquires it, passes
	// its own existence check and boots an image this call is midway through
	// removing.
	if err := os.Remove(s.DiskPath(name)); err != nil && !os.IsNotExist(err) {
		_ = lock.Close()
		return err
	}
	// Closed before the rest, not deferred: Windows cannot unlink an open file,
	// so a held handle would leave a half-removed directory. A start that wins
	// the lock from here on fails on the disk that is already gone.
	_ = lock.Close()
	return os.RemoveAll(s.Dir(name))
}
