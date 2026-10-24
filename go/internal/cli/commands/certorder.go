package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/wendylabsinc/wendy-go/devices"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// The mTLS dial path probes every CLI certificate against a device until one
// is accepted (see connectResolvedAgent). The probe order is the order the
// certs happen to load in, so an operator holding certificates for several
// organisations pays a full, doomed TLS handshake plus a GetAgentVersion probe
// for every org that isn't the device's — on every single command. Measured
// against a device on the second of two orgs, that wasted attempt was roughly
// half of a ~250ms connect, and TLS session resumption cannot recover it: a
// rejected first attempt is a fresh handshake by definition.
//
// Remembering which organisation last worked for a host and probing that one
// first removes the waste. The memo is a pure optimisation with no security
// weight: it only reorders candidates, never adds one, and a stale or wrong
// entry costs exactly what today's behaviour costs, because the remaining
// certs are still probed in their original order behind it.

// certOrderMemo maps a device host to the organisation ID of the certificate
// that last completed a probe against it.
type certOrderMemo struct {
	Hosts map[string]int `json:"hosts"`
}

// certOrderMu serialises the read-modify-write of the memo file so two
// concurrent commands can't clobber each other's entries.
var certOrderMu sync.Mutex

// certOrderCacheDir resolves the base cache directory. Indirected through a
// variable so tests can redirect the memo instead of writing to the developer's
// real cache (os.UserCacheDir ignores XDG_CACHE_HOME on macOS).
var certOrderCacheDir = os.UserCacheDir

func certOrderPath() (string, error) {
	cacheDir, err := certOrderCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cacheDir, "wendy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "certorder.json"), nil
}

func loadCertOrderMemo() certOrderMemo {
	memo := certOrderMemo{Hosts: map[string]int{}}
	path, err := certOrderPath()
	if err != nil {
		return memo
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return memo
	}
	// A corrupt memo is indistinguishable from no memo for our purposes: both
	// mean "no hint", and the probe loop behaves exactly as it did before.
	var parsed certOrderMemo
	if err := json.Unmarshal(data, &parsed); err != nil || parsed.Hosts == nil {
		return memo
	}
	return parsed
}

// preferredCertOrg returns the organisation ID that last authenticated against
// host, if one is remembered.
func preferredCertOrg(host string) (int, bool) {
	if host == "" {
		return 0, false
	}
	certOrderMu.Lock()
	defer certOrderMu.Unlock()
	org, ok := loadCertOrderMemo().Hosts[host]
	return org, ok
}

// certOrderConfigLoad is a seam over config.Load so tests can supply device
// pins without touching the developer's real ~/.wendy/config.json.
var certOrderConfigLoad = config.Load

// devicePinOrg returns the organisation pinned for host, if any. The pin is
// written by enforceDevicePin after every successful mTLS connect, so its
// presence means that org genuinely authenticated here at some point.
// Best-effort: an unreadable config is simply "no hint".
func devicePinOrg(host string) (int, bool) {
	if host == "" {
		return 0, false
	}
	cfg, err := certOrderConfigLoad()
	if err != nil || cfg == nil {
		return 0, false
	}
	pin, ok := cfg.DevicePinFor(host)
	if !ok || pin.OrgID == 0 {
		return 0, false
	}
	return pin.OrgID, true
}

// preferredCertOrgForHost is the pre-dial org hint, drawn from two local
// records of what has actually authenticated against host, most specific first:
//
//  1. certorder.json — the org that last authenticated against this exact host
//     string. Most recent and most precise, but it lives in the cache directory
//     (wipeable) and keys on the raw host, so "orin" and "orin.local" are
//     separate entries.
//  2. config.DevicePins — the org+cloud pin enforceDevicePin records on every
//     successful mTLS connect. It lives in the config directory, so it survives
//     a cache wipe, and its key is normalised ("orin.local" → "orin"), so a
//     dial by either spelling benefits from a connect made via the other.
//
// Both describe an org that provably authenticated, which is why they outrank
// the advertised mDNS orgid dialAgentLKG rotates by. Like every hint on this
// path it is pure ordering: a wrong answer costs exactly what no answer costs.
func preferredCertOrgForHost(host string) (int, bool) {
	if org, ok := preferredCertOrg(host); ok {
		return org, true
	}
	return devicePinOrg(host)
}

// rememberCertOrg records that org authenticated against host. Best-effort:
// the memo is a cache, so a write failure only costs the next command the
// wasted probe it would have paid anyway.
func rememberCertOrg(host string, org int) {
	if host == "" {
		return
	}
	certOrderMu.Lock()
	defer certOrderMu.Unlock()

	memo := loadCertOrderMemo()
	if existing, ok := memo.Hosts[host]; ok && existing == org {
		// Already correct — skip the write so the common (warm) path doesn't
		// touch the disk on every command.
		return
	}
	memo.Hosts[host] = org

	path, err := certOrderPath()
	if err != nil {
		return
	}
	data, err := json.Marshal(memo)
	if err != nil {
		return
	}
	// Write via a temp file + rename so a crash mid-write can't leave a
	// truncated memo behind.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".certorder-*.json")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
	}
}

// promoteOrgNext moves the first not-yet-probed cert belonging to org to the
// position immediately after pos in order, so it is probed next instead of
// whenever its original position came up. order holds indices into certs and is
// rewritten in place; the entries at and before pos (already probed) are
// untouched, and nothing is dropped — every cert is still tried.
//
// This is the correction the dial ladder applies when the device's own server
// certificate names its org (see dialAgentLadderWithCerts), which is what saves
// an agent too old to advertise an mDNS `orgid` TXT record from a full linear
// scan on a cold memo.
//
// Reports whether it acted on the hint. False means org names nothing worth
// trying next: either we hold no cert for that org, or the only ones we hold
// were already probed — the same-org failure case (clock skew, expired cert),
// where reordering changes nothing and the caller's existing rejection
// diagnostics should stand.
func promoteOrgNext(order []int, pos int, certs []config.CertificateInfo, org int32) bool {
	return devices.PromoteOrgNext(order, pos, sdkCredentials(certs), org)
}

// orderCertsByOrg returns certs with those belonging to org moved to the
// front, preserving the relative order within both groups.
//
// Returns the input untouched when there is no hint or nothing matches, so the
// caller's probe order is unchanged in exactly the cases where the memo has
// nothing useful to say. The slice is copied rather than sorted in place: the
// caller's slice comes from loadAllCLICerts and is indexed elsewhere.
func orderCertsByOrg(certs []config.CertificateInfo, org int, have bool) []config.CertificateInfo {
	return cliCredentials(certs, devices.OrderCertsByOrg(sdkCredentials(certs), org, have))
}
