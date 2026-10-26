package services

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// PeerDialer opens a byte stream to a port on another device in this tenant:
// LAN-direct when possible, over the routed mesh, or via the Cloud relay
// otherwise. Satisfied by services.MeshDialer.
//
// Declared here rather than taking *MeshDialer so the build service can be
// tested without a broker, matching how mesh.Proxy declares its own dialer.
type PeerDialer interface {
	DialTarget(ctx context.Context, target DeliveryTarget, port uint16) (net.Conn, string, error)
}

// pushProxy forwards loopback connections to a peer device's registry over
// mTLS, so buildkitd can push plaintext to localhost and needs no per-registry
// client certificates of its own.
//
// It records the most recent actionable outbound failure. Without that, a proxy that cannot
// reach its target still accepts the local connection and then closes it, so
// the pusher sees only "connection reset by peer" on 127.0.0.1 — a message that
// cannot distinguish an unreachable peer from a rejected certificate, which are
// the two causes with entirely different fixes. The most recent failure is the
// useful one because BuildKit may retry an earlier 502 and reach a different
// terminal outcome. Cleanup cancellation is retained only when no actionable
// failure preceded it, so teardown cannot erase the cause a user can fix.
type pushProxy struct {
	addr string
	ln   net.Listener
	stop func()
	// credential is this build's loopback password. The listener is on
	// 127.0.0.1, which is not a boundary on a shared build host: every local
	// user can reach it, and without a credential any of them could push an
	// arbitrary image to the target device using the agent's mesh identity and
	// client certificate, for as long as a build runs.
	credential string
	assetUUID  string
	// dial opens the outbound hop. A field so tests can relay over plain TCP;
	// production dials the mesh peer and wraps the result in TLS. It must be set
	// before serve, which is when the first reader of it can exist.
	dial func(ctx context.Context) (net.Conn, error)

	mu  sync.Mutex
	err error
}

// latestError returns the most recent outbound failure seen, or nil if the
// proxy never failed (including the case where nothing ever connected to it).
func (p *pushProxy) latestError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *pushProxy) recordError(err error) {
	if err == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil && pushProxyCleanupError(err) && !pushProxyCleanupError(p.err) {
		return
	}
	p.err = err
}

// pushProxyCleanupError identifies errors commonly produced while a failed
// request or the proxy itself is being torn down. They are useful when they are
// the only cause available, but must not replace an earlier registry, mesh, or
// TLS failure with the much less actionable "context canceled"/"closed".
func pushProxyCleanupError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed)
}

// validRepositoryRe matches a bare OCI "repository:tag" with no registry host
// and no separator that buildctl's --output parser would read as structure.
//
// It is an allowlist rather than a list of rejected characters on purpose: the
// value is concatenated into `type=image,name=<ref>,push=true`, where a comma
// starts a new key=value pair and an '=' starts a new value, so anything not
// positively known to be part of a name is a chance to append an exporter
// option. The CLI only ever sends lowercased "<appid>:latest".
var validRepositoryRe = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?::[A-Za-z0-9][A-Za-z0-9._-]*)?$`)

// validatePushTarget checks a push destination before any build runs.
//
// There is no hostname to constrain here: an asset UUID can only ever address
// a device in this tenant through the peer dialer, so the "push an image
// anywhere" hazard a free-form registry string carried is structurally absent.
// What is left is shape — a canonical UUID, a real port, and a repository that
// is a bare "repo:tag" with no host smuggled into it.
func validatePushTarget(t *agentpbv2.PushTarget) error {
	if t == nil {
		return status.Error(codes.InvalidArgument, "build spec carries no push target")
	}
	if _, err := meshid.ParseDeviceUUID(t.GetAssetUuid()); err != nil {
		return status.Errorf(codes.InvalidArgument, "push target asset is not a canonical Cloud asset UUID: %v", err)
	}
	if p := t.GetRegistryPort(); p == 0 || p > 65535 {
		return status.Errorf(codes.InvalidArgument, "push target has an invalid registry port %d", p)
	}
	repo := t.GetRepository()
	if repo == "" {
		return status.Error(codes.InvalidArgument, "push target has no repository")
	}
	// A slash would make the first element a registry host once joined to the
	// proxy address, quietly redirecting the push somewhere else; a comma or an
	// '=' would end the name and start another buildctl output option.
	if !validRepositoryRe.MatchString(repo) {
		return status.Errorf(codes.InvalidArgument, "push target repository %q must be a bare repository:tag", repo)
	}
	return nil
}

// startPushProxy listens on loopback and forwards each accepted connection to
// the target device's registry, dialed through the mesh by resolved delivery
// target and then wrapped in mTLS with this host's client certificate.
//
// This does not change how the image is named: the target's registry derives
// its image prefix from its own listen address, so the image lands there as
// localhost:<regPort>/<repo>:<tag> regardless of how the pusher reached it.
func startPushProxy(ctx context.Context, dialer PeerDialer, target DeliveryTarget, registryPort uint16, tlsCfg *tls.Config) (*pushProxy, error) {
	p, err := newPushProxy(dialer, target, registryPort, tlsCfg)
	if err != nil {
		return nil, err
	}
	p.serve(ctx)
	return p, nil
}

// pushProxyUser is the username half of the loopback credential. Only the
// password carries entropy; the username exists because the registry credential
// format has a slot for one.
const pushProxyUser = "wendy-build"

// pushIdleConnTimeout bounds how long an unused mesh tunnel stays in the HTTP
// pool. It reduces stale reuse; it is not a liveness probe and does not replace
// BuildKit's descriptor-level retry after a failed request.
const pushIdleConnTimeout = 20 * time.Second

// newPushProxy binds the loopback listener and mints this build's credential,
// but serves nothing yet.
//
// Construction is split from serve so that a caller replacing dial — a test
// relaying over plain TCP — writes the field before any goroutine that reads it
// exists. Overwriting it after the server is running is a data race.
func newPushProxy(dialer PeerDialer, target DeliveryTarget, registryPort uint16, tlsCfg *tls.Config) (*pushProxy, error) {
	credential, err := newPushCredential()
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("starting push proxy: %w", err)
	}
	return &pushProxy{
		addr: ln.Addr().String(),
		ln:   ln,
		// Serving replaces this with the server's own shutdown. Set here so a
		// proxy that is constructed and never served is still closable, rather
		// than leaking the listener through a nil func.
		stop:       func() { _ = ln.Close() },
		credential: credential,
		assetUUID:  target.AssetUUID,
		dial: func(ctx context.Context) (net.Conn, error) {
			if tlsCfg == nil {
				return nil, errors.New("missing registry TLS credentials")
			}
			raw, mode, err := dialer.DialTarget(ctx, target, registryPort)
			if err != nil {
				return nil, err
			}
			// Authenticate before selecting the path: a TCP listener advertised on
			// the LAN is only a candidate until it proves the target's principal.
			return authenticateDelivery(ctx, dialer, target, registryPort, raw, mode, func(ctx context.Context, raw net.Conn) (net.Conn, error) {
				secure := tls.Client(raw, tlsCfg)
				if err := secure.HandshakeContext(ctx); err != nil {
					return nil, err
				}
				return secure, nil
			})
		},
	}, nil
}

// newPushCredential mints the per-build password for the loopback hop.
func newPushCredential() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating the push credential: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// authorized reports whether r carries this build's credential.
//
// Constant-time comparison because the attacker this gate exists for is local
// and can retry as fast as loopback allows.
func (p *pushProxy) authorized(r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(user), []byte(pushProxyUser)) == 1 &&
		subtle.ConstantTimeCompare([]byte(pass), []byte(p.credential)) == 1
}

// serve runs the loopback registry endpoint until stop shuts it down.
//
// This is an HTTP reverse proxy rather than the byte relay it replaces, because
// a byte relay cannot tell one local connection from another: it forwards
// whatever arrives, using credentials the caller does not have. Speaking HTTP is
// what makes the credential check possible.
func (p *pushProxy) serve(ctx context.Context) {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// The transport always dials the mesh peer regardless of this host,
			// so it serves only to produce a well-formed request. The inbound
			// Host is preserved: the target's registry names images from its own
			// listen address, and the byte relay this replaces forwarded the
			// header untouched.
			pr.Out.URL.Scheme = "https"
			pr.Out.URL.Host = pr.In.Host
			pr.Out.Host = pr.In.Host
			// The loopback credential is ours, not the registry's. The registry
			// authenticates us by client certificate.
			pr.Out.Header.Del("Authorization")
			// Count only this request body's reads. BuildKit can retry a failed
			// upload and pushes several layers concurrently, so a proxy-wide total
			// would be traffic volume rather than a meaningful completion point.
			attempt := &deliveryAttempt{total: pr.Out.ContentLength}
			pr.Out = pr.Out.WithContext(context.WithValue(
				pr.Out.Context(), deliveryAttemptContextKey{}, attempt))
			if pr.Out.Body != nil {
				pr.Out.Body = &deliveryBodyCounter{
					deliveryReader: &deliveryReader{inner: pr.Out.Body, c: &attempt.consumed},
					closer:         pr.Out.Body,
				}
			}
		},
		Transport: &http.Transport{
			// BuildKit retries registry 5xx responses at the descriptor layer,
			// where it still has the source content needed to replay a request.
			// The proxy must not stack another retry ladder underneath it: that
			// multiplies attempts and still cannot resume a partially sent body.
			DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return p.dial(ctx)
			},
			// Registry pushes are HTTP/1.1 with sized bodies; the byte relay this
			// replaces never negotiated h2, so do not start now.
			ForceAttemptHTTP2:   false,
			MaxIdleConnsPerHost: 8,
			// Bound the age of an idle pooled tunnel to reduce stale reuse. This
			// cannot detect a dead tunnel immediately or prevent the first failed
			// write; BuildKit handles the resulting 502 at the request layer.
			IdleConnTimeout:       pushIdleConnTimeout,
			ResponseHeaderTimeout: 0,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// Same reason the byte relay recorded its dial failure: buildkit
			// would otherwise see only a broken loopback connection, which
			// cannot distinguish an unreachable peer from a rejected certificate.
			attempt, _ := r.Context().Value(deliveryAttemptContextKey{}).(*deliveryAttempt)
			var consumed, total int64
			if attempt != nil {
				consumed, total = attempt.consumed.bytes(), attempt.total
			}
			p.recordError(annotateDeliveryFailure(
				fmt.Errorf("reaching device %s's registry over the wendy net: %w", p.assetUUID, err),
				consumed, total))
			w.WriteHeader(http.StatusBadGateway)
		},
	}

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !p.authorized(r) {
				// The challenge is what makes this work with an ordinary
				// registry client: containerd's authorizer retries with the
				// credentials it holds for this host once it sees one.
				w.Header().Set("WWW-Authenticate", `Basic realm="wendy build push"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			rp.ServeHTTP(w, r)
		}),
		BaseContext: func(net.Listener) context.Context { return ctx },
		// A layer push can legitimately take minutes; a read/write deadline here
		// would abort large uploads rather than protect anything.
		ReadHeaderTimeout: 30 * time.Second,
	}
	p.stop = func() { _ = srv.Close() }

	go func() { _ = srv.Serve(p.ln) }()
}

// dockerConfigWithPushAuth writes a docker config.json holding the loopback
// credential and returns its directory, for DOCKER_CONFIG.
//
// A file rather than anything on the command line, deliberately: buildctl's
// argv is world-readable through /proc/<pid>/cmdline, so a credential passed as
// an argument — or embedded in the image reference — would be readable by the
// very local user this gate exists to stop. /proc/<pid>/environ is restricted to
// the process owner, and the file itself is 0600 in a 0700 directory.
func dockerConfigWithPushAuth(dir, registryHost, credential string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating the push credential directory: %w", err)
	}
	type authEntry struct {
		Auth string `json:"auth"`
	}
	cfg := struct {
		Auths map[string]authEntry `json:"auths"`
	}{
		Auths: map[string]authEntry{
			registryHost: {Auth: base64.StdEncoding.EncodeToString([]byte(pushProxyUser + ":" + credential))},
		},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encoding the push credential: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		return fmt.Errorf("writing the push credential: %w", err)
	}
	return nil
}
