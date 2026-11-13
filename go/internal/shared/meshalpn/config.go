package meshalpn

import (
	"crypto/tls"
	"crypto/x509"
	"strings"
	"time"
)

// ClientProtocols offers root-first fingerprints and cached CA availability.
// Aliases acknowledge the shared prefix without changing the application.
func (c *Certificates) ClientProtocols(protocols ...string) []string {
	if c == nil {
		return append([]string(nil), protocols...)
	}
	if c.recoveryState().active() {
		return append([]string(nil), protocols...)
	}
	return c.clientProtocols(c.snapshot(time.Now()), protocols...)
}
func (c *Certificates) clientProtocols(known []*x509.Certificate, protocols ...string) []string {
	cached := make([]Fingerprint, 0, len(known))
	for _, cert := range known {
		cached = append(cached, Hash(cert.Raw))
	}
	var result []string
	for _, protocol := range protocols {
		result = append(result, Offer(protocol, c.path, cached)...)
	}
	return result
}

// A connection keeps the exact immutable CA snapshot it advertised/acknowledged.
// Cache FIFO turnover may drop its metadata but Go references retain the DER
// until verification has consumed it. No lock is held during TLS or crypto.
func (c *Certificates) bindIssuerSnapshot(config *tls.Config, available []*x509.Certificate, hinted bool) {
	if c == nil {
		return
	}
	loan := &handshakeIssuers{available: available}
	loan.configureTickets(config)
	chainVerify := config.VerifyPeerCertificate
	if chainVerify != nil {
		config.VerifyPeerCertificate = func(raw [][]byte, verified [][]*x509.Certificate) error {
			err := chainVerify(c.candidatesFrom(raw, loan.available), verified)
			if err != nil {
				c.recoveryState().failed(err, hinted)
				loan.available = nil
			}
			return err
		}
	}
	previous := config.VerifyConnection
	config.VerifyConnection = func(state tls.ConnectionState) error {
		defer func() { loan.available = nil }()
		raw := make([][]byte, len(state.PeerCertificates))
		for i, cert := range state.PeerCertificates {
			raw[i] = cert.Raw
		}
		complete := c.candidatesFrom(raw, loan.available)
		augmented := state
		augmented.PeerCertificates = append([]*x509.Certificate(nil), state.PeerCertificates...)
		for _, der := range complete[len(raw):] {
			cert, err := x509.ParseCertificate(der)
			if err != nil {
				return err
			}
			augmented.PeerCertificates = append(augmented.PeerCertificates, cert)
		}
		if previous != nil {
			if err := previous(augmented); err != nil {
				c.recoveryState().failed(err, hinted)
				return err
			}
		}
		loan.dependencies = loan.dependencies[:0]
		for _, der := range complete[len(raw):] {
			loan.dependencies = append(loan.dependencies, Hash(der))
		}
		loan.protocol = state.NegotiatedProtocol
		c.Remember(complete, time.Now())
		return nil
	}
}

func omitKnownCertificates(certificate tls.Certificate, known map[Fingerprint]bool) *tls.Certificate {
	filtered := certificate
	filtered.Certificate = make([][]byte, 0, len(certificate.Certificate))
	for index, der := range certificate.Certificate {
		if index == 0 || !known[Hash(der)] {
			filtered.Certificate = append(filtered.Certificate, der)
		}
	}
	return &filtered
}

// ConfigureClient operates on a connection-specific config. Its certificate
// selection state must not be shared by cached TLS configs or concurrent dials.
func (c *Certificates) ConfigureClient(config *tls.Config, protocols ...string) {
	if c != nil {
		available := c.snapshot(time.Now())
		hinted := !c.recoveryState().active()
		if hinted {
			config.NextProtos = resumptionProtocols(config, c.clientProtocols(available, protocols...), available)
			hinted = false
			for _, protocol := range config.NextProtos {
				_, hashes := Decode(protocol)
				if len(hashes) > 0 || strings.HasPrefix(protocol, inventory) {
					hinted = true
					break
				}
			}
		} else {
			config.NextProtos = append([]string(nil), protocols...)
			fullExchange(config)
		}
		c.bindIssuerSnapshot(config, available, hinted)
	} else {
		config.NextProtos = append([]string(nil), protocols...)
		return
	}
	var acknowledged []Fingerprint
	previous := config.VerifyConnection
	config.VerifyConnection = func(state tls.ConnectionState) error {
		if previous != nil {
			if err := previous(state); err != nil {
				return err
			}
		}
		_, acknowledged = Decode(state.NegotiatedProtocol)
		return nil
	}
	selectCertificate := config.GetClientCertificate
	config.GetClientCertificate = func(request *tls.CertificateRequestInfo) (*tls.Certificate, error) {
		certificate := c.certificate
		if len(config.Certificates) > 0 {
			certificate = config.Certificates[0]
		}
		if selectCertificate != nil {
			selected, err := selectCertificate(request)
			if err != nil || selected == nil {
				return selected, err
			}
			certificate = *selected
		}
		known := make(map[Fingerprint]bool, len(acknowledged))
		for _, hash := range acknowledged {
			known[hash] = true
		}
		return omitKnownCertificates(certificate, known), nil
	}
}

// ServerCertificate removes only the exact CA certificates advertised by the
// client. Certificate verification and trust anchors are unchanged.
func (c *Certificates) ServerCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if c.recoveryState().active() {
		return &c.certificate, nil
	}
	return omitKnownCertificates(c.certificate, Offered(hello.SupportedProtos, "")), nil
}

// ConfigureServer uses a connection-specific config to acknowledge the
// longest client prefix locally available. ClientHello selection remains
// stateless on a shared listener, including concurrent no-SNI handshakes.
func (c *Certificates) ConfigureServer(config *tls.Config) {
	if c == nil {
		return
	}
	config.Certificates = nil
	config.GetCertificate = c.ServerCertificate
	previous := config.GetConfigForClient
	config.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		selected := config
		if previous != nil {
			var err error
			selected, err = previous(hello)
			if err != nil {
				return nil, err
			}
			if selected == nil {
				selected = config
			}
		}
		known := make(map[Fingerprint]bool)
		var available []*x509.Certificate
		if c != nil {
			available = c.snapshot(time.Now())
			for _, cert := range available {
				known[Hash(cert.Raw)] = true
			}
		}
		cooldown := c.recoveryState().active()
		protocol := Select(selected.NextProtos, hello.SupportedProtos, func(hash Fingerprint) bool { return !cooldown && known[hash] })
		if protocol == "" {
			return selected, nil
		} // TLS reports the ordinary ALPN failure.
		result := selected.Clone()
		result.GetConfigForClient = nil
		result.NextProtos = []string{protocol}
		_, hashes := Decode(protocol)
		if len(hashes) == 0 {
			// Ordinary negotiated ALPN always requests full presentations in both
			// directions, even if the hello also contained inventory names.
			result.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &c.certificate, nil }
		} else {
			// Freeze the choice for this handshake, including across cooldown expiry.
			certificate := omitKnownCertificates(c.certificate, Offered(hello.SupportedProtos, ""))
			result.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return certificate, nil }
		}
		if cooldown {
			fullExchange(result)
		}
		c.bindIssuerSnapshot(result, available, len(hashes) > 0)
		return result, nil
	}
}
