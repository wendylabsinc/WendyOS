package timesync

import (
	"crypto/ed25519"
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/shared/roughtime"
)

// Servers is the baked-in set of Roughtime servers queried on startup and on
// network-up events. All four are queried at once and the answers are
// intersected, rather than the first valid response being taken: Roughtime's
// guarantee is that a lying server can be CAUGHT, not that any one server can
// be trusted, and catching it needs more than one answer to compare.
//
// The direct loop applies the intersection at "verified" (three or more
// agreeing) and at "degraded" (two agreeing), and retries on the backoff
// schedule below that; see disciplinesClock in roughtime.go for why two is the
// floor. The multicast relay path is separate and applies a single relayed
// response, because there the sender is a machine on the local network that
// the device is already trusting for provisioning.
//
// Keys must be fetched from each operator's published ecosystem JSON and
// encoded as 32-byte ed25519.PublicKey. See design doc for retrieval steps.
var Servers = []roughtime.Server{
	{
		Name:      "cloudflare",
		Address:   "roughtime.cloudflare.com:2003",
		PublicKey: ed25519.PublicKey(mustDecodeKey(cloudflarePublicKeyHex)),
	},
	{
		Name:      "int08h",
		Address:   "roughtime.int08h.com:2002",
		PublicKey: ed25519.PublicKey(mustDecodeKey(int08hPublicKeyHex)),
	},
	{
		Name:      "roughtime.se",
		Address:   "roughtime.se:2002",
		PublicKey: ed25519.PublicKey(mustDecodeKey(roughtimeSEPublicKeyHex)),
	},
	{
		Name:      "time.txryan.com",
		Address:   "time.txryan.com:2002",
		PublicKey: ed25519.PublicKey(mustDecodeKey(txryanPublicKeyHex)),
	},
}

// Public keys are 32-byte Ed25519 keys from Cloudflare's Roughtime ecosystem JSON.
const (
	cloudflarePublicKeyHex  = "d060fb737c8ff3111ce19976cdeb8dd9294bbc3555a1c8ec3d22fcfd197fef38"
	int08hPublicKeyHex      = "016e6e0284d24c37c6e4d7d8d5b4e1d3c1949ceaa545bf875616c9dce0c9bec1"
	roughtimeSEPublicKeyHex = "4b70337d92790a349d909db564919bc6a7583ff4a813c7d7298d3e6a272c7a12"
	txryanPublicKeyHex      = "881563c60ff58fbcb5fa44144c161d4da6f10a9a5eb14ff4ec3e0f303264d960"
)

func mustDecodeKey(hex64 string) []byte {
	b, err := hexDecode32(hex64)
	if err != nil {
		panic("timesync: invalid server public key: " + err.Error())
	}
	return b
}

func hexDecode32(s string) ([]byte, error) {
	// encoding/hex is not imported to avoid adding an import just for init.
	// Inline a minimal hex decoder for 32-byte keys (64 hex chars).
	if len(s) != 64 {
		return nil, fmt.Errorf("expected 64 hex chars, got %d", len(s))
	}
	b := make([]byte, 32)
	for i := 0; i < 32; i++ {
		hi, ok1 := hexNibble(s[i*2])
		lo, ok2 := hexNibble(s[i*2+1])
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("invalid hex at position %d", i*2)
		}
		b[i] = hi<<4 | lo
	}
	return b, nil
}

func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
