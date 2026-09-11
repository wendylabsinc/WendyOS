package commands

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
)

// The two stores an unpin can clear, as they are named in its report. They are
// keyed differently on purpose — one by the name the user dials, one by the
// certificate's own identity URN — so a report that did not say which store an
// entry came from would not tell the user what they had just given up.
const (
	clearedConfigPin   = "device pin"
	clearedIdentityPin = "certificate key pin"
)

// clearedPin is one pin an unpin removed.
//
// It exists so the command can print what it did. Clearing is not a single
// keyed delete: one unpin can reach several config keys and several SPKI
// entries, and the version of this code that did that silently is what let an
// over-broad clear (a hostile discovery-cache alias dragging another device's
// pins along) go unnoticed. Anything removed gets named.
type clearedPin struct {
	// store is which of the two pin stores the entry came from.
	store string
	// key is the key the entry was filed under: a hostname for a config pin,
	// an identity URN for an SPKI pin.
	key string
	// identity is the URN the entry names, or "" for a legacy config pin with
	// no asset id — which names an organisation, not a device.
	identity string
}

// newDeviceUnpinCmd is the escape hatch every identity refusal in
// enforceDeviceIdentity, challengeUnprovisionedDevice, and spkiRefusal points
// at by name. Until this command exists, hitting one of those refusals is a
// dead end: the CLI tells the user to run it, and there is nothing to run.
//
// It takes either name a refusal can print, because the two refusals do not
// print the same kind of name. A config-pin refusal names a hostname; an SPKI
// refusal can only name the certificate identity URN the SPKI store is keyed
// by, and for the dials that reach that store there is often no hostname to
// offer — `wendy device list` and the picker dial the IP first, and nothing in
// local state ties an IP to an asset URN. Accepting the URN is what makes those
// refusals recoverable without hand-editing known_devices.json.
//
// Unpinning clears local trust state only. It never dials the device — the
// whole point is to work when the device is offline, wiped, or gone, which is
// exactly when a pin most needs clearing.
func newDeviceUnpinCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unpin <hostname|identity-urn>",
		Short: "Clear the recorded identity pin for a device",
		Long: "Clear the recorded identity pin for a device, so the next connection to it\n" +
			"records a fresh identity instead of being challenged against the old one.\n" +
			"Accepts either the hostname you connect to or the identity URN a refusal\n" +
			"prints (urn:wendy:org:<org>:asset:<id>).\n" +
			"Use this after a legitimate reflash, factory reset, or re-enrollment —\n" +
			"anything that made 'wendy device unpin' the CLI's own suggestion.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := strings.TrimSpace(args[0])

			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			var cleared []clearedPin
			// Detect the URN form by parsing it, not by guessing at its shape: a
			// hostname is never six colon-separated fields beginning
			// "urn:wendy:org", and certs owns what a well-formed identity is.
			if identity, urnErr := certs.ParseIdentityURN(target); urnErr == nil {
				cleared = clearPinsForIdentity(cfg, identity)
			} else {
				// pinKeyForAddr, not the raw argument: a pin recorded via
				// `--device host.local:50051` files under "host.local" (the port
				// stripped), and a user unpinning must be able to hand back exactly
				// what they used to connect. This is the same bug fixed in
				// set-default in Task 6 — do not reintroduce it here.
				cleared = clearPinsGoverning(cfg, pinKeyForAddr(target))
			}

			if err := config.Save(cfg); err != nil {
				return fmt.Errorf("saving config: %w", err)
			}

			// A prepared session broker may retain an authenticated transport
			// to exactly the device whose trust was just revoked. Unpublish
			// them all: brokers are a per-connect optimization and rebuild on
			// the next dial, so collateral invalidation of other devices'
			// brokers costs one handshake each, while a stale one surviving an
			// unpin would outlive the user's trust decision.
			_ = sessionbroker.InvalidateAll()

			printClearedPins(cmd.OutOrStdout(), cleared)
			fmt.Fprintf(cmd.OutOrStdout(), "Unpinned %q. The next connection to it will record a fresh identity.\n", target)
			return nil
		},
	}
}

// printClearedPins reports every entry an unpin removed, one line each.
//
// Unpinning one name can legitimately clear several entries, and the user is
// the only one who can tell "that is the device I meant" from "that is not my
// device". Printing nothing when nothing matched is also the answer to
// "unpinning something already unpinned" — the command still exits 0, it just
// has nothing to report.
func printClearedPins(w io.Writer, cleared []clearedPin) {
	for _, c := range cleared {
		identity := c.identity
		if identity == "" {
			identity = "no recorded identity"
		}
		fmt.Fprintf(w, "Cleared %s %q (%s)\n", c.store, c.key, identity)
	}
}

// clearPinsForIdentity clears everything filed under one certificate identity:
// the SPKI entry keyed by that URN, plus any config pin that derives the same
// URN. Both halves matter — clearing only the SPKI entry would leave the config
// pin behind to refuse the next dial under a different message, which is the
// same dead end from the other direction, and the two stores drifting apart is
// what makes an escape hatch feel unreliable.
//
// Unlike the hostname path there is no aliasing to reason about: the URN IS the
// identity, so "every pin naming this identity" is exactly the right blast
// radius and no discovery-derived data is consulted at all.
func clearPinsForIdentity(cfg *config.Config, identity certs.WendyIdentity) []clearedPin {
	key := identity.IdentityKey()
	var cleared []clearedPin
	for _, host := range configPinKeysForIdentity(cfg, key) {
		cfg.ClearDevicePin(host)
		cleared = append(cleared, clearedPin{store: clearedConfigPin, key: host, identity: key})
	}
	return append(cleared, removeSPKIPins([]string{key})...)
}

// configPinKeysForIdentity lists the config pin keys whose pin names
// identityKey, sorted so the command's report is deterministic.
func configPinKeysForIdentity(cfg *config.Config, identityKey string) []string {
	if cfg == nil || identityKey == "" {
		return nil
	}
	var keys []string
	for host, pin := range cfg.DevicePins {
		if configPinIdentityKey(pin) == identityKey {
			keys = append(keys, host)
		}
	}
	sort.Strings(keys)
	return keys
}

// clearPinsGoverning drops the pins that govern an identity refusal for pinKey:
// the pin filed under the refusal's exact key when present, otherwise the pin
// found under a device alias, plus same-identity aliases and their SPKI entries.
// It returns what it removed and mutates cfg; saving is the caller's job.
//
// It clears more than the single key lookupPin would return because "unpin" has
// to terminate. A device pinned under both its mDNS hostname and the cloud
// roster's asset name would otherwise refuse under one pin, be unpinned, and
// refuse again under the other — a user watching that happen learns the command
// does not work.
//
// The bound on that: an alias key is cleared only when its pin names the SAME
// (org, asset) as the governing pin. The alias list comes from the discovery
// cache, which is filled verbatim from unauthenticated mDNS TXT records, so an
// attacker who spoofs the hostname a user is about to unpin can put any
// displayname or mesh name they like in it — including another device's. Under
// the previous rule (clear every candidate) that made `wendy device unpin
// thor.local` delete the victim's pin and SPKI entry too, turning the escape
// hatch into a downgrade primitive: the victim's next connect became a
// permissive first use. Requiring the alias's pin to name the same device is
// what makes a hostile alias inert — it names a pin for a different asset, so
// it is not this device's pin, so it is left alone.
//
// A pin with no asset id is never matched as an alias. It names an
// organisation, not a device, so "same identity" cannot be established for it,
// and two unrelated legacy pins in one org must not be treated as one device.
// The governing key itself is always cleared, so this only ever costs a second
// unpin in the pathological case, never the escape hatch.
//
// SPKI entries are derived from the pins actually cleared, plus the discovery
// cache's identity when there is no config pin at all. The second source is
// what covers the store that gets written where no config pin ever does —
// getAgentVersionAtAddress runs the ladder with the SPKI store for every device
// the mDNS prober enumerates, so `wendy device list` alone leaves SPKI entries
// with no config pin behind them. When a config pin does exist, the cache is
// believed only where it agrees with that pin: a disagreeing cache is either
// stale or hostile, and in neither case does it name this device.
func clearPinsGoverning(cfg *config.Config, pinKey string) []clearedPin {
	if cfg == nil || pinKey == "" {
		return nil
	}

	// Prefer a pin filed directly under the argument. Post-connect identity
	// enforcement names that exact key in its refusal, so resolving through a
	// higher-precedence cloud alias here can clear the new device while leaving
	// the stale pin that raised the refusal untouched. Only fall back to alias
	// lookup when the named key has no pin of its own (the cloud-roster-only
	// case).
	governing, pinned := cfg.DevicePinFor(pinKey)
	governingKey := pinKey
	if !pinned {
		governing, governingKey, pinned = lookupPin(cfg, pinKey)
	}

	var cleared []clearedPin
	var identityKeys []string
	for _, key := range pinCandidateKeys(pinKey) {
		pin, ok := cfg.DevicePinFor(key)
		if !ok {
			continue
		}
		isGoverning := normalizeMDNSHost(key) == normalizeMDNSHost(governingKey)
		if !isGoverning && !sameConfigPinIdentity(pin, governing) {
			continue
		}
		cfg.ClearDevicePin(key)
		// Read the pin BEFORE it is gone: it is the only place the (org, asset)
		// pair needed to compute the SPKI store's key lives. A legacy pin (no
		// AssetID) has no derivable key — not a failure, just nothing to remove.
		identity := configPinIdentityKey(pin)
		cleared = append(cleared, clearedPin{store: clearedConfigPin, key: key, identity: identity})
		if identity != "" {
			identityKeys = append(identityKeys, identity)
		}
	}

	if key := cachedIdentityKey(pinKey); key != "" && (!pinned || key == configPinIdentityKey(governing)) {
		identityKeys = append(identityKeys, key)
	}

	return append(cleared, removeSPKIPins(identityKeys)...)
}

// configPinIdentityKey is the SPKI store key a config pin names, or "" for a
// legacy pin written before asset ids were recorded — which names an
// organisation and a cloud, not a device, and so identifies no SPKI entry.
func configPinIdentityKey(pin config.DevicePin) string {
	if pin.AssetID == "" {
		return ""
	}
	return certs.WendyIdentity{
		OrgID:      int32(pin.OrgID),
		EntityType: "asset",
		EntityID:   pin.AssetID,
	}.IdentityKey()
}

// sameConfigPinIdentity reports whether two config pins name the same device —
// same organisation and same asset. Pins with no asset id never match, not even
// each other: they constrain an org, and treating two of them as one device is
// how an unpin reaches a pin the user never named.
func sameConfigPinIdentity(a, b config.DevicePin) bool {
	key := configPinIdentityKey(a)
	return key != "" && key == configPinIdentityKey(b)
}

// cachedIdentityKey derives the SPKI store's key for pinKey from the discovery
// cache, which records the asset and org a device advertised. It is the only
// lookup that reaches an SPKI entry belonging to a host with no asset-bearing
// config pin — the `wendy device list` case above, where nothing else in local
// state ties the hostname the user types to the asset URN the store is keyed
// by.
//
// What it returns is NOT trustworthy: Cache.Replace writes mDNS TXT records
// verbatim, so an attacker on the LAN chooses this value for any hostname they
// can answer for. It is safe only because of how the caller uses it — as a
// candidate that must agree with the governing config pin before anything is
// deleted, and as a sole source only when there is no config pin to disagree
// with. An earlier version of this comment claimed the worst case was "removing
// a pin the user was already asking to remove"; that was wrong, because it
// assumed the advertised asset belonged to the device being unpinned, and the
// whole point of spoofing it is that it does not.
//
// Returns "" when the cache is unavailable, has no entry, or the entry carries
// no asset identity.
func cachedIdentityKey(pinKey string) string {
	entry, ok := cachedDeviceHostEntry(pinKey)
	if !ok || entry.AssetID <= 0 || entry.OrgID <= 0 {
		return ""
	}
	return certs.WendyIdentity{
		OrgID:      entry.OrgID,
		EntityType: "asset",
		EntityID:   strconv.Itoa(int(entry.AssetID)),
	}.IdentityKey()
}

// removeSPKIPins drops the given identity keys from the SPKI pin store and
// reports the ones that were actually there. Duplicates are collapsed, so a key
// reached twice (a config pin and the cache agreeing, as they should) is
// reported once.
//
// Best-effort: an unopenable store leaves the config side of the unpin done,
// which is strictly better than failing the command outright.
func removeSPKIPins(identityKeys []string) []clearedPin {
	if len(identityKeys) == 0 {
		return nil
	}
	// Same config-directory resolution as openPinStore, but that helper returns
	// certs.PinChecker, which only exposes CheckAndUpdate — not Remove. Open the
	// concrete store directly.
	dir, err := config.ConfigDir()
	if err != nil {
		return nil
	}
	store, err := devicepin.Open(dir)
	if err != nil {
		return nil
	}
	var cleared []clearedPin
	seen := map[string]bool{}
	for _, key := range identityKeys {
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		// Has before Remove: Remove reports success for a key that was never
		// there, and reporting entries we did not remove would make the command's
		// own account of its blast radius useless.
		if !store.Has(key) {
			continue
		}
		if err := store.Remove(key); err != nil {
			continue
		}
		cleared = append(cleared, clearedPin{store: clearedIdentityPin, key: key, identity: key})
	}
	return cleared
}

// clearedAnyConfigPin reports whether a clear touched the config store, which
// is the only half that needs cfg written back — the SPKI store flushes itself.
func clearedAnyConfigPin(cleared []clearedPin) bool {
	for _, c := range cleared {
		if c.store == clearedConfigPin {
			return true
		}
	}
	return false
}
