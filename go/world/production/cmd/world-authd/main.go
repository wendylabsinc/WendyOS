// world-authd serves one local simulation dataset behind Wendy read authority.
// It exposes no signing, commissioning, publishing, task or hardware endpoint.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/production/admission"
	"github.com/wendylabsinc/wendy/go/world/production/authenticatedtime"
	"github.com/wendylabsinc/wendy/go/world/production/authorizer"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/operator"
	"github.com/wendylabsinc/wendy/go/world/production/readservice"
	"github.com/wendylabsinc/wendy/go/world/production/simbackend"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"golang.org/x/sys/unix"
)

type config struct {
	Version          int                 `json:"version"`
	Tenant           string              `json:"tenant"`
	Site             string              `json:"site"`
	Audience         string              `json:"audience"`
	Listen           string              `json:"listen"`
	TLSCertificate   string              `json:"tls_certificate"`
	TLSKey           string              `json:"tls_key"`
	TrustRoots       string              `json:"trust_roots"`
	Ledger           string              `json:"ledger"`
	PolicyIssuer     admission.Pin       `json:"policy_issuer"`
	RevocationIssuer admission.Pin       `json:"revocation_issuer"`
	Policy           string              `json:"policy"`
	Revocations      string              `json:"revocations"`
	TimeSource       string              `json:"time_source"`
	Dataset          readservice.Dataset `json:"dataset"`
	SimulationReader string              `json:"simulation_reader"`
}

// O_NONBLOCK prevents a raced replacement with a FIFO from blocking startup.
func readFile(path string, limit int, private bool) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("configured input cannot be opened")
	}
	f := os.NewFile(uintptr(fd), "authd-input")
	defer f.Close()
	s, err := f.Stat()
	if err != nil || !s.Mode().IsRegular() || (private && s.Mode().Perm()&0077 != 0) {
		return nil, errors.New("configured input requires a regular file with appropriate permissions")
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil || len(b) > limit {
		clear(b)
		return nil, errors.New("configured input exceeds bound")
	}
	return b, nil
}

// Validate before resolving or opening anything; hostnames and service names
// are not commissioned listener addresses. Port zero is useful for local tests.
func loopbackAddress(value string) (*net.TCPAddr, error) {
	host, portText, err := net.SplitHostPort(value)
	if err != nil {
		return nil, errors.New("listener must be numeric loopback")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() || portText == "" {
		return nil, errors.New("listener must be numeric loopback")
	}
	for _, c := range portText {
		if c < '0' || c > '9' {
			return nil, errors.New("listener port must be numeric")
		}
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return nil, errors.New("invalid listener port")
	}
	return &net.TCPAddr{IP: ip, Port: port}, nil
}

func run(ctx context.Context, path string) error {
	raw, err := readFile(path, 64<<10, false)
	if err != nil {
		return err
	}
	var cfg config
	if jws.DecodeStrict(raw, &cfg) != nil || cfg.Version != 1 || cfg.TimeSource != "wendy" {
		return errors.New("explicit version 1 configuration and Wendy time required")
	}
	addr, err := loopbackAddress(cfg.Listen)
	if err != nil {
		return err
	}
	roots, err := operator.LoadTrustRoots(cfg.TrustRoots)
	if err != nil {
		return err
	}
	certificates, err := wendycert.New(wendycert.Config{Tenant: cfg.Tenant, Roots: roots})
	if err != nil {
		return err
	}
	certPEM, err := readFile(cfg.TLSCertificate, 64<<10, false)
	if err != nil {
		return err
	}
	keyPEM, err := readFile(cfg.TLSKey, 32<<10, true)
	if err != nil {
		return err
	}
	defer clear(keyPEM)
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return errors.New("invalid local TLS pair")
	}
	backendBytes, err := readFile(cfg.SimulationReader, 64<<10, true)
	if err != nil {
		return err
	}
	defer clear(backendBytes)
	var backendConfig client.Config
	if jws.DecodeStrict(backendBytes, &backendConfig) != nil {
		return errors.New("invalid simulation reader configuration")
	}
	backend, err := simbackend.New(backendConfig, cfg.Dataset)
	if err != nil {
		return err
	}
	clock, err := authenticatedtime.Acquire(ctx, authenticatedtime.Config{Servers: authenticatedtime.WendyServers(), QueryTimeout: 3 * time.Second, MaxHoldover: 10 * time.Minute, DriftPPM: 100, MaxWidth: 5 * time.Second})
	if err != nil {
		return err
	}
	auth, err := authorizer.Open(cfg.Ledger, authorizer.Config{Clock: clock, Admission: admission.Config{Tenant: cfg.Tenant, Site: cfg.Site, Audience: cfg.Audience, PolicyIssuer: cfg.PolicyIssuer, RevocationIssuer: cfg.RevocationIssuer, Certificates: certificates, MaxRevocationAge: 10 * time.Minute, MaxPolicyLifetime: 10 * time.Minute, MaxRequestLifetime: time.Minute}})
	if err != nil {
		return err
	}
	defer auth.Close()
	for _, input := range []struct {
		path  string
		apply func(string) error
	}{{cfg.Revocations, auth.ApplyRevocations}, {cfg.Policy, auth.ApplyPolicy}} {
		b, err := readFile(input.path, jws.MaxProofBytes, false)
		if err != nil {
			return err
		}
		if err = input.apply(strings.TrimSpace(string(b))); err != nil {
			return err
		}
	}
	handler, err := readservice.New(readservice.Config{Tenant: cfg.Tenant, Site: cfg.Site, Audience: cfg.Audience, Dataset: cfg.Dataset, Authorizer: auth, Reader: backend})
	if err != nil {
		return err
	}
	listener, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return err
	}
	bounded := &boundedListener{Listener: listener, slots: make(chan struct{}, 32), done: make(chan struct{})}
	defer bounded.Close()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 6 * time.Second, WriteTimeout: 6 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8 << 10, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, NextProtos: []string{"http/1.1"}}, TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){}}
	server.SetKeepAlivesEnabled(false)
	errch := make(chan error, 1)
	go func() { errch <- server.Serve(tls.NewListener(bounded, server.TLSConfig)) }()
	if err = json.NewEncoder(os.Stdout).Encode(map[string]any{"schema": "wendy.world.authd.ready.v1", "listen": listener.Addr().String(), "dataset": cfg.Dataset, "time": clock.Report()}); err != nil {
		_ = server.Close()
		return err
	}
	select {
	case err = <-errch:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 7*time.Second)
		defer cancel()
		if err = server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	}
}

type boundedListener struct {
	net.Listener
	slots chan struct{}
	done  chan struct{}
	once  sync.Once
}

func (l *boundedListener) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	c, e := l.Listener.Accept()
	if e != nil {
		<-l.slots
		return nil, e
	}
	return &boundedConn{Conn: c, release: func() { <-l.slots }}, nil
}
func (l *boundedListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type boundedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *boundedConn) Close() error { e := c.Conn.Close(); c.once.Do(c.release); return e }

func main() {
	path := flag.String("config", "", "explicit local read-service configuration")
	flag.Parse()
	if *path == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: world-authd --config service.json")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, *path); err != nil {
		fmt.Fprintln(os.Stderr, "read service unavailable:", err)
		os.Exit(1)
	}
}
