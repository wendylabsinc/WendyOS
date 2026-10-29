package localmesh

import (
	"crypto/tls"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshalpn"
	"time"
)

func (c *Credentials) ClientProtocols(protocols ...string) []string {
	if c == nil || c.issuerMaterial == nil {
		return append([]string(nil), protocols...)
	}
	return c.issuerMaterial.ClientProtocols(protocols...)
}
func (c *Credentials) ConfigureTLSClient(config *tls.Config, protocols ...string) {
	if c.issuerMaterial == nil {
		config.NextProtos = append([]string(nil), protocols...)
		return
	}
	c.issuerMaterial.ConfigureClient(config, protocols...)
}
func (c *Credentials) ServerCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if c.issuerMaterial == nil {
		return &c.Certificate, nil
	}
	return c.issuerMaterial.ServerCertificate(hello)
}
func (c *Credentials) ConfigureTLSServer(config *tls.Config) {
	if c.issuerMaterial != nil {
		c.issuerMaterial.ConfigureServer(config)
	}
}
func (c *Credentials) knownIssuerCandidates(chain [][]byte, now time.Time) [][]byte {
	if c.issuerMaterial == nil {
		return chain
	}
	return c.issuerMaterial.Candidates(chain, now)
}

// ApplicationProtocol returns the actual service carried by a known variant.
func ApplicationProtocol(protocol string) string { return meshalpn.Application(protocol) }
