package useripc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
)

type loopbackIdentity struct {
	Address     string `json:"address"`
	Certificate []byte `json:"certificate"`
	Key         []byte `json:"key"`
}

func (i loopbackIdentity) tlsConfig(server bool) (*tls.Config, error) {
	host, port, err := net.SplitHostPort(i.Address)
	if err != nil || host != "127.0.0.1" || port == "0" {
		return nil, errors.New("IPC requires IPv4 loopback")
	}
	pair, err := tls.X509KeyPair(i.Certificate, i.Key)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(i.Certificate) {
		return nil, errors.New("invalid IPC certificate")
	}
	config := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, RootCAs: pool, ClientCAs: pool, ServerName: "localhost", SessionTicketsDisabled: true}
	if server {
		config.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return config, nil
}
func listenLoopback(path string) (net.Listener, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	// Trust is scoped to this listener's private file, removed when it closes.
	// The generous expiry avoids interrupting a long-running local session.
	certificate := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Wendy private IPC"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().AddDate(1, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	identity := loopbackIdentity{Address: listener.Addr().String(), Certificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), Key: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})}
	config, err := identity.tlsConfig(true)
	if err != nil {
		listener.Close()
		return nil, err
	}
	data, err := json.Marshal(identity)
	if err != nil {
		listener.Close()
		return nil, err
	}
	if err = atomicfile.Write(path, data, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	return &loopbackListener{Listener: tls.NewListener(listener, config), path: path}, nil
}
func dialLoopback(ctx context.Context, path string) (net.Conn, error) {
	if err := ValidateFile(path, false); err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > 16*1024 {
		return nil, errors.New("oversized IPC identity")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var identity loopbackIdentity
	if err = json.Unmarshal(data, &identity); err != nil {
		return nil, err
	}
	config, err := identity.tlsConfig(false)
	if err != nil {
		return nil, err
	}
	return (&tls.Dialer{Config: config}).DialContext(ctx, "tcp4", identity.Address)
}

type loopbackListener struct {
	net.Listener
	path string
	once sync.Once
	err  error
}

func (l *loopbackListener) Close() error {
	l.once.Do(func() { l.err = l.Listener.Close(); _ = os.Remove(l.path) })
	return l.err
}
