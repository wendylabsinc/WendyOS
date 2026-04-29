//go:build darwin

package commands

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// drive represents an external disk suitable for image writing.
type drive struct {
	DevicePath  string      // e.g. /dev/disk4
	RawPath     string      // e.g. /dev/rdisk4
	Name        string      // human-readable name
	Size        string      // human-readable size
	SizeBytes   int64       // size in bytes
	IsRemovable bool
	StorageType StorageType // detected medium: StorageSD, StorageNVMe, or StorageUnknown
}

// listAllDrives lists external physical drives (NVMe, USB, SD cards) on macOS.
func listAllDrives() ([]drive, error) {
	return listDrivesText()
}

// listExternalDrives uses diskutil to find external removable drives on macOS.
func listExternalDrives() ([]drive, error) {
	return listDrivesText()
}

// listDrivesText parses the text output of `diskutil list external physical`
// and `diskutil list internal physical` to find writable external/removable
// drives. It checks both external and internal physical disks because built-in
// SD card readers present media as internal on macOS.
func listDrivesText() ([]drive, error) {
	out, err := exec.Command("diskutil", "list", "external", "physical").Output()
	if err != nil {
		return nil, fmt.Errorf("running diskutil: %w", err)
	}

	seen := make(map[string]bool)
	drives := parseDiskutilOutput(out, seen, true)

	// Also check internal physical disks for removable media
	// (e.g., built-in SD card readers show as "internal" on macOS).
	internalOut, err := exec.Command("diskutil", "list", "internal", "physical").CombinedOutput()
	if err != nil {
		// Surface a warning instead of silently ignoring the failure so that
		// users can diagnose missing drives (e.g., SD cards in built-in readers).
		fmt.Fprintf(os.Stderr, "warning: failed to list internal physical disks with diskutil: %v\n", err)
	} else {
		for _, d := range parseDiskutilOutput(internalOut, seen, false) {
			if d.IsRemovable {
				drives = append(drives, d)
			}
		}
	}

	return drives, nil
}

// parseDiskutilOutput extracts drive entries from diskutil list output.
// When isExternal is true, all drives are marked removable. When false,
// removability is determined from diskutil info (Removable Media / Ejectable).
func parseDiskutilOutput(out []byte, seen map[string]bool, isExternal bool) []drive {
	var drives []drive
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		// Lines like: /dev/disk4 (external, physical):
		if !strings.HasPrefix(line, "/dev/disk") {
			continue
		}

		parts := strings.SplitN(line, " ", 2)
		devPath := strings.TrimSuffix(parts[0], ":")
		if seen[devPath] {
			continue
		}
		seen[devPath] = true
		rawPath := strings.Replace(devPath, "/dev/disk", "/dev/rdisk", 1)

		// Get disk info for size, name, and removability.
		info, infoErr := getDiskInfo(devPath)
		name := devPath
		size := ""
		var sizeBytes int64
		removable := isExternal
		if infoErr == nil {
			if info.name != "" {
				name = info.name
			}
			size = info.size
			sizeBytes = info.sizeBytes
			if !isExternal {
				removable = info.removable || info.ejectable
			}
		}

		st := StorageUnknown
		if infoErr == nil {
			st = detectStorageTypeDarwin(info.protocol, info.name)
		}

		drives = append(drives, drive{
			DevicePath:  devPath,
			RawPath:     rawPath,
			Name:        name,
			Size:        size,
			SizeBytes:   sizeBytes,
			IsRemovable: removable,
			StorageType: st,
		})
	}
	return drives
}

type diskInfo struct {
	name      string
	size      string
	sizeBytes int64
	removable bool // "Removable Media: Removable"
	ejectable bool // "Ejectable: Yes"
	protocol  string // e.g. "SD", "USB", "SATA"
}

func getDiskInfo(devPath string) (*diskInfo, error) {
	out, err := exec.Command("diskutil", "info", devPath).Output()
	if err != nil {
		return nil, err
	}

	info := &diskInfo{}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "Disk Size:") {
			info.size = strings.TrimSpace(strings.TrimPrefix(line, "Disk Size:"))
			// Parse byte count from e.g. "31.9 GB (31,914,983,424 Bytes)..."
			if start := strings.Index(info.size, "("); start != -1 {
				if end := strings.Index(info.size[start:], " Bytes"); end != -1 {
					rawBytes := info.size[start+1 : start+end]
					// diskutil may include thousands separators (commas); remove them before parsing.
					rawBytes = strings.ReplaceAll(rawBytes, ",", "")
					fmt.Sscanf(rawBytes, "%d", &info.sizeBytes)
				}
			}
		}
		if strings.HasPrefix(line, "Device / Media Name:") {
			info.name = strings.TrimSpace(strings.TrimPrefix(line, "Device / Media Name:"))
		}
		if strings.HasPrefix(line, "Removable Media:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "Removable Media:"))
			info.removable = strings.HasPrefix(strings.ToLower(val), "removable")
		}
		if strings.HasPrefix(line, "Ejectable:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "Ejectable:"))
			info.ejectable = strings.EqualFold(val, "yes")
		}
		if strings.HasPrefix(line, "Protocol:") {
			info.protocol = strings.TrimSpace(strings.TrimPrefix(line, "Protocol:"))
		}
	}
	return info, nil
}

// detectStorageTypeDarwin infers the physical storage medium of a disk from the
// diskutil protocol string and media name.
//
//   - Internal SD card readers report Protocol: SD (or SDXC/SDHC).
//   - External USB SD card readers report Protocol: USB but their media name
//     usually contains "SD", "SDHC", "SDXC", or "MMC".
//   - NVMe drives in USB enclosures report Protocol: USB with a vendor/model name.
func detectStorageTypeDarwin(protocol, mediaName string) StorageType {
	p := strings.ToUpper(strings.TrimSpace(protocol))
	// Anything whose protocol begins with "SD" is an SD card slot.
	if p == "SD" || strings.HasPrefix(p, "SD") || p == "MMC" || p == "SDXC" || p == "SDHC" {
		return StorageSD
	}
	if p == "USB" {
		lower := strings.ToLower(mediaName)
		for _, kw := range []string{"emmc", "e-mmc", "embedded mmc"} {
			if strings.Contains(lower, kw) {
				return StorageEMMC
			}
		}
		for _, kw := range []string{"sd card", "sdhc", "sdxc", "sd/mmc", " mmc", "sdcard"} {
			if strings.Contains(lower, kw) {
				return StorageSD
			}
		}
		// USB without SD/eMMC indicators is assumed to be an NVMe drive in an enclosure.
		return StorageNVMe
	}
	return StorageUnknown
}

// unmountDisk unmounts all volumes on a disk before writing.
// Falls back to force-unmount if the normal unmount fails.
func unmountDisk(devPath string) error {
	cmd := exec.Command("sudo", "diskutil", "unmountDisk", devPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Retry with force unmount.
		forceCmd := exec.Command("sudo", "diskutil", "unmountDisk", "force", devPath)
		if forceOut, forceErr := forceCmd.CombinedOutput(); forceErr != nil {
			return fmt.Errorf("unmounting %s: %s\nClose Finder windows, Disk Utility, or any apps using the disk, then retry", devPath, string(forceOut)+string(out))
		}
	}
	return nil
}

// writeImageToDisk writes an image file to a raw disk device using dd. dd
// reads the file directly (rather than via a stdin pipe) so that bs=4m
// actually produces 4 MiB writes to /dev/rdiskN — pipe input forces dd to
// issue one write per pipe-buffer-sized read, which is dramatically slower
// on raw devices. Progress is driven by periodically signaling dd with
// SIGINFO, which makes BSD dd emit a status line on stderr.
//
// SIGINFO delivery is non-trivial because sudo on macOS uses a PAM-based
// monitor process that calls setsid() and runs dd in a new session/pgid,
// and sudo does not relay SIGINFO to the command. So we can't just signal
// the recorded sudo pid or its pgroup — we have to walk the process tree
// to find dd itself, then signal it (via sudo, since dd is owned by root).
func writeImageToDisk(imagePath string, d drive, progressFn func(written int64)) error {
	if err := unmountDisk(d.DevicePath); err != nil {
		return err
	}

	// Use rdisk for faster raw writes on macOS.
	// conv=sync pads the final partial block so the total length is a
	// multiple of bs, which raw devices require.
	cmd := exec.Command("sudo", "dd",
		fmt.Sprintf("if=%s", imagePath),
		fmt.Sprintf("of=%s", d.RawPath),
		"bs=4m",
		"conv=sync",
	)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("creating stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting dd: %w", err)
	}
	sudoPid := cmd.Process.Pid

	done := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		// Send the first signal quickly so the user sees activity within a
		// second, rather than waiting a full tick. The brief delay gives
		// sudo's monitor time to fork and exec dd, so ps can find it.
		select {
		case <-done:
			return
		case <-time.After(750 * time.Millisecond):
		}
		signalDD(sudoPid)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				signalDD(sudoPid)
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		scanDDProgressBSD(stderr, progressFn)
	}()

	waitErr := cmd.Wait()
	close(done)
	wg.Wait()

	if waitErr != nil {
		return fmt.Errorf("writing image: %w", waitErr)
	}

	// Sync to flush any remaining writes.
	exec.Command("sync").Run() //nolint:errcheck

	return nil
}

// signalDD locates the dd descendant of the given sudo PID and sends it
// SIGINFO so it prints a status line to stderr. dd is owned by root, so the
// kill itself is run via sudo (-n: never prompt; relies on cached creds).
//
// We walk descendants instead of using process group or `pkill -f`:
//   - process groups don't work because sudo's monitor calls setsid()
//   - `pkill -f` is fragile against regex escaping in image paths
//
// Best-effort: any failure here just means the user sees no progress update
// for that tick.
func signalDD(sudoPid int) {
	pid, ok := findDescendantNamed(sudoPid, "dd")
	if !ok {
		// Fallback: if sudo exec'd directly into dd (no monitor), the sudo
		// pid IS dd. Signal it.
		exec.Command("sudo", "-n", "kill", "-INFO", strconv.Itoa(sudoPid)).Run() //nolint:errcheck
		return
	}
	exec.Command("sudo", "-n", "kill", "-INFO", strconv.Itoa(pid)).Run() //nolint:errcheck
}

// findDescendantNamed walks the process tree rooted at root and returns the
// first descendant whose command name (argv[0] basename) is name.
// Implemented via a single `ps` invocation rather than recursive `pgrep` to
// avoid spawning multiple processes per tick.
func findDescendantNamed(root int, name string) (int, bool) {
	out, err := exec.Command("ps", "-Ao", "pid=,ppid=,comm=").Output()
	if err != nil {
		return 0, false
	}
	return findDescendantInPSOutput(string(out), root, name)
}

// findDescendantInPSOutput parses the output of `ps -Ao pid=,ppid=,comm=` and
// returns the first descendant of root whose basename matches name. Split out
// from findDescendantNamed so it can be unit-tested without spawning ps.
func findDescendantInPSOutput(psOutput string, root int, name string) (int, bool) {
	type entry struct {
		ppid int
		comm string
	}
	procs := make(map[int]entry, 256)
	for _, line := range strings.Split(psOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		// comm may contain spaces; rejoin remaining fields and basename it.
		comm := strings.Join(fields[2:], " ")
		if i := strings.LastIndex(comm, "/"); i >= 0 {
			comm = comm[i+1:]
		}
		procs[pid] = entry{ppid: ppid, comm: comm}
	}
	// BFS from root.
	queue := []int{root}
	visited := map[int]bool{root: true}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for pid, e := range procs {
			if e.ppid != cur || visited[pid] {
				continue
			}
			if e.comm == name {
				return pid, true
			}
			visited[pid] = true
			queue = append(queue, pid)
		}
	}
	return 0, false
}

// scanDDProgressBSD parses BSD dd's stderr output and invokes progressFn with
// the running byte count. Each SIGINFO triggers three lines, the third of
// which contains "<n> bytes transferred in ...".
func scanDDProgressBSD(r io.Reader, progressFn func(written int64)) {
	if progressFn == nil {
		io.Copy(io.Discard, r) //nolint:errcheck
		return
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		idx := strings.Index(line, " bytes transferred")
		if idx <= 0 {
			continue
		}
		token := strings.TrimSpace(line[:idx])
		written, err := strconv.ParseInt(token, 10, 64)
		if err != nil {
			continue
		}
		progressFn(written)
	}
}

// ejectDisk ejects the disk so macOS shows the safe-to-remove notification.
// Called by the caller after all post-flash operations are complete.
func ejectDisk(devPath string) {
	exec.Command("diskutil", "eject", devPath).Run() //nolint:errcheck
}
