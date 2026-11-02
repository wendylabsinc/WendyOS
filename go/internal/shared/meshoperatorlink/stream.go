// Package meshoperatorlink carries full operator routers and adjacent management
// over one authenticated TLS session. The shared Node owns Babel and routes.
package meshoperatorlink

import (
	"bytes"
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
)

const maxControlBytes = 16384 + 256

// ClientConfig binds the actual primary operator certificate and installation.
type ClientConfig struct {
	Identity meshoperator.VerifiedOperator
	Signer   crypto.Signer
	Instance meshoperator.InstanceID
	Router   meshid.Device
	Roots    []*x509.Certificate
}
type control struct {
	Stream   uint32                  `cbor:"s,omitempty"`
	Credit   uint32                  `cbor:"c,omitempty"`
	Error    string                  `cbor:"e,omitempty"`
	Kind     string                  `cbor:"k"`
	Instance meshoperator.InstanceID `cbor:"n,omitempty"`
}
type Link struct {
	writeMu sync.Mutex

	agentMu      sync.Mutex
	agent        *agentStream
	agentID      uint32
	agentClosing bool
	agentServing bool
	agentWake    chan struct{}
	agentHandler func(context.Context, net.Conn) error
	agentContext context.Context

	workers    sync.WaitGroup
	outControl chan control
	expiryMu   sync.Mutex
	expires    time.Time
	closed     bool
	conn       *tls.Conn
	address    netip.Addr
	server     bool
	once       sync.Once
	done       chan struct{}
	stop       func() bool
	timer      *time.Timer
}

func (l *Link) Address() netip.Addr   { return l.address }
func (l *Link) Done() <-chan struct{} { return l.done }
func (l *Link) Close() error {
	l.once.Do(func() {
		l.expiryMu.Lock()
		l.closed = true
		if l.timer != nil {
			l.timer.Stop()
		}
		l.expiryMu.Unlock()
		l.closeAgent()
		// Closing the underlying transport first wakes a blocked writer without a
		// TLS close_notify write waiting on the very reader being cancelled.
		_ = l.conn.NetConn().Close()
		if l.stop != nil {
			l.stop()
		}
		close(l.done)
	})
	return nil
}

// Wait joins attachment-owned background drivers after Close.
func (l *Link) Wait() { l.workers.Wait() }

func ready(ctx context.Context, c *tls.Conn, address netip.Addr, server bool, expires time.Time) (*Link, error) {
	if err := c.SetDeadline(expires); err != nil {
		return nil, err
	}
	l := &Link{conn: c, address: address, server: server, done: make(chan struct{}), expires: expires, outControl: make(chan control, 4), agentWake: make(chan struct{}, 1)}
	// Install callbacks before returning; their functions cannot access partially
	// assigned timer/stop fields because they wait for the publication barrier.
	published := make(chan struct{})
	l.stop = context.AfterFunc(ctx, func() { <-published; l.Close() })
	l.timer = time.AfterFunc(max(time.Until(expires), 0), func() { <-published; l.expire() })
	close(published)
	l.workers.Add(1)
	go l.controlWriter()
	return l, nil
}

func verifyRouterState(state tls.ConnectionState, cfg ClientConfig, now time.Time) (time.Time, error) {
	chain := state.PeerCertificates
	if !cfg.Router.IsValid() || cfg.Router.Tenant() != cfg.Identity.Operator().Tenant() || len(chain) == 0 || len(chain) > 16 || len(cfg.Roots) == 0 {
		return time.Time{}, errors.New("missing pinned router or primary trust")
	}
	peer, err := meshid.DeviceFromCert(chain[0])
	if err != nil {
		return time.Time{}, err
	}
	if peer != cfg.Router {
		return time.Time{}, errors.New("operator attachment router identity differs from target")
	}
	return certs.VerifyPeerCertificateChainExpiry(chain[0], chain[1:], cfg.Roots, x509.ExtKeyUsageServerAuth, now, now)
}

// ClientTLSConfig replaces hostname verification with the exact authenticated
// device principal and existing primary chain verifier. It presents the same
// frozen primary credential that will sign the attachment proof.
func ClientTLSConfig(cfg ClientConfig, certificate tls.Certificate) (*tls.Config, error) {
	if cfg.Signer == nil || len(certificate.Certificate) == 0 || !bytes.Equal(certificate.Certificate[0], cfg.Identity.LeafDER()) {
		return nil, errors.New("TLS presentation differs from primary operator credential")
	}
	if !cfg.Router.IsValid() || cfg.Router.Tenant() != cfg.Identity.Operator().Tenant() || len(cfg.Roots) == 0 {
		return nil, errors.New("missing same-tenant pinned router or primary trust")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, NextProtos: []string{RouterALPN}, Certificates: []tls.Certificate{certificate}, InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error { _, err := verifyRouterState(state, cfg, time.Now()); return err }}, nil
}

func writeFrame(w io.Writer, kind byte, data []byte) error {
	frame := make([]byte, 5+len(data))
	frame[0] = kind
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(data)))
	copy(frame[5:], data)
	for len(frame) > 0 {
		n, err := w.Write(frame)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
		frame = frame[n:]
	}
	return nil
}
func readFrame(r io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(header[1:])
	limit := uint32(maxControlBytes)
	if header[0] == 5 {
		limit = MaxRouterControl
	}
	if n == 0 || n > limit {
		return 0, nil, errors.New("invalid operator frame size")
	}
	if header[0] == 3 && (n <= 4 || n > agentFramePayload+4) {
		return 0, nil, errors.New("invalid operator agent frame size")
	}
	if header[0] == 2 && n > 1100 {
		return 0, nil, errors.New("oversized operator packet fragment")
	}
	body := make([]byte, n)
	_, err := io.ReadFull(r, body)
	return header[0], body, err
}
func writeControl(w io.Writer, m control) error {
	data, err := netwire.Marshal(m)
	if err != nil {
		return err
	}
	return writeFrame(w, 1, data)
}
func readControl(r io.Reader) (control, error) {
	kind, data, err := readFrame(r)
	var m control
	if err != nil {
		return m, err
	}
	if kind != 1 {
		return m, errors.New("expected operator control")
	}
	err = netwire.UnmarshalExtensibleArtifact(data, &m, maxControlBytes)
	return m, err
}
