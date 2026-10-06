package commands

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/flock"
)

// cloudGRPCForOrg returns the cloud gRPC endpoint of the auth session that owns
// a certificate for orgID, or "" if none is found. It maps the org carried by
// the verifying mTLS cert back to the cloud host that issued it.
func cloudGRPCForOrg(cfg *config.Config, orgID int) string {
	for _, auth := range cfg.Auth {
		for _, c := range auth.Certificates {
			if c.OrganizationID == orgID {
				return auth.CloudGRPC
			}
		}
	}
	return ""
}

func displayCloud(c string) string {
	if c == "" {
		return "an unknown cloud"
	}
	return c
}

// observedDeviceIdentity is what a live connection actually proved about the
// device answering at a hostname. All of it comes from a certificate the CLI
// verified, or — when mTLS is false — from the absence of any certificate at
// all, which is itself the signal enforceDeviceIdentity acts on.
type observedDeviceIdentity struct {
	// mTLS is true when the connection was authenticated. False means the
	// device answered unauthenticated: it is not provisioned.
	mTLS bool
	// orgID is the organisation of the CLI certificate that authenticated.
	orgID int
	// assetID is the device's asset id from the verified server cert — the
	// name of its tenant SPIFFE principal, or the legacy
	// "urn:wendy:org:<org>:asset:<assetID>" SAN on an old chain. Empty when the
	// agent's certificate carries no asset identity.
	assetID string
	// principal is the full tenant SPIFFE principal, when the cert carries one.
	// Recorded into the pin so an unpin can reach the SPKI entry it keys.
	principal string
}

// observeDeviceIdentityFn is a seam over observeDeviceIdentity: only a real
// TLS handshake can set the verified server identity it reads, so a test of
// the paths that enforce a pin stubs the reading instead.
var observeDeviceIdentityFn = observeDeviceIdentity

// observeDeviceIdentity reads what conn proved about the device it reached.
func observeDeviceIdentity(conn *grpcclient.AgentConnection) observedDeviceIdentity {
	if conn == nil || !conn.IsMTLS || conn.CertInfo == nil {
		return observedDeviceIdentity{}
	}
	obs := observedDeviceIdentity{mTLS: true, orgID: conn.CertInfo.OrganizationID}
	// Only an "asset" entity is a device; a "user" URN on a server cert would
	// be a misissued certificate, and pinning it would be meaningless.
	if id, ok := conn.ObservedServerIdentity(); ok && id.EntityType == certs.EntityAsset {
		obs.assetID = id.EntityID
		obs.principal = id.Principal
	}
	return obs
}

// enforceDevicePin checks the (organisation, cloud host, asset) pin for a
// freshly connected device (WDY-1149) and records or challenges it.
func enforceDevicePin(hostname string, conn *grpcclient.AgentConnection) error {
	return enforceDevicePinAt(hostname, "", conn)
}

// enforceDevicePinAt is enforceDevicePin for a connection a front door dialled
// at dialAddr, the address the user typed ("" for one some fallback
// substituted for the dial). The address matters only for a connection judged
// under a running VM's vm:<name> key at its literal 127.0.0.1 forward: its
// accepted identity is then also recorded under the bare 127.0.0.1 key, as
// main recorded the same typed connection (see typedVMBarePinKey and
// recordTypedVMBarePin). Every other connection ignores it.
func enforceDevicePinAt(hostname, dialAddr string, conn *grpcclient.AgentConnection) error {
	if conn == nil {
		return nil
	}
	// No key, no pin: the caller reached something whose address is not an
	// identity, such as a VM behind a loopback forward.
	if hostname == "" {
		return nil
	}
	return enforceDeviceIdentityAt(hostname, typedVMBarePinKey(hostname, dialAddr), observeDeviceIdentityFn(conn))
}

// enforceDeviceIdentity compares what a connection proved about a device
// against the pin recorded for its hostname:
//
//   - first use   → record the pin, proceed
//   - match       → proceed; a renewed or re-enrolled cert for the same
//     organisation + cloud + asset is expected and never challenged
//   - legacy pin  → backfill the observed asset id, proceed
//   - mismatch    → explain what changed and refuse
//   - unprovisioned, but pinned → same refusal: a device we have seen enrolled
//     answering with no identity at all has been reflashed, factory reset, or
//     replaced by something squatting its name
//
// The two refusals are unconditional and read identically in interactive, JSON,
// and non-interactive modes. There is deliberately no "trust this anyway?"
// prompt: a man-in-the-middle warning that can be dismissed gets dismissed, and
// the one person who can tell a legitimate replacement from an attack is not
// the one staring at a prompt mid-command. `wendy device unpin <host>` is the
// deliberate, separate act that resolves it.
//
// A device with no pin that answers unprovisioned is the ordinary
// out-of-the-box case and passes silently. It is best-effort about local state:
// a config read/write/lock failure never blocks an already-verified connection
// — and never skips the check either (see the read-only fallback below).
func enforceDeviceIdentity(hostname string, obs observedDeviceIdentity) error {
	return enforceDeviceIdentityAt(hostname, "", obs)
}

// enforceDeviceIdentityAt is enforceDeviceIdentity for a typed connection to a
// running VM: an accepted identity is also recorded under bareKey (see
// recordTypedVMBarePin), in the same locked update. An empty bareKey records
// nothing extra.
func enforceDeviceIdentityAt(hostname, bareKey string, obs observedDeviceIdentity) error {
	var refusal error
	judged := false
	// Under the config lock so a pin recorded here cannot be reverted by, or
	// revert, another wendy process's concurrent write.
	updateErr := config.Update(func(cfg *config.Config) (bool, error) {
		judged = true
		changed, err := applyDeviceIdentity(cfg, hostname, bareKey, obs)
		refusal = err
		return changed && err == nil, nil
	})
	if judged {
		return refusal
	}
	// Update never reached the check: the lock could not be taken (a read-only
	// config dir, a hung wendy process) or the config could not be read.
	// Recording a pin needs the lock; judging one must not, or an unwritable
	// ~/.wendy would switch enforcement off. Judge what can be read.
	cfg, err := config.Load()
	if err != nil {
		return nil
	}
	before := maps.Clone(cfg.DevicePins)
	changed, refusal := applyDeviceIdentity(cfg, hostname, bareKey, obs)
	switch decideFallbackAction(updateErr, changed, refusal) {
	case fallbackWarnUnrecorded:
		// Another wendy process holds config.lock — hung, or just busy. Writing
		// without the lock risks reverting that process's own change, so this
		// verdict is judged but never recorded here. Say so, naming every key
		// it would have written: without a warning, the next connection there
		// would silently treat this as a first use (or a swap) all over again.
		fmt.Fprint(os.Stderr, unrecordedIdentityWarning(hostname, unrecordedPinKeys(before, cfg.DevicePins), updateErr))
	case fallbackSaveUnlocked:
		// The lock file itself could not be opened or created (a read-only or
		// foreign-owned config dir), or Update's own Load failed after taking
		// the lock. No process can be holding a lock that could not even be
		// opened, so this is exactly main's pre-lock behaviour: an unlocked
		// best-effort save that can lose to a concurrent writer but never
		// accepts anything. Skipping it here would silently reopen the
		// trust-on-first-use window main did not have.
		_ = config.Save(cfg)
	}
	return refusal
}

// unrecordedPinKeys lists, sorted, the pin keys a verdict changed in after
// that are not so in before: what the lock fallback judged but could not
// record.
func unrecordedPinKeys(before, after map[string]config.DevicePin) []string {
	var keys []string
	for key, pin := range after {
		if prev, ok := before[key]; !ok || prev != pin {
			keys = append(keys, key)
		}
	}
	for key := range before {
		if _, ok := after[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// unrecordedIdentityWarning is the lock fallback's warning that a verdict was
// judged but not recorded. It names every key that verdict would have written
// — for a typed connection to a running VM that can be the bare 127.0.0.1
// alone, or beside vm:<name> — and hostname only when it has no list.
func unrecordedIdentityWarning(hostname string, keys []string, updateErr error) string {
	if len(keys) == 0 {
		keys = []string{hostname}
	}
	quoted := make([]string, len(keys))
	for i, key := range keys {
		quoted[i] = strconv.Quote(key)
	}
	return fmt.Sprintf("wendy: device identity for %s was not recorded: another wendy process holds the config lock (%v)\n", strings.Join(quoted, " and "), updateErr)
}

// fallbackAction is what enforceDeviceIdentity's read-only fallback does with
// a verdict config.Update could not reach.
type fallbackAction int

const (
	// fallbackNothing covers a refusal (already returned as-is; nothing to
	// save) and a verdict with nothing to record at all.
	fallbackNothing fallbackAction = iota
	// fallbackWarnUnrecorded is a verdict that would have written a pin, lost
	// because another wendy process holds config.lock.
	fallbackWarnUnrecorded
	// fallbackSaveUnlocked is a verdict that would have written a pin, tried
	// unlocked because the lock itself could not be taken by anyone.
	fallbackSaveUnlocked
)

// decideFallbackAction chooses what enforceDeviceIdentity's read-only fallback
// does with applyDeviceIdentity's verdict, given the error config.Update
// returned before it could reach that verdict. It is pure so the branch that
// distinguishes "another process holds the lock" from "the lock could not be
// taken at all" is testable without a real 10s lock-contention wait: the
// timeout shape itself is covered by config's own
// TestUpdateGivesUpWhileAnotherProcessHoldsTheLock.
//
// changed && refusal == nil is the only case with anything to lose — a
// refusal is returned exactly as it would have been under the lock, and
// "nothing to save" needs nothing done either way.
func decideFallbackAction(updateErr error, changed bool, refusal error) fallbackAction {
	if !changed || refusal != nil {
		return fallbackNothing
	}
	if errors.Is(updateErr, flock.ErrTimeout) {
		return fallbackWarnUnrecorded
	}
	return fallbackSaveUnlocked
}

// applyDeviceIdentity is enforceDeviceIdentity's decision, made against cfg and
// recorded into it. changed reports whether cfg must be saved; refusal is the
// error to return when the device must not be used (cfg is then unchanged).
//
// The decision is made under hostname alone. An accepted mTLS judgement then
// also records the identity under bareKey, when there is one — which can only
// add a pin or adopt an asset, never refuse (see recordTypedVMBarePin).
func applyDeviceIdentity(cfg *config.Config, hostname, bareKey string, obs observedDeviceIdentity) (changed bool, refusal error) {
	if !obs.mTLS {
		return false, challengeUnprovisionedDevice(cfg, hostname)
	}

	cloud := cloudGRPCForOrg(cfg, obs.orgID)
	switch cfg.EvaluateDevicePin(hostname, obs.orgID, cloud, obs.assetID) {
	case config.PinMatch:
		// Backfill the principal into a pin that matches but predates the SPIFFE
		// cutover. Same silent upgrade as PinAdoptAsset: nothing about the trust
		// decision changes, the pin just gains the key an unpin needs to find
		// the device's SPKI entry.
		if prev, ok := cfg.DevicePinFor(hostname); ok && prev.Principal == "" && obs.principal != "" {
			cfg.SetDevicePinFrom(hostname, prev.OrgID, prev.CloudGRPC, prev.AssetID, obs.principal, cfg.PinSource(hostname))
			changed = true
		}
	case config.PinFirstUse, config.PinAdoptAsset:
		// PinAdoptAsset is a pin written before asset ids were recorded: org and
		// cloud already match, so this is a silent upgrade, not a challenge.
		cfg.SetDevicePin(hostname, obs.orgID, cloud, obs.assetID, obs.principal)
		changed = true
	default: // config.PinMismatch
		prev, _ := cfg.DevicePinFor(hostname)
		return false, refuseDevicePin(devicePinDiagnostic{
			hostname: hostname,
			heading:  fmt.Sprintf("Connection blocked: device %q identity changed.", hostname),
			details: fmt.Sprintf("Saved: organization %d via %s%s\nNow:   organization %d via %s%s",
				prev.OrgID, displayCloud(prev.CloudGRPC), assetSuffix(prev.AssetID),
				obs.orgID, displayCloud(cloud), assetSuffix(obs.assetID)),
		})
	}
	if recordTypedVMBarePin(cfg, bareKey, obs, cloud) {
		changed = true
	}
	return changed, nil
}

// recordTypedVMBarePin records an identity just accepted under a running VM's
// vm:<name> key, for a connection typed at the VM's 127.0.0.1 forward, under
// bareKey — the bare 127.0.0.1 main judged and pinned that same typed
// connection under (see typedVMBarePinKey). Once the VM stops, pinKeyForAddr
// keys its address as the bare host again, and it is then as pinned as main
// left it. obs is what the connection proved; cloud is its organisation's
// cloud host.
//
// It never refuses — the connection's own key has already judged it, and a
// VM's key never consults the bare pin — and it writes exactly what main's
// connection wrote there, where main's would not have refused:
//
//   - no bare pin (first use), or an org-only bare pin in the same
//     organisation and cloud with a certificate naming its asset (adopt): the
//     observed identity is filed, as main's SetDevicePin filed it (an
//     adoptable pin is never cloud-sourced, so its source stays LAN);
//   - a pin that matches, or that names a different identity: nothing. It
//     keeps governing every other loopback port, and this address once the VM
//     stops, exactly as it did.
//
// So it never overwrites a pin naming another device and never moves or clears
// anything.
func recordTypedVMBarePin(cfg *config.Config, bareKey string, obs observedDeviceIdentity, cloud string) bool {
	if bareKey == "" {
		return false
	}
	switch cfg.EvaluateDevicePin(bareKey, obs.orgID, cloud, obs.assetID) {
	case config.PinFirstUse, config.PinAdoptAsset:
		cfg.SetDevicePin(bareKey, obs.orgID, cloud, obs.assetID, obs.principal)
		return true
	default: // config.PinMatch, config.PinMismatch
		return false
	}
}

// devicePinDiagnostic puts intentional unenrollment and organization changes
// next to the recovery command. The same information is kept in Error() for
// MCP and other callers; only the CLI presentation adds styling.
type devicePinDiagnostic struct {
	hostname   string
	heading    string
	details    string
	checkLogin bool
}

func (d devicePinDiagnostic) message(styled bool) string {
	heading := d.heading
	command := "wendy device unpin " + shellQuoteArg(d.hostname)
	details := d.details
	if styled {
		heading = tui.ErrorMessage(heading)
		command = tui.Command(command)
		detailLines := strings.Split(details, "\n")
		for i, line := range detailLines {
			detailLines[i] = tui.Dim(line)
		}
		details = strings.Join(detailLines, "\n")
	}
	lines := []string{
		heading,
		"This CLI still remembers its previous enrollment (a local pin).",
		"",
		"If you intentionally unenrolled, reset, reflashed, or changed this device's organization:",
		"  " + command,
		"Then retry your command. Unpinning only clears this CLI's saved device identity.",
		"",
		"If this change was unexpected, keep the pin and verify the device first.",
	}
	if d.checkLogin {
		lines = append(lines, "Missing organization credentials? Run 'wendy auth login', then retry.")
	}
	return strings.Join(append(lines, "", details), "\n")
}

func refuseDevicePin(diagnostic devicePinDiagnostic) error {
	err := refuseIdentity("%s", diagnostic.message(false))
	err.diagnostic = &diagnostic
	return err
}

// CLIMessage is rendered once by the CLI entry point, including when wrapped.
// Rendering here avoids painting the recovery instructions red along with the
// heading, or printing a second copy while the error is propagated.
func (e *deviceIdentityRefusalError) CLIMessage() string {
	if e.diagnostic == nil {
		return tui.ErrorMessage(e.Error())
	}
	return e.diagnostic.message(true)
}

// challengeUnprovisionedDevice handles a connection with no verifiable identity
// at all. Only a hostname we have previously seen enrolled is challenged: for
// everything else, connecting to an unprovisioned device is the normal
// out-of-the-box flow.
//
// This case cannot be folded into EvaluateDevicePin — there is no observed
// identity to compare — but it is the same trust question, and it gets the same
// unconditional answer: the device that vouched for this hostname is not the one
// answering now, so the connection does not happen.
func challengeUnprovisionedDevice(cfg *config.Config, hostname string) error {
	prev, pinned := cfg.DevicePinFor(hostname)
	if !pinned {
		return nil
	}

	return refuseDevicePin(devicePinDiagnostic{
		hostname: hostname,
		heading:  fmt.Sprintf("Connection blocked: device %q has no enrolled identity.", hostname),
		details: fmt.Sprintf("Saved: organization %d via %s%s\nNow:   unprovisioned (no mTLS)",
			prev.OrgID, displayCloud(prev.CloudGRPC), assetSuffix(prev.AssetID)),
		checkLogin: true,
	})
}

// clearDevicePinForRepin drops the stored pins for hostname so the next
// successful connection records a fresh one. `wendy device set-default <host>`
// calls it because naming a device on the command line is the user asserting
// they mean that device — without it, set-default's own connect would hit the
// refusals above and never reach the re-pin. It is the same operation the
// refusals point at by name (`wendy device unpin <host>`), reached through a
// different command, so it goes through the same clearPinsGoverning: pki/README
// already promises set-default has "the same clearing effect", and a version of
// it that missed the SPKI store would make that promise false for exactly the
// refusal that has no other way out. Best-effort; a config read/write failure
// just leaves the old pin in place.
//
// It reports what it cleared on stderr for the same reason unpin does on
// stdout: set-default deleting trust state is a side effect of a command whose
// name does not mention pins, and the user is the only one who can notice it
// touched something they did not mean. Stderr keeps it out of any JSON output.
func clearDevicePinForRepin(hostname string) {
	var cleared []clearedPin
	_ = config.Update(func(cfg *config.Config) (bool, error) {
		cleared = clearPinsGoverning(cfg, hostname)
		// The SPKI half flushes itself, so only a config-store clear needs a save.
		return clearedAnyConfigPin(cleared), nil
	})
	printClearedPins(os.Stderr, cleared)
}

// shellQuoteArg renders s so a copy-paste of the recovery command survives a
// POSIX shell as a single argument. d.hostname can be an mDNS display alias
// copied verbatim from unauthenticated discovery data, so a name like
// `foo; rm -rf ~` must not turn the suggested command into something else, and
// one containing spaces must still reach `device unpin` as one arg (it enforces
// ExactArgs(1)). An ordinary, unsurprising name is left bare.
func shellQuoteArg(s string) string {
	unsafe := strings.ContainsFunc(s, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return false
		default:
			return !strings.ContainsRune("-_.:/@%+=", r)
		}
	})
	if s != "" && !unsafe {
		return s
	}
	// POSIX single-quoting: everything is literal inside '...', and an embedded
	// single quote is closed, escaped, and reopened.
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func assetSuffix(assetID string) string {
	if assetID == "" {
		return ""
	}
	return fmt.Sprintf(", asset %s", assetID)
}
