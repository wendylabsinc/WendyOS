package estcacerts

import (
	"bytes"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"mime"
	"strings"
)

// ParseCertificates parses a PEM or EST certs-only CMS cacerts body.
func ParseCertificates(body []byte, contentType string) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte("-----BEGIN CERTIFICATE-----")) {

		for rest := body; len(bytes.TrimSpace(rest)) > 0; {
			block, tail := pem.Decode(rest)
			if block == nil || block.Type != "CERTIFICATE" || len(certs) >= 16 {
				return nil, fmt.Errorf("invalid PEM CA bundle")
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, err
			}
			certs = append(certs, cert)
			rest = tail
		}
	} else {
		media, _, err := mime.ParseMediaType(contentType)
		if err != nil || media != "application/pkcs7-mime" {
			return nil, fmt.Errorf("CA discovery expected PEM or EST certs-only CMS")
		}
		der := body
		if len(der) == 0 || der[0] != 0x30 {
			der, err = base64.StdEncoding.DecodeString(strings.TrimSpace(string(body)))
			if err != nil {
				return nil, fmt.Errorf("invalid EST base64: %w", err)
			}
		}
		var ci struct {
			Type    asn1.ObjectIdentifier
			Content asn1.RawValue
		}
		rest, err := asn1.Unmarshal(der, &ci)
		if err != nil || len(rest) != 0 || !ci.Type.Equal(asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}) {
			return nil, fmt.Errorf("invalid EST SignedData")
		}
		sdDER := ci.Content.FullBytes
		// Accept RFC 5652 explicit content and the direct SEQUENCE emitted by
		// existing pki-core deployments.
		if ci.Content.Class == 2 && ci.Content.Tag == 0 {
			sdDER = ci.Content.Bytes
		}
		var sd struct {
			Version int
			Digests []pkix.AlgorithmIdentifier `asn1:"set"`
			Content struct{ Type asn1.ObjectIdentifier }
			Certs   []asn1.RawValue `asn1:"optional,tag:0"`
			Signers []asn1.RawValue `asn1:"set"`
		}
		rest, err = asn1.Unmarshal(sdDER, &sd)
		if err != nil || len(rest) != 0 || sd.Version != 1 || len(sd.Digests) != 0 || !sd.Content.Type.Equal(asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}) || len(sd.Signers) != 0 || len(sd.Certs) > 16 {
			return nil, fmt.Errorf("invalid EST certs-only SignedData")
		}
		for _, raw := range sd.Certs {
			cert, err := x509.ParseCertificate(raw.FullBytes)
			if err != nil {
				return nil, err
			}
			certs = append(certs, cert)
		}
	}
	return certs, nil
}
