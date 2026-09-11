//go:build windows

package scan

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/shared/env"
)

// RunBLECheck is a no-op on Windows. The CoreBluetooth entitlement problem it
// exists for is macOS-only; here an unavailable radio surfaces as an ordinary
// error from newScanner.
func RunBLECheck() int { return 0 }

// watcherScript drives a WinRT BluetoothLEAdvertisementWatcher from Windows
// PowerShell 5.1 and prints one JSON object per sighting to stdout.
//
// PowerShell 5.1 specifically: it is the WinRT-projecting host, so
// [Type, Assembly, ContentType=WindowsRuntime] resolves. PowerShell 7 dropped
// that projection. env.PowershellExe resolves WindowsPowerShell\v1.0, which is
// exactly 5.1.
//
// Events are queued with Register-ObjectEvent and drained by the main loop
// rather than printed from an -Action block: an action scriptblock runs in its
// own runspace and its output does not reach the parent's stdout, which is the
// classic way this pattern fails.
//
// %s is replaced by the service-UUID filter setup.
const watcherScript = `
$ErrorActionPreference = 'Stop'
try {
    $watcher = New-Object 'Windows.Devices.Bluetooth.Advertisement.BluetoothLEAdvertisementWatcher, Windows, ContentType=WindowsRuntime'
    # Active scanning solicits scan responses, where devices commonly put the
    # local name and the full service UUID list.
    $watcher.ScanningMode = [Windows.Devices.Bluetooth.Advertisement.BluetoothLEScanningMode, Windows, ContentType=WindowsRuntime]::Active
%s
    Register-ObjectEvent -InputObject $watcher -EventName Received -SourceIdentifier BLESighting | Out-Null
    $watcher.Start()

    while ($true) {
        $events = @(Get-Event -SourceIdentifier BLESighting -ErrorAction SilentlyContinue)
        foreach ($e in $events) {
            Remove-Event -EventIdentifier $e.EventIdentifier -ErrorAction SilentlyContinue
            $args0 = $e.SourceArgs[1]
            if ($null -eq $args0) { continue }
            $uuids = @()
            foreach ($u in $args0.Advertisement.ServiceUuids) { $uuids += $u.ToString() }
            $record = [ordered]@{
                address = ('{0:X12}' -f $args0.BluetoothAddress)
                name    = [string]$args0.Advertisement.LocalName
                uuids   = $uuids
                rssi    = [int]$args0.RawSignalStrengthInDBm
            }
            Write-Output ($record | ConvertTo-Json -Compress -Depth 3)
        }
        [Console]::Out.Flush()
        Start-Sleep -Milliseconds 200
    }
} catch {
    [Console]::Error.WriteLine($_.Exception.Message)
    exit 1
}
`

// buildWatcherScript fills in the service filter. WinRT wants full 128-bit
// GUIDs, which is what the engine's canonicalization already produces.
func buildWatcherScript(services []string) string {
	if len(services) == 0 {
		return fmt.Sprintf(watcherScript, "")
	}
	var b strings.Builder
	for _, svc := range services {
		// Only canonical 36-character UUIDs are accepted; anything else would
		// make [Guid] throw and kill the whole scan for one bad entry.
		if len(svc) != 36 {
			continue
		}
		fmt.Fprintf(&b, "    $watcher.AdvertisementFilter.Advertisement.ServiceUuids.Add([Guid]'%s')\n", svc)
	}
	return fmt.Sprintf(watcherScript, b.String())
}

// windowsScanner holds the PowerShell watcher process and the sightings read
// from it so far.
type windowsScanner struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc

	mu        sync.Mutex
	devices   map[string]BLEDeviceInfo
	readErr   error
	closeOnce sync.Once
}

// newScanner starts one long-lived PowerShell watcher for the whole session.
// Re-invoking it per sample would be far too slow — process start plus WinRT
// projection is on the order of a second.
func newScanner(ctx context.Context, services []string) (scanner, error) {
	// A child context, so Close stops the process even when the caller's ctx
	// outlives the scan.
	scanCtx, cancel := context.WithCancel(ctx)

	cmd := exec.CommandContext(scanCtx, env.PowershellExe(),
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-Command", buildWatcherScript(services))

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("opening PowerShell stdout: %w", err)
	}
	// Drained concurrently with stdout: PowerShell writing a long error would
	// otherwise fill the stderr pipe and block the process.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("opening PowerShell stderr: %w", err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("starting PowerShell BLE watcher: %w", err)
	}

	s := &windowsScanner{
		cmd:     cmd,
		cancel:  cancel,
		devices: make(map[string]BLEDeviceInfo),
	}

	// cmd.Wait must not run until both pipe readers are done — exec closes the
	// pipes underneath them otherwise, which is a documented race. So the two
	// readers are tracked and reaped by a third goroutine.
	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		s.readLoop(stdout)
	}()
	go func() {
		defer readers.Done()
		s.drainStderr(stderr)
	}()
	go func() {
		readers.Wait()
		s.reap(s.cmd.Wait())
	}()

	return s, nil
}

// readLoop folds stdout lines into the device map until the pipe closes.
func (s *windowsScanner) readLoop(stdout io.ReadCloser) {
	scanner := bufio.NewScanner(stdout)
	// A sighting line is small, but a device advertising many UUIDs plus a long
	// name can exceed bufio's default 64 KiB ceiling on a pathological record.
	scanner.Buffer(make([]byte, 0, 4096), 256*1024)

	for scanner.Scan() {
		dev, ok := parseWinRTLine(scanner.Text())
		if !ok {
			continue
		}
		s.mu.Lock()
		s.devices[dev.Address] = dev
		s.mu.Unlock()
	}

	// Scan stops on a clean EOF (Err() == nil) when Close kills the process —
	// expected, nothing to report. Any other Err() (a pipe I/O error, or
	// bufio.ErrTooLong from a pathologically long line) means stdout has
	// stopped being drained while the process may still be running and
	// writing: left alone it will eventually block on a full pipe. Cancel it
	// so it can't leak, and record why.
	if err := scanner.Err(); err != nil {
		s.setReadErr(fmt.Errorf("PowerShell BLE watcher stdout: %w", err))
		s.cancel()
	}
}

// setReadErr records the first fatal error, ignoring later ones so the
// original cause of failure is preserved.
func (s *windowsScanner) setReadErr(err error) {
	s.mu.Lock()
	if s.readErr == nil {
		s.readErr = err
	}
	s.mu.Unlock()
}

// reap records that PowerShell exited. The watcher is meant to run until Close
// kills it, so an exit on its own has to end the stream rather than leave
// Snapshot returning a frozen set forever. A Close-initiated kill also lands
// here, which is harmless: the engine has already stopped sampling by then.
func (s *windowsScanner) reap(err error) {
	if err != nil {
		s.setReadErr(fmt.Errorf("PowerShell BLE watcher exited: %w", err))
		return
	}
	s.setReadErr(fmt.Errorf("PowerShell BLE watcher exited unexpectedly"))
}

// drainStderr keeps the stderr pipe empty and captures the first line, which is
// the message the script's catch block prints.
func (s *windowsScanner) drainStderr(stderr io.ReadCloser) {
	sc := bufio.NewScanner(stderr)
	first := ""
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" && first == "" {
			first = line
		}
	}
	if first == "" {
		return
	}
	s.setReadErr(fmt.Errorf("PowerShell BLE watcher: %s", first))
}

// Snapshot ignores ctx: it reads in-memory state a background PowerShell
// watcher already populated, so there is nothing here that can hang.
//
// Devices and a fatal readErr are returned together rather than favoring one
// or the other — sample in scan.go merges and emits whatever is here before
// honoring a non-nil error, so nothing found right up to the moment the
// watcher died is lost.
func (s *windowsScanner) Snapshot(_ context.Context) ([]BLEDeviceInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]BLEDeviceInfo, 0, len(s.devices))
	for _, d := range s.devices {
		out = append(out, d)
	}
	return out, s.readErr
}

// Close stops the watcher process. Idempotent.
func (s *windowsScanner) Close() {
	s.closeOnce.Do(func() {
		// Cancelling the command context kills PowerShell, which ends readLoop
		// and drainStderr by closing their pipes.
		s.cancel()
	})
}
