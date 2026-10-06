package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/chunkupload"
	"github.com/wendylabsinc/wendy/go/internal/shared/flock"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

const (
	// chunkCompressionEnv selects the WriteChunks compression: auto (the
	// default), gzip or none. See chooseChunkUploadConfig.
	chunkCompressionEnv = "WENDY_CHUNK_COMPRESSION"

	// chunkStallTimeout is how long uncompressed WriteChunks streams may sit
	// open with no progress before the push reconnects and retries with gzip.
	// It exceeds the 17–27 s NVMe write stalls seen on an Orin Nano (WDY-3210).
	// A false positive costs more than that one retry: rememberChunkUploadStall
	// keeps the device on gzip for chunkStallMemory (30 days), so pick this
	// generously rather than risk flagging a merely slow — not wedged — link.
	chunkStallTimeout = 30 * time.Second

	// chunkStallMemory is how long a device that stalled uncompressed stays
	// on gzip.
	chunkStallMemory = 30 * 24 * time.Hour

	// chunkStallMemoryDays is chunkStallMemory expressed the way the fallback
	// notices word it. A const (rather than computing it at print time) keeps
	// the two notices (chunkresume.go and noteComposeChunkStall) and this
	// value from drifting apart the way a hard-coded "30 days" in each string
	// could.
	chunkStallMemoryDays = int(chunkStallMemory / (24 * time.Hour))

	// firstUncompressedChunkOSVersion is the first WendyOS that auto sends
	// uncompressed chunks to. #1765's link stall was reproduced on 0.18.2.
	firstUncompressedChunkOSVersion = "0.19.0"
)

// chunkUploadConfig is how one chunk push sends its WriteChunks messages.
type chunkUploadConfig struct {
	compressor   string        // chunkupload.Gzip, or "" for uncompressed
	stallTimeout time.Duration // > 0 runs the stall watchdog; set only when uncompressed
	stallKey     string        // the device's stall-memory key; "" records nothing
}

// gzipChunkUploadConfig is gzip with no watchdog: PR 1's transport, used over
// cloud tunnels, for old or unknown WendyOS, after a stall, and by callers
// that do not know their device.
var gzipChunkUploadConfig = chunkUploadConfig{compressor: chunkupload.Gzip}

// describe names the config for the WENDY_TIMING tuning line.
func (c chunkUploadConfig) describe() string {
	if c.compressor != "" {
		return c.compressor
	}
	return fmt.Sprintf("uncompressed, stall watchdog %s", c.stallTimeout)
}

// chunkUploadTarget is what the compression policy knows about a push.
type chunkUploadTarget struct {
	tunnel    bool   // a cloud tunnel: bandwidth-bound, so gzip pays for itself
	osVersion string // the agent's os_version, "" when unknown
	deviceKey string // deviceFingerprintKey, "" when unknown
	// directLink is true only when the device is reached at a link-local
	// address (USB-C NCM, or a direct cable with no DHCP server). Uncompressed
	// was measured about 15% faster than gzip there (WDY-3211, Orin Nano over
	// USB-C at 347 MB/s). A routable LAN or Wi-Fi hop has no matching
	// measurement — a compressible layer could go 2-3x slower uncompressed —
	// so auto keeps gzip there until the hardware matrix covers it.
	directLink bool
}

// stallKey identifies the device and its OS for the stall memory, so an OS
// update gives the device a fresh chance at uncompressed uploads.
func (t chunkUploadTarget) stallKey() string {
	if t.deviceKey == "" {
		return ""
	}
	return t.deviceKey + "@" + t.osVersion
}

// chooseChunkUploadConfig applies WENDY_CHUNK_COMPRESSION. gzip and none force
// that choice; anything else is auto, which sends uncompressed only to a
// device reached at a link-local address (t.directLink — USB-C NCM, or a
// direct cable with no DHCP) running WendyOS 0.19.0 or later with no recent
// stall, and picks gzip for everything else: a cloud tunnel, a routable LAN
// or Wi-Fi hop the hardware matrix hasn't measured, WendyOS before 0.19.0 or
// an unknown version, and a device that stalled uncompressed within
// chunkStallMemory. On a direct USB-C link to an Orin Nano, device-side
// gunzip (78 MB/s per core) was the upload's bottleneck (WDY-3211); the same
// tradeoff has not been measured off that link, so auto stays conservative
// there (WDY-3211 final-review fix wave, I1).
func chooseChunkUploadConfig(mode string, t chunkUploadTarget, stalledRecently func(key string) bool) chunkUploadConfig {
	uncompressed := chunkUploadConfig{stallTimeout: chunkStallTimeout, stallKey: t.stallKey()}
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "gzip":
		return gzipChunkUploadConfig
	case "none":
		return uncompressed
	}
	if t.tunnel || !t.directLink || osNeedsGzipChunks(t.osVersion) || stalledRecently(t.stallKey()) {
		return gzipChunkUploadConfig
	}
	return uncompressed
}

// osNeedsGzipChunks reports whether osVersion is a WendyOS older than
// firstUncompressedChunkOSVersion, or unknown. Dev builds and non-WendyOS
// version strings (a distro's "24.04") compare as new enough.
func osNeedsGzipChunks(osVersion string) bool {
	v := strings.TrimPrefix(strings.TrimSpace(osVersion), "WendyOS-")
	if v == "" {
		return true
	}
	if version.IsDev(v) {
		return false
	}
	return version.CompareVersions(v, firstUncompressedChunkOSVersion) < 0
}

// warnUnknownChunkCompressionOnce guards the unrecognized-WENDY_CHUNK_COMPRESSION
// warning so a long-lived process (or a loop of deploys in one `wendy`
// invocation) prints it once, not on every push. It is a *sync.Once, rather
// than a plain sync.Once, purely so tests can reset it between cases.
var warnUnknownChunkCompressionOnce = &sync.Once{}

// chunkCompressionModeFromEnv reads WENDY_CHUNK_COMPRESSION for
// chunkUploadConfigFor. An unrecognized value (anything but ""/auto/gzip/none,
// case- and space-insensitive — chooseChunkUploadConfig's own normalization)
// is most often a typo, and typo'd env vars are usually silent; warn about it
// once via cliNotice, naming the valid values, then fall back to auto exactly
// as chooseChunkUploadConfig already would. The validation lives here rather
// than in chooseChunkUploadConfig so that function stays a pure mapping from
// (already read) inputs to a config, with no I/O of its own.
func chunkCompressionModeFromEnv() string {
	mode := os.Getenv(chunkCompressionEnv)
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "auto", "gzip", "none":
	default:
		warnUnknownChunkCompressionOnce.Do(func() {
			cliNotice("%s=%q is not one of auto, gzip or none; using auto.", chunkCompressionEnv, mode)
		})
	}
	return mode
}

// chunkUploadConfigFor resolves the config for a push over conn. A failed
// version probe leaves the OS version unknown, which picks gzip.
//
// For non-tunnel connections, directLink comes from isLinkLocalIP(registryIPResolver(conn.Host)) —
// the same pair docker.go's Apple Container registry path already uses to tell a
// direct device hop from a routed one. For cloud tunnels (conn.Reconnect != nil),
// conn.Host is the device's display name (e.g. "Ethan Nano"), not an address; the
// lookup is skipped entirely because it would be doomed and the tunnel picks gzip
// regardless, so directLink stays false.
//
// conn.Addr is not a cheaper substitute for directLink's computation:
// it is set (grpcclient/client.go) to the exact dial string the caller
// passed in, i.e. conn.Host plus a port, never a separately resolved IP, so
// there is no second, already-resolved address to prefer over Host.
// registryIPResolver itself costs no network round trip for the common direct
// case: a USB-C NCM or bare-cable device's Host is already a link-local IP
// literal, which netip.ParseAddr matches before any DNS lookup is attempted;
// only a hostname target pays for the lookup registryIPResolver already makes
// on that path today.
//
// Session-broker connections (conn.IsSessionProxy) need no special case:
// ConnectSessionProxy sets Host to the broker's Spec.Host — the verified
// device's own host, not the broker's loopback socket — so the same rule
// applies unchanged. If that were ever loopback instead, isLinkLocalIP would
// correctly report false (loopback is not link-local), falling back to gzip
// exactly as the ruling requires for an unidentifiable device.
func chunkUploadConfigFor(ctx context.Context, conn *grpcclient.AgentConnection) chunkUploadConfig {
	isTunnel := conn.Reconnect != nil
	// For a tunnel, conn.Host is the device's display name (e.g. "Ethan Nano"),
	// not an address; resolveRegistryIP would run a doomed DNS lookup and mDNS
	// browse (~200 ms) with no outcome change, since a tunnel picks gzip
	// regardless. Skip it entirely for cloud tunnels.
	var directLink bool
	if !isTunnel {
		directLink = isLinkLocalIP(registryIPResolver(conn.Host))
	}
	t := chunkUploadTarget{
		tunnel:     isTunnel,
		directLink: directLink,
	}
	if v, err := agentVersionForRun(ctx, conn); err == nil {
		t.osVersion, t.deviceKey = v.GetOsVersion(), deviceFingerprintKey(v)
	}
	now := time.Now()
	return chooseChunkUploadConfig(chunkCompressionModeFromEnv(), t, func(key string) bool {
		return chunkUploadStalledRecently(key, now)
	})
}

// registryIPResolver is the function used to resolve a registry host to an IP.
// Tests swap it to observe/control resolution behavior. It defaults to
// resolveRegistryIP.
var registryIPResolver = resolveRegistryIP

// chunkStallTestDir, when non-empty, overrides the stall memory's directory.
var chunkStallTestDir string

func chunkStallPath() (string, error) {
	dir := chunkStallTestDir
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(base, "wendy")
	}
	return filepath.Join(dir, "chunk-upload-stalls.json"), nil
}

// loadChunkStalls returns the recorded stalls by key. A missing, unreadable
// or corrupt file reads as no stalls: the memory must never fail a deploy.
func loadChunkStalls() map[string]time.Time {
	stalls := map[string]time.Time{}
	p, err := chunkStallPath()
	if err != nil {
		return stalls
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return stalls
	}
	if err := json.Unmarshal(data, &stalls); err != nil {
		return map[string]time.Time{}
	}
	return stalls
}

// chunkUploadStalledRecently reports whether key's device stalled uncompressed
// within chunkStallMemory of now.
func chunkUploadStalledRecently(key string, now time.Time) bool {
	if key == "" {
		return false
	}
	at, ok := loadChunkStalls()[key]
	return ok && now.Sub(at) < chunkStallMemory
}

// rememberChunkUploadStall records that key's device stalled uncompressed at
// now, and drops entries older than chunkStallMemory. It replaces the file
// atomically, serializing writers with a lock file to prevent concurrent
// read-modify-write races. Callers treat an error as best effort.
func rememberChunkUploadStall(key string, now time.Time) error {
	if key == "" {
		return nil
	}
	p, err := chunkStallPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}

	// Serialize writers with a lock file to prevent concurrent
	// read-modify-write races from losing updates. Opened read-only (rather
	// than O_RDWR) and created 0644 rather than 0600: under `sudo wendy run`
	// this file (and the data file below) would otherwise become root-owned,
	// silently breaking the user's own later, unprivileged runs (M5). flock(2)
	// locks a whole file regardless of the descriptor's read/write mode on
	// darwin and linux (unlike fcntl(2) record locks, which do care) — verified
	// directly against this package's TryLock/Unlock on darwin; linux shares
	// the same flock(2) semantics and is exercised by this package's own
	// GOOS=linux build gate.
	lockPath := p + ".lock"
	f, err := os.OpenFile(lockPath, os.O_RDONLY|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("opening stall lock: %w", err)
	}
	defer f.Close()
	// OpenFile's mode is subject to the process umask, and a pre-existing
	// lock file from before this fix could still be 0600; Chmod makes the
	// 0644 exact. Best effort: a foreign-owned lock file this process can
	// already read (that's how it got this far) but not chmod is left as is,
	// which is no worse than before this fix.
	_ = f.Chmod(0o644)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	locked, err := flock.TryLock(f)
	if err != nil {
		return fmt.Errorf("acquiring stall lock: %w", err)
	}
	if !locked {
		if err := blockLockFile(ctx, f); err != nil {
			return fmt.Errorf("acquiring stall lock: %w", err)
		}
	}
	defer flock.Unlock(f)

	// Hold the lock across the entire read → prune → write → rename sequence.
	stalls := loadChunkStalls()
	for k, at := range stalls {
		if now.Sub(at) >= chunkStallMemory {
			delete(stalls, k)
		}
	}
	stalls[key] = now
	data, err := json.Marshal(stalls)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "chunk-upload-stalls-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	// os.CreateTemp creates the file 0600; match the 0644 of the lock file
	// and sibling cache files, so a run under sudo doesn't leave a
	// root-only-readable file blocking the user's later, unprivileged runs.
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}
