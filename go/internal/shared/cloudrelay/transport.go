package cloudrelay

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// DeviceEndpoint selects the end-to-end mTLS listener. The public API listener
// authenticates operators and must not receive a fabricated device XFCC header.
func DeviceEndpoint(cloud, override string) (string, error) {
	if override != "" {
		return target(override)
	}
	t, err := target(cloud)
	if err != nil {
		return "", err
	}
	host, port, _ := net.SplitHostPort(t)
	switch host {
	case "api.dev.wendy.sh":
		host = "devices.dev.wendy.sh"
	case "api.wendy.sh":
		host = "devices.wendy.sh"
	}
	return net.JoinHostPort(host, port), nil
}
func Issuer(cloud, override string) (string, error) {
	if override != "" {
		u, e := url.Parse(override)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return "", fmt.Errorf("Cloud grant issuer must be an HTTPS URL")
		}
		return strings.TrimSuffix(override, "/"), nil
	}
	t, err := target(cloud)
	if err != nil {
		return "", err
	}
	host, port, _ := net.SplitHostPort(t)
	if port == "443" {
		return "https://" + host, nil
	}
	return "https://" + t, nil
}
func target(endpoint string) (string, error) {
	if strings.Contains(endpoint, "://") {
		u, err := url.Parse(endpoint)
		if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
			return "", fmt.Errorf("relay endpoint must be an HTTPS URL or host:port")
		}
		endpoint = u.Host
	}
	if strings.ContainsAny(endpoint, "/ ?#@\r\n\t") {
		return "", fmt.Errorf("invalid relay endpoint")
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		host = endpoint
		port = "443"
	}
	if host == "" || port == "" {
		return "", fmt.Errorf("empty relay endpoint")
	}
	return net.JoinHostPort(host, port), nil
}

// DialCloud authenticates the device directly with its complete normalized
// certificate chain. No client-supplied identity headers are used.
func DialCloud(endpoint, certPEM, chainPEM string, keyPEM []byte) (*grpc.ClientConn, error) {
	t, err := target(endpoint)
	if err != nil {
		return nil, err
	}
	pair, err := certs.TLSKeyPair(certPEM, chainPEM, string(keyPEM))
	if err != nil {
		return nil, err
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	certs.AppendChainToPool(roots, chainPEM)
	trust, err := certs.ParseCertsFromPEM([]byte(chainPEM))
	if err != nil {
		return nil, err
	}
	host, _, _ := net.SplitHostPort(t)
	config := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: host, Certificates: []tls.Certificate{pair}, RootCAs: roots}
	// Standard validation handles public CAs; custom verification supports the
	// ML-DSA issuing hierarchy on Cloud's dedicated devices listener.
	config.InsecureSkipVerify = true // verification is performed below, including hostname
	config.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return fmt.Errorf("Cloud presented no certificate")
		}
		leaf := cs.PeerCertificates[0]
		if err := leaf.VerifyHostname(host); err != nil {
			return err
		}
		intermediates := x509.NewCertPool()
		for _, c := range cs.PeerCertificates[1:] {
			intermediates.AddCert(c)
		}
		if _, e := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); e == nil {
			return nil
		}
		return certs.VerifyPeerCertificateChain(leaf, cs.PeerCertificates[1:], trust, x509.ExtKeyUsageServerAuth, time.Now(), time.Now())
	}
	return grpc.NewClient(t, grpc.WithTransportCredentials(credentials.NewTLS(config)))
}

// DialBroker deliberately has no client certificate or identity metadata.
func DialBroker(endpoint string) (*grpc.ClientConn, error) {
	t, err := target(endpoint)
	if err != nil {
		return nil, err
	}
	return grpc.NewClient(t, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13})))
}
