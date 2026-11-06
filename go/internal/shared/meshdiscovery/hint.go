// Package meshdiscovery defines the untrusted mesh scope hints shared by LAN
// and Wi-Fi Aware discovery. Certificates, not these hints, authorize peers.
package meshdiscovery

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// Hint is the first eight bytes of SHA-256 over a mesh name's exact UTF-8
// bytes. LAN TXT represents it as sixteen lowercase hex characters; NAN sends
// the eight bytes directly. A zero-valued hint is not an absent mesh.
type Hint [8]byte

// DefaultHint identifies today's single default mesh within an enrolled tenant.
// The caller must validate its tenant before advertising or admitting a peer.
func DefaultHint(tenant meshid.Tenant) Hint {
	sum := sha256.Sum256([]byte(fmt.Sprintf("tenant:%s:default", tenant)))
	return Hint(sum[:8])
}

func (h Hint) String() string { return hex.EncodeToString(h[:]) }
