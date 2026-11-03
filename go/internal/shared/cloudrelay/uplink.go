package cloudrelay

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/http/httpproxy"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Leave time for alternative interfaces within the agent's 15s lease request.
const uplinkAttemptTimeout = 3 * time.Second

type uplink struct {
	name string
	dial func(context.Context, string, string) (net.Conn, error)
}

type uplinkDialer struct {
	primary    uplink
	candidates func(context.Context) ([]uplink, error)
	timeout    time.Duration
}

func newUplinkDialer() *uplinkDialer {
	return &uplinkDialer{primary: uplink{name: "system route", dial: (&net.Dialer{}).DialContext},
		candidates: availableUplinks, timeout: uplinkAttemptTimeout}
}

// Try the system route first. Race alternatives only after it fails, so a dead
// Ethernet default cannot consume the whole deadline before Wi-Fi is tried.
// attempt includes TLS when appropriate: a captive portal accepting TCP does
// not establish a usable Cloud path.
func (d *uplinkDialer) connect(ctx context.Context, attempt func(context.Context, uplink) (net.Conn, error)) (net.Conn, error) {
	try := func(ctx context.Context, path uplink) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, d.timeout)
		defer cancel()
		return attempt(ctx, path)
	}
	conn, primaryErr := try(ctx, d.primary)
	if primaryErr == nil {
		return conn, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	paths, err := d.candidates(ctx)
	if err != nil {
		return nil, errors.Join(primaryErr, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	type result struct {
		conn net.Conn
		err  error
	}
	results := make(chan result)
	jobs := make(chan uplink, len(paths))
	for _, path := range paths {
		jobs <- path
	}
	close(jobs)
	var workers errgroup.Group
	defer func() { cancel(); _ = workers.Wait() }()
	workers.SetLimit(4)
	for range min(4, len(paths)) {
		workers.Go(func() error {
			for path := range jobs {
				if ctx.Err() != nil {
					return nil
				}
				conn, err := try(ctx, path)
				if err != nil {
					err = fmt.Errorf("uplink %s: %w", path.name, err)
				}
				select {
				case results <- result{conn, err}:
				case <-ctx.Done():
					if conn != nil {
						conn.Close()
					}
				}
			}
			return nil
		})
	}
	errs := []error{primaryErr}
	for range paths {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case r := <-results:
			if r.err == nil {
				return r.conn, nil
			}
			errs = append(errs, r.err)
		}
	}
	return nil, errors.Join(errs...)
}

func (d *uplinkDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return d.connect(ctx, func(ctx context.Context, path uplink) (net.Conn, error) {
		return path.dial(ctx, network, address)
	})
}

type authenticatedConn struct {
	net.Conn
	auth credentials.AuthInfo
}

func (d *uplinkDialer) dialAuthenticated(ctx context.Context, address string, creds credentials.TransportCredentials) (net.Conn, error) {
	return d.connect(ctx, func(ctx context.Context, path uplink) (net.Conn, error) {
		raw, err := path.dial(ctx, "tcp", address)
		if err != nil {
			return nil, err
		}
		conn, auth, err := creds.ClientHandshake(ctx, address, raw)
		if err != nil {
			raw.Close()
			return nil, err
		}
		return &authenticatedConn{Conn: conn, auth: auth}, nil
	})
}

// TLS is completed inside the dial attempt so handshake failures also try
// other interfaces. gRPC still receives the original credentials' TLSInfo,
// including the peer certificate and security level for per-RPC credentials.
type uplinkCredentials struct {
	credentials.TransportCredentials
}

func (c uplinkCredentials) ClientHandshake(_ context.Context, _ string, raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	conn, ok := raw.(*authenticatedConn)
	if !ok {
		return nil, nil, errors.New("Cloud transport was not TLS authenticated")
	}
	return conn.Conn, conn.auth, nil
}

func (c uplinkCredentials) Clone() credentials.TransportCredentials {
	return uplinkCredentials{c.TransportCredentials.Clone()}
}

func newCloudClient(address string, creds credentials.TransportCredentials, fallback bool) (*grpc.ClientConn, error) {
	// A custom gRPC dialer bypasses its HTTP CONNECT proxy support. Preserve
	// explicitly configured proxies instead of silently connecting directly.
	proxy, proxyErr := httpproxy.FromEnvironment().ProxyFunc()(&url.URL{Scheme: "https", Host: address})
	if !fallback || !interfaceFallbackSupported || proxyErr != nil || proxy != nil {
		return grpc.NewClient(address, grpc.WithTransportCredentials(creds), withKeepalive(brokerKeepalive))
	}
	d := newUplinkDialer()
	// Keep the hostname intact. gRPC's DNS resolver would otherwise fail on the
	// dead primary before the interface-specific resolver ever gets a chance.
	return grpc.NewClient("passthrough:///"+address,
		grpc.WithContextDialer(func(ctx context.Context, address string) (net.Conn, error) {
			return d.dialAuthenticated(ctx, address, creds)
		}), grpc.WithTransportCredentials(uplinkCredentials{creds}), withKeepalive(brokerKeepalive))
}

func cloudKeyTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if interfaceFallbackSupported {
		d := newUplinkDialer()
		t.DialContext = d.DialContext
		t.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			return d.connect(ctx, func(ctx context.Context, path uplink) (net.Conn, error) {
				raw, err := path.dial(ctx, network, address)
				if err != nil {
					return nil, err
				}
				conn := tls.Client(raw, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
				if err := conn.HandshakeContext(ctx); err != nil {
					raw.Close()
					return nil, err
				}
				return conn, nil
			})
		}
	}
	return t
}

var grantKeyTransport = cloudKeyTransport()
