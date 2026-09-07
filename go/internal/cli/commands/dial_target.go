package commands

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
)

// dialTarget carries what the ladder needs to know about *who* it is dialing,
// not just where. Before this, the ladder took a bare address, so the identity
// the user asked for was gone by the time a certificate arrived — which is
// exactly what let a spoofed mDNS answer redirect a connection to another
// same-CA host.
type dialTarget struct {
	// PinKey is the name the user asked for (--device value, saved default, or
	// the picker's device name) — never the resolved IP, which changes on
	// ordinary DHCP churn and would train users to unpin reflexively. An empty
	// PinKey disables pin enforcement for this dial.
	PinKey string
	// PinnedKey is the key the governing pin is ACTUALLY filed under, which is
	// not always PinKey: lookupPin resolves a pin across every name the device
	// answers to, so dialling "wendyos-calm-zinnia" can be governed by a pin
	// recorded under the cloud roster's "calm-zinnia". Empty when no pin
	// governs this dial.
	//
	// It exists because a refusal has to name a key `wendy device unpin` can
	// act on. Naming the dialled key when an alias holds the pin sends the user
	// to a command that clears nothing, and the next dial refuses identically —
	// a permanent dead end dressed up as an escape hatch.
	PinnedKey string
	// Addr is the host:port actually dialed, and the first of Candidates when
	// there are several. It stays the single "primary" address so every caller
	// and diagnostic that only ever wanted one address keeps working.
	Addr string
	// Candidates are every address this dial may try, in the order to try them
	// (see orderDialCandidates: IPv4, then routable IPv6, then link-local/ULA).
	// Empty means "just Addr" — read it through dialCandidates, never directly.
	//
	// One name legitimately resolves to several addresses, and on a network that
	// hands out fresh DHCP leases the first one is regularly the wrong one. The
	// ladder used to be handed a single pre-resolved address, so a device that
	// was perfectly reachable at its second address was reported as unreachable
	// — and, when pinned, reported as an identity problem.
	Candidates []string
	// Expected constrains the peer certificate. Non-nil only when a pin (or a
	// cloud-seeded value) names a specific asset.
	Expected *certs.WendyIdentity
}

// dialCandidates returns the addresses the ladder must walk, always at least
// Addr. Candidates is consulted through here so that a hand-built dialTarget
// (and every existing caller that sets only Addr) behaves exactly as it did
// before multi-address dialing existed.
func (t dialTarget) dialCandidates() []string {
	if len(t.Candidates) > 0 {
		return t.Candidates
	}
	if t.Addr == "" {
		return nil
	}
	return []string{t.Addr}
}

// refusalKey is the name a refusal for this dial must print: the key the pin is
// filed under when one governs, else the name the user asked for. Falling back
// to PinKey keeps a hand-built dialTarget (and any future caller that sets only
// PinKey) naming something rather than an empty string.
func (t dialTarget) refusalKey() string {
	if t.PinnedKey != "" {
		return t.PinnedKey
	}
	return t.PinKey
}

// pinned reports whether a pin governs this dial, answered entirely from what
// newDialTarget already resolved. A pinned host has been reached over mTLS
// before, so the ladder must not offer it the plaintext rung.
//
// It reads the target rather than re-reading pin state because one connect must
// make ONE decision about what the pin says. The guard used to call back into
// the config for a second, independent answer, which could disagree with the
// first — a cloud seeding or an unpin landing from another process mid-ladder
// would have the plaintext rung consult a pin state that never produced this
// target's Expected or refusalKey. Deriving both from the same resolution makes
// that disagreement unrepresentable.
//
// PinnedKey is non-empty for exactly the dials lookupPin found a pin for, and
// never empty when it did: the key comes from pinCandidateKeys, which drops
// empty candidates, so "a pin governs" and "we know the key it is filed under"
// are the same fact. Expected is deliberately NOT consulted — it is set only
// when a pin names an asset, so a pin without one would read as unpinned, and
// the constraint Expected carries is enforced in VerifyConnection, not here.
func (t dialTarget) pinned() bool {
	return t.PinnedKey != ""
}

// loadConfigForPinFn is a seam over config.Load for tests.
var loadConfigForPinFn = config.Load

// plaintextConnectFn is a seam over grpcclient.Connect — the ladder's last,
// unauthenticated rung — so a test can prove that rung was never reached.
var plaintextConnectFn = grpcclient.Connect

// identityMismatchFn is a seam over the ladder's reading of a wrong-device
// rejection. The flag itself can only be set by a real TLS handshake — the
// VerifyConnection sink owns the unexported field — so seaming the ladder's
// CONSUMPTION of it is what puts the abort under test without a live ML-DSA
// peer. That abort matters on its own: once a cloud-seeded Expected can exist
// for a host with no config pin, it is the only thing standing between a wrong
// device and the plaintext rung.
var identityMismatchFn = (*grpcclient.AgentConnection).IdentityMismatch

// pinMismatchFn is the same seam for the SPKI store's rejection. It is seamed
// for the same reason: only a real handshake against a device whose key rotated
// can set the flag, so testing the ladder's REACTION to it — abort, no
// plaintext rung, a message naming `wendy device unpin` — needs the read
// stubbed rather than a live ML-DSA peer with a rotated keypair.
var pinMismatchFn = (*grpcclient.AgentConnection).PinMismatch

// newDialTarget resolves the pin for pinKey and returns a target constrained by
// it. Key resolution deliberately may read discovery-derived names: choosing
// the wrong key can only ever produce a mismatch — a stricter outcome — never
// a bypass, because the trust decision itself stays on the certificate.
func newDialTarget(pinKey, addr string) dialTarget {
	return newDialTargetCandidates(pinKey, []string{addr})
}

// newDialTargetCandidates is newDialTarget for a name that resolved to several
// addresses. The pin resolution is identical and deliberately so: which device
// is acceptable is decided once, from the name the user asked for, and cannot
// vary between candidates. Only the routing differs.
func newDialTargetCandidates(pinKey string, addrs []string) dialTarget {
	target := dialTarget{PinKey: pinKey}
	if len(addrs) > 0 {
		target.Addr = addrs[0]
		if len(addrs) > 1 {
			target.Candidates = addrs
		}
	}
	pin, key, ok := governingPin(pinKey)
	if !ok {
		return target
	}
	target.PinnedKey = key
	if pin.AssetID != "" {
		target.Expected = &certs.WendyIdentity{OrgID: int32(pin.OrgID), EntityType: "asset", EntityID: pin.AssetID}
	}
	return target
}

// governingPin resolves the pin that governs pinKey across every name the
// device answers to, returning it and the key it is actually filed under. It is
// the single reader of pin state on the dial path, so the identity a dial
// enforces, the fact that it is pinned, and the key a refusal names can never
// disagree about which pin they are talking about.
func governingPin(pinKey string) (config.DevicePin, string, bool) {
	if pinKey == "" {
		return config.DevicePin{}, "", false
	}
	cfg, err := loadConfigForPinFn()
	if err != nil {
		return config.DevicePin{}, "", false
	}
	return lookupPin(cfg, pinKey)
}

// pinKeyForAddr extracts the pin key from the address a caller was asked to
// reach, BEFORE any resolution: the host as the user named it. That is the same
// key enforceDevicePin records under, so the two agree on what "this device"
// means. A resolved IP is deliberately never used as a key — it changes on
// ordinary DHCP churn — but an address the user typed as a literal IP is the
// name they asked for, so it keys a pin like any other host.
// Loopback gets no special case. It used to: local VMs all answer on 127.0.0.1,
// so two of them collide on one key. But every alternative was worse -- an
// empty key reads as "unpinned" and disarms the guard against reaching a
// previously-authenticated host over plaintext, and a port-qualified key
// orphans the pins existing users already hold under the bare host. Known VM
// aliases instead use their own vm:<name> key (see connectSimulatorAgent),
// leaving typed IP addresses governed by their existing pins.
func pinKeyForAddr(addr string) string {
	// SplitHostPort accepts non-numeric service names, so vm:dev would
	// otherwise become just "vm" when set-default/unpin derives its key.
	if name, matched, err := simulatorName(addr); err == nil && matched {
		return vmDeviceIDPrefix + name
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return strings.TrimSpace(addr)
	}
	return host
}

// isLoopbackHost reports whether host names this machine. "localhost" is
// matched by name because net.ParseIP does not resolve it, and it is the form
// people actually type at a forwarded port.
func isLoopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// expectedIdentityFor returns the asset identity pinned for pinKey, or nil when
// the host is unpinned or its pin predates asset ids. Nil means "first contact
// is permissive" — the posture that keeps legacy and unprovisioned devices
// working; the pin is written on the first successful connect.
//
// The lookup goes through lookupPin, so a pin recorded under any of the
// device's names — hostname, mesh name, or display name — is honoured here.
func expectedIdentityFor(pinKey string) *certs.WendyIdentity {
	pin, _, ok := governingPin(pinKey)
	if !ok || pin.AssetID == "" {
		return nil
	}
	return &certs.WendyIdentity{OrgID: int32(pin.OrgID), EntityType: "asset", EntityID: pin.AssetID}
}

// pinCandidateKeys returns the keys a pin for pinKey may have been recorded
// under, most-specific first: the name the caller dialed, then — from the
// discovery-cache entry whose hostname matches it — that device's mesh name and
// display name. One device answers to all three, and different surfaces record
// under different ones: enforceDevicePin records the dialed mDNS hostname
// (wendyos-calm-zinnia), cloud seeding records the asset name the roster
// carries (calm-zinnia, which is the cache's display name), and mesh dials name
// the device by its mesh name. A cloud Asset carries only {Id, Name} — no
// hostname — so the cloud side cannot record under the dial key, and the
// reconciliation has to happen here on lookup.
//
// Duplicates are dropped under the same normalisation the pin store applies, so
// an alias that is only a cosmetic variant of the dialed name never produces a
// second, redundant candidate, and empty names are never looked up.
//
// Reading discovery-derived names to pick a key is safe FOR LOOKUP in a way
// that reading them to make a trust decision would not be: consulting an extra
// candidate can only ever FIND a pin, never discard one, so an attacker-chosen
// alias can at most impose a constraint that the real device fails — a stricter
// outcome, not a bypass. The trust decision itself stays on the certificate.
//
// That justification is about lookup and does not carry to clearing. A caller
// that DELETES every candidate turns the same attacker-chosen alias into a way
// to drop another device's pin, which is a bypass — see clearPinsGoverning,
// which consumes this list but removes an alias's pin only when it names the
// same device as the governing one.
func pinCandidateKeys(pinKey string) []string {
	if pinKey == "" {
		return nil
	}
	candidates := []string{pinKey}
	// Best effort by construction: cachedDeviceHostEntry reports false for an
	// unopenable cache, an unreadable one, and a plain miss alike, and every
	// one of those degrades to exactly the single-key list above.
	entry, ok := cachedDeviceHostEntry(pinKey)
	if !ok {
		return candidates
	}
	seen := map[string]bool{normalizeMDNSHost(pinKey): true}
	for _, alias := range []string{entry.MeshName, entry.DisplayName} {
		norm := normalizeMDNSHost(alias)
		if norm == "" || seen[norm] {
			continue
		}
		seen[norm] = true
		candidates = append(candidates, alias)
	}
	return candidates
}

// lookupPin resolves the pin governing pinKey across every candidate key,
// returning it, the key it was found under, and whether there was one.
//
// A cloud-sourced pin outranks a LAN-sourced one wherever each sits in the
// candidate order: cloud learned the binding from the org's cloud over an
// authenticated session, while a LAN pin records only what some host on the
// local network presented. Among pins of equal source the earliest candidate
// wins, which keeps the dialed name authoritative over an alias.
//
// That precedence yields to one invariant: an asset-less cloud pin never
// displaces an incumbent carrying an asset id. Cloud authority decides WHICH
// binding to believe, and applying it is worth nothing if it leaves
// expectedIdentityFor with no binding to enforce — a host constrained to one
// asset would become constrained to none, which is exactly the same-CA-host
// redirect this path exists to stop. An asset-less pin is a real state, not a
// hypothetical: config's EvaluateDevicePin carries a dedicated branch for a
// cloud-sourced pin with no asset id.
//
// Because the search only ever adds keys, a host pinned under pinKey stays
// pinned no matter what the cache says or fails to say — the property that lets
// the cache be consulted best-effort without a cache outage quietly switching
// enforcement off.
func lookupPin(cfg *config.Config, pinKey string) (config.DevicePin, string, bool) {
	if cfg == nil {
		return config.DevicePin{}, "", false
	}
	var best config.DevicePin
	var bestKey string
	found := false
	for _, key := range pinCandidateKeys(pinKey) {
		pin, ok := cfg.DevicePinFor(key)
		if !ok {
			continue
		}
		if !found {
			best, bestKey, found = pin, key, true
			continue
		}
		if best.Source != config.PinSourceCloud && pin.Source == config.PinSourceCloud &&
			(pin.AssetID != "" || best.AssetID == "") {
			best, bestKey = pin, key
		}
	}
	return best, bestKey, found
}

// errDeviceIdentityRefused is what every refusal in this package answers
// errors.Is to. It exists so a caller can tell "the device you asked for is not
// what answered" apart from "nothing answered" WITHOUT matching on message
// text: the picker's Bluetooth fallback turns on exactly that distinction, and
// a fallback that silently reaches the rejected device over a second transport
// is the refusal undone.
var errDeviceIdentityRefused = errors.New("device identity refused")

// deviceIdentityRefusalError carries a refusal's full user-facing text while
// staying recognisable to errors.Is. The text is the whole message rather than
// a wrap so the refusals read exactly as they did before this type existed.
type deviceIdentityRefusalError struct{ msg string }

func (e *deviceIdentityRefusalError) Error() string { return e.msg }

func (e *deviceIdentityRefusalError) Is(target error) bool {
	return target == errDeviceIdentityRefused
}

// refuseIdentity builds a refusal that errors.Is(err, errDeviceIdentityRefused)
// recognises. Every refusal raised because the wrong device answered — here and
// in device_pin.go — must go through it.
func refuseIdentity(format string, args ...any) error {
	return &deviceIdentityRefusalError{msg: fmt.Sprintf(format, args...)}
}

// identityRefusal renders a wrong-device rejection. Same text in interactive,
// JSON, and non-interactive modes — there is deliberately no "trust this?"
// prompt, because a MITM warning that can be dismissed gets dismissed.
func identityRefusal(pinKey string, im *certs.IdentityMismatchError) error {
	got := "no wendy identity"
	if im.GotAsset != "" {
		got = fmt.Sprintf("asset %s in organization %d", im.GotAsset, im.GotOrg)
	}
	return refuseIdentity(
		"device %q is pinned to asset %s in organization %d, but the host answering presented %s; refusing to connect — if this device was legitimately replaced or re-enrolled, run 'wendy device unpin %s'",
		pinKey, im.WantAsset, im.WantOrg, got, pinKey)
}

// errNoAuthenticatedEndpoint is what a "nothing answered" refusal answers
// errors.Is to.
//
// It is deliberately NOT errDeviceIdentityRefused. That sentinel means "the
// device you asked for is not what answered", and the picker's Bluetooth
// fallback turns on exactly that distinction: a fallback that reaches a
// *rejected* device over a second transport is the refusal undone. A device
// that could not be reached over IP at all is the opposite case — it is
// precisely when trying another transport is the right thing to do — so
// filing it under the same sentinel would suppress the fallback for the one
// population that needs it.
var errNoAuthenticatedEndpoint = errors.New("no authenticated endpoint answered")

// noAuthenticatedEndpointError carries the full user-facing text while staying
// recognisable to errors.Is, mirroring deviceIdentityRefusalError.
type noAuthenticatedEndpointError struct{ msg string }

func (e *noAuthenticatedEndpointError) Error() string { return e.msg }

func (e *noAuthenticatedEndpointError) Is(target error) bool {
	return target == errNoAuthenticatedEndpoint
}

// blocksUnauthenticatedFallback reports whether err forbids reaching this device
// over a transport that enforces nothing — today the picker's Bluetooth
// fallback, where attemptBLEConnect sets no ExpectedIdentity and
// enforceSelectedDevicePin is a no-op.
//
// BOTH refusals block it, for two different reasons, and keeping them distinct
// error types is exactly why this predicate has to exist rather than the gate
// matching one sentinel:
//
//   - errDeviceIdentityRefused: the wrong device answered. Reaching it over a
//     second transport is the refusal undone.
//   - errNoAuthenticatedEndpoint: nothing authenticated answered a host we hold
//     a PIN for — meaning we have reached this device over mTLS before and know
//     it authenticates. An unauthenticated BLE peer advertising its name is not
//     evidence of being that device, and BLE checks nothing, so accepting one
//     here would be the downgrade the pin exists to prevent. The error is
//     raised only under target.pinned(), so an unpinned device that simply did
//     not answer still gets its fallback — which is what that fallback is for.
//
// Splitting the sentinels is about what the user is TOLD (an unreachable device
// must not be told its identity is suspect, nor handed an `unpin` command) and
// about letting a caller tell the two facts apart. It is not licence to route a
// pinned device onto an unauthenticated transport.
func blocksUnauthenticatedFallback(err error) bool {
	return errors.Is(err, errDeviceIdentityRefused) || errors.Is(err, errNoAuthenticatedEndpoint)
}

// pinnedHostNoAuthenticatedEndpointError renders the honest version of what
// used to be a single message shared with genuine identity mismatches: every
// candidate address was dialed and none produced an authenticated endpoint, so
// no certificate ever arrived and no identity was ever compared against the
// pin.
//
// The pin is therefore not evidence of anything here, and this message
// deliberately contains no `wendy device unpin` command. Unpinning is the only
// irreversible action available at this prompt — it discards a trust binding
// that took a successful mTLS connection to establish — and the message that
// used to appear here recommended it for what is usually stale routing. On a
// network that rotates DHCP leases that is a standing invitation to throw the
// binding away on every lease change.
//
// It names every address tried and how many of each family, because "the CLI
// only ever tried one of the several addresses this name resolves to" is the
// fact that made the original report take hours to diagnose.
// certSeen must be true when ANY address rejected our certificate, so the
// message never claims nothing was compared when something was.
func pinnedHostNoAuthenticatedEndpointError(pinKey string, candidates []string, attempts []mtlsAttemptError, certSeen bool) error {
	var where string
	if tried := describeDialAttempts(attempts); len(tried) > 0 {
		where = fmt.Sprintf("at any address it resolves to: %s (each tried on both its plaintext and mTLS port)",
			strings.Join(tried, ", "))
	} else {
		// No mTLS rung ran at all — the CLI holds no usable client certificate
		// for this device. Still not an identity problem, so still no unpin.
		where = fmt.Sprintf("at %s, and no authenticated connection could be attempted (no usable client certificate for this device)",
			strings.Join(candidates, ", "))
	}
	// The diagnosis clause has to stay honest. If some address DID present a
	// certificate we refused, "no identity was compared" would be false, and
	// this message is the one the user acts on.
	diagnosis := "The pin is intact and no device identity was compared, so this is a reachability problem rather than an identity one"
	if certSeen {
		diagnosis = "The pin is intact; one address did present a certificate that was refused, so check the certificate diagnostics above as well as reachability"
	}
	v4, v6 := addrFamilyCounts(candidates)
	return &noAuthenticatedEndpointError{msg: fmt.Sprintf(
		"device %q is pinned to an enrolled identity and no authenticated endpoint answered %s. "+
			"%s — unpinning the device is not the fix. "+
			"Tried %s; if the device is only reachable over one address family, check that its agent is listening and that stale DNS/mDNS records are not holding the CLI to an address the device no longer has.",
		pinKey, where, diagnosis, describeAddrFamilies(v4, v6))}
}

// describeDialAttempts renders one entry per ADDRESS tried, in the order tried,
// carrying that address's last failure. Attempts arrive per address *and port*
// (the ladder tries the plaintext port and the mTLS port), so they are folded by
// host: six entries for three addresses would read as though the CLI had tried
// twice as many places as it did, and the count would then disagree with the
// family summary the user is being asked to act on.
func describeDialAttempts(attempts []mtlsAttemptError) []string {
	var order []string
	lastReason := map[string]string{}
	for _, a := range attempts {
		host := hostOnly(a.addr)
		if host == "" {
			continue
		}
		if _, seen := lastReason[host]; !seen {
			order = append(order, host)
		}
		if a.err != nil {
			lastReason[host] = condenseDialReason(a.err.Error())
		}
	}
	out := make([]string, 0, len(order))
	for _, host := range order {
		if reason := lastReason[host]; reason != "" {
			out = append(out, fmt.Sprintf("%s (%s)", host, reason))
		} else {
			out = append(out, host)
		}
	}
	return out
}

// condenseDialReason reduces a gRPC dial error to the one clause that tells the
// user what happened.
//
// The raw text arrives as
// `rpc error: code = Unavailable desc = connection error: desc = "transport:
// Error while dialing: dial tcp 10.0.0.5:50051: connect: connection refused"` —
// the two useful words are at the very END, so truncating the front (which is
// what a plain length cap does) throws away exactly the part being quoted. With
// several addresses to show, that produced a list of identical ellipses.
func condenseDialReason(reason string) string {
	flat := strings.Join(strings.Fields(reason), " ")
	// Timeouts are rewritten rather than quoted. connectToAgent classifies the
	// error it gets back by SUBSTRING (isReachabilityTimeoutError matches
	// "i/o timeout" / "deadline exceeded" / "connection timed out") to decide
	// whether to print "connection timed out; retrying" and re-run the whole
	// connect. Quoting the transport's own wording inside this message made a
	// black-holed address — the exact case this error exists for — trigger two
	// spurious full re-walks under a misleading banner. The wording below says
	// the same thing to a human and matches none of those classifiers.
	for _, timeout := range []string{"context deadline exceeded", "i/o timeout", "connection timed out"} {
		if strings.Contains(flat, timeout) {
			return "no response before the handshake budget elapsed"
		}
	}
	// Ordered so a more specific phrase wins over one it contains.
	for _, phrase := range []string{
		"connection refused",
		"no route to host",
		"host is unreachable",
		"network is unreachable",
		"connection reset by peer",
		"no such host",
		"tls: ",
	} {
		if idx := strings.Index(flat, phrase); idx >= 0 {
			return strings.TrimSuffix(strings.TrimSpace(flat[idx:]), `"`)
		}
	}
	const maxReason = 90
	if runes := []rune(flat); len(runes) > maxReason {
		return string(runes[:maxReason]) + "…"
	}
	return flat
}

// hostOnly strips the port from a host:port. Input without a port is returned
// trimmed rather than discarded — a bare host is legitimate here — so the ""
// result this can produce means only "nothing usable was given".
func hostOnly(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return strings.TrimSpace(addr)
	}
	return host
}

// addrFamilyCounts counts how many of addrs are IPv4 and how many IPv6, folding
// duplicates by host so the same address on two ports counts once.
//
// The fold keeps the zone: fe80::1%en0 and fe80::1%en1 are two candidates, dialed
// over two different interfaces, and either can fail while the other works — so
// the tally has to agree with describeDialAttempts, which lists them separately.
// Only the parse strips the zone, since net.ParseIP rejects a zoned literal.
func addrFamilyCounts(addrs []string) (v4, v6 int) {
	seen := map[string]bool{}
	for _, addr := range addrs {
		host := hostOnly(addr)
		if host == "" || seen[host] {
			continue
		}
		ip := net.ParseIP(stripZone(host))
		if ip == nil {
			continue
		}
		seen[host] = true
		if ip.To4() != nil {
			v4++
		} else {
			v6++
		}
	}
	return v4, v6
}

// describeAddrFamilies renders the family tally as a clause naming both
// families only when both were actually tried — "0 IPv6" invites the reader to
// debug an IPv6 problem that never happened.
func describeAddrFamilies(v4, v6 int) string {
	noun := func(n int) string {
		if n == 1 {
			return "address"
		}
		return "addresses"
	}
	switch {
	case v4 > 0 && v6 > 0:
		return fmt.Sprintf("%d IPv4 and %d IPv6 %s", v4, v6, noun(v4+v6))
	case v4 > 0:
		return fmt.Sprintf("%d IPv4 %s", v4, noun(v4))
	case v6 > 0:
		return fmt.Sprintf("%d IPv6 %s", v6, noun(v6))
	default:
		return "no resolved addresses"
	}
}

// spkiRefusal renders the OTHER pin's rejection: the SPKI store's, which is
// keyed by the certificate's own asset URN rather than by hostname and fires
// when a device's public key changes while its pinned certificate is still
// valid.
//
// Without this the store's PinMismatchError reached the user only as whatever
// text survived gRPC's handshake wrapper — a message that names neither the
// host as the user knows it nor any way out, leaving hand-editing
// known_devices.json as the only recovery. The SPKI store has no hostname in
// it, so the key comes from the dial: pinKey when there is one, the store's own
// display name otherwise.
//
// The command it prints names pm.Key — the SPKI store's own key — not the
// hostname, even though the hostname is what the user recognises. Naming the
// hostname is what made this refusal a dead end for the two populations that
// hit it most: the picker and `wendy device list` dial the IP first, so the
// message named an IP that keys nothing and whose asset nothing in local state
// can derive; and agents that never advertise `orgid` (the Swift macOS agent,
// Linux agents before 2026-07-18) leave the discovery cache with no identity to
// derive it from either. pm.Key is in hand at the moment of refusal and is
// exactly what the store is keyed by, so `wendy device unpin <urn>` reaches the
// entry directly — and clears any config pin naming the same identity with it.
func spkiRefusal(pinKey string, pm *devicepin.PinMismatchError) error {
	named := pinKey
	if named == "" {
		named = pm.DisplayName
	}
	// A store key is always present in a real mismatch; fall back to the name
	// only so a hand-built error still points at something.
	unpinArg := pm.Key
	if unpinArg == "" {
		unpinArg = named
	}
	return refuseIdentity(
		"device %q presented a different certificate key than the one pinned for %s (pinned %s, now %s); refusing to connect — if its certificate was legitimately reissued, run 'wendy device unpin %s'",
		named, pm.Key, pm.Want, pm.Got, unpinArg)
}
