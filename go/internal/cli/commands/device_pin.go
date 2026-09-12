package commands

import (
	"fmt"
	"os"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
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
	// assetID is the device's cloud asset id from the verified server cert's
	// "urn:wendy:org:<org>:asset:<assetID>" SAN. Empty when the agent's
	// certificate carries no asset identity (legacy certs).
	assetID string
}

// observeDeviceIdentity reads what conn proved about the device it reached.
func observeDeviceIdentity(conn *grpcclient.AgentConnection) observedDeviceIdentity {
	if conn == nil || !conn.IsMTLS || conn.CertInfo == nil {
		return observedDeviceIdentity{}
	}
	obs := observedDeviceIdentity{mTLS: true, orgID: conn.CertInfo.OrganizationID}
	// Only an "asset" entity is a device; a "user" URN on a server cert would
	// be a misissued certificate, and pinning it would be meaningless.
	if id, ok := conn.ObservedServerIdentity(); ok && id.EntityType == "asset" {
		obs.assetID = id.EntityID
	}
	return obs
}

// enforceDevicePin checks the (organisation, cloud host, asset) pin for a
// freshly connected device (WDY-1149) and records or challenges it.
func enforceDevicePin(hostname string, conn *grpcclient.AgentConnection) error {
	if conn == nil {
		return nil
	}
	// No key, no pin: the caller reached something whose address is not an
	// identity, such as a VM behind a loopback forward.
	if hostname == "" {
		return nil
	}
	return enforceDeviceIdentity(hostname, observeDeviceIdentity(conn))
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
// a config read/write failure never blocks an already-verified connection.
func enforceDeviceIdentity(hostname string, obs observedDeviceIdentity) error {
	cfg, err := config.Load()
	if err != nil {
		return nil
	}

	if !obs.mTLS {
		return challengeUnprovisionedDevice(cfg, hostname)
	}

	cloud := cloudGRPCForOrg(cfg, obs.orgID)
	switch cfg.EvaluateDevicePin(hostname, obs.orgID, cloud, obs.assetID) {
	case config.PinMatch:
		return nil
	case config.PinFirstUse, config.PinAdoptAsset:
		// PinAdoptAsset is a pin written before asset ids were recorded: org and
		// cloud already match, so this is a silent upgrade, not a challenge.
		cfg.SetDevicePin(hostname, obs.orgID, cloud, obs.assetID)
		_ = config.Save(cfg)
		return nil
	default: // config.PinMismatch
		prev, _ := cfg.DevicePinFor(hostname)
		return refuseDevicePin(devicePinDiagnostic{
			hostname: hostname,
			heading:  fmt.Sprintf("Connection blocked: device %q identity changed.", hostname),
			details: fmt.Sprintf("Saved: organization %d via %s%s\nNow:   organization %d via %s%s",
				prev.OrgID, displayCloud(prev.CloudGRPC), assetSuffix(prev.AssetID),
				obs.orgID, displayCloud(cloud), assetSuffix(obs.assetID)),
		})
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
	command := "wendy device unpin " + d.hostname
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
	cfg, err := config.Load()
	if err != nil {
		return
	}
	cleared := clearPinsGoverning(cfg, hostname)
	printClearedPins(os.Stderr, cleared)
	// The SPKI half flushes itself, so only a config-store clear needs a save.
	if !clearedAnyConfigPin(cleared) {
		return
	}
	_ = config.Save(cfg)
}

func assetSuffix(assetID string) string {
	if assetID == "" {
		return ""
	}
	return fmt.Sprintf(", asset %s", assetID)
}
