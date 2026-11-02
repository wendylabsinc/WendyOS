package localmesh

import "github.com/wendylabsinc/wendy/go/internal/shared/meshalpn"

// Test credentials model independent processes, rather than sharing a cooldown
// between unrelated simulated devices and test cases.
func newTestCredentials(cert, chain, key string) (*Credentials, error) {
	c, err := NewCredentials(cert, chain, key)
	if err == nil {
		c.issuerMaterial.SetRecovery(new(meshalpn.Recovery))
	}
	return c, err
}
