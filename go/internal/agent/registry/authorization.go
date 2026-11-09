package registry

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"cuelabs.dev/go/oci/ociregistry"
	digest "github.com/opencontainers/go-digest"
	"github.com/wendylabsinc/wendy/go/internal/agent/delivery"
	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/shared/builddelivery"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

const maxManifestBody = 4 * 1024 * 1024

type accountedBodyKey struct{}

func bodyAccounted(ctx context.Context) bool {
	accounted, _ := ctx.Value(accountedBodyKey{}).(bool)
	return accounted
}

func authorizationMiddleware(source delivery.Source, mode interceptor.OrgMode) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			snapshot, err := source()
			if err != nil {
				_ = ociregistry.WriteError(w, ociregistry.ErrDenied)
				return
			}
			if strings.Contains(r.URL.Path, "/manifests/") {
				r.Body = http.MaxBytesReader(w, r.Body, maxManifestBody)
			}
			if r.TLS == nil {
				// Preserve only the genuinely unprovisioned bootstrap registry. Failure to
				// construct TLS on an enrolled target cannot expose an HTTP write bypass.
				if snapshot.Provisioned {
					_ = ociregistry.WriteError(w, ociregistry.ErrUnauthorized)
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			if !r.TLS.HandshakeComplete || len(r.TLS.PeerCertificates) == 0 {
				_ = ociregistry.WriteError(w, ociregistry.ErrUnauthorized)
				return
			}
			leaf := r.TLS.PeerCertificates[0]
			if !time.Now().Before(leaf.NotAfter) {
				_ = ociregistry.WriteError(w, ociregistry.ErrUnauthorized)
				return
			}
			identity, found, err := certs.IdentityFromCert(leaf)
			if err != nil {
				_ = ociregistry.WriteError(w, ociregistry.ErrDenied)
				return
			}
			if !found || identity.EntityType != certs.EntityAsset {
				if err = interceptor.CheckOperatorCertificate(leaf, snapshot.Scope, mode); err != nil {
					_ = ociregistry.WriteError(w, ociregistry.ErrDenied)
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			if !snapshot.Scope.Known() || !identity.Scope().Matches(snapshot.Scope) {
				_ = ociregistry.WriteError(w, ociregistry.ErrDenied)
				return
			}
			if (r.URL.Path == "/v2/" || r.URL.Path == "/readyz") && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
				next.ServeHTTP(w, r)
				return
			}
			ctx := peer.NewContext(r.Context(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: *r.TLS}})
			ctx = metadata.NewIncomingContext(ctx, metadata.MD{builddelivery.MetadataKey: r.Header.Values(builddelivery.HTTPHeader)})
			ctx, grant, err := delivery.RPC(ctx, source, "")
			if err != nil || grant == nil {
				_ = ociregistry.WriteError(w, ociregistry.ErrDenied)
				return
			}
			ctx, cancel := context.WithDeadline(ctx, grant.Deadline())
			defer cancel()
			// These are the operator's absolute job bounds, not an idle timeout on a
			// legitimately slow multi-GB transfer. net/http resets them for later requests.
			controller := http.NewResponseController(w)
			_ = controller.SetReadDeadline(grant.Deadline())
			_ = controller.SetWriteDeadline(grant.Deadline())
			defer func() {
				// net/http may drain an unread body after this handler returns.
				// Clearing an expired deadline would let that drain block again.
				if ctx.Err() == nil && time.Now().Before(grant.Deadline()) {
					_ = controller.SetReadDeadline(time.Time{})
					_ = controller.SetWriteDeadline(time.Time{})
				}
			}()
			switch r.Method {
			case http.MethodPost, http.MethodPatch, http.MethodPut:
				_, completed, stateErr := grant.CompletedOutput()
				if stateErr != nil {
					_ = ociregistry.WriteError(w, ociregistry.ErrDenied)
					return
				}
				if completed {
					// Only a bounded, read-only final-manifest replay remains.
					// The backend checks the exact tag and current output digest.
					if r.Method != http.MethodPut || !strings.Contains(r.URL.Path, "/manifests/") {
						_ = ociregistry.WriteError(w, ociregistry.ErrDenied)
						return
					}
				} else {
					if err = grant.AuthorizeStaging(); err != nil {
						_ = ociregistry.WriteError(w, ociregistry.ErrDenied)
						return
					}
					if r.ContentLength >= 0 {
						if err = grant.ReserveBytes(r.ContentLength); err != nil {
							_ = ociregistry.WriteError(w, ociregistry.ErrDenied)
							return
						}
					} else {
						r.Body = &chargedBody{ReadCloser: r.Body, grant: grant}
					}
				}
				ctx = context.WithValue(ctx, accountedBodyKey{}, true)
			case http.MethodGet, http.MethodHead, http.MethodDelete:
			default:
				_ = ociregistry.WriteError(w, ociregistry.ErrDenied)
				return
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

type chargedBody struct {
	io.ReadCloser
	grant *delivery.Grant
}

func (b *chargedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		if e := b.grant.ReserveBytes(int64(n)); e != nil {
			return 0, ociregistry.ErrDenied
		}
	}
	return n, err
}
func repoGrant(ctx context.Context, repo string, staging bool) (*delivery.Grant, error) {
	g := delivery.FromContext(ctx)
	if g == nil {
		return nil, nil
	}
	if g.Repository() != repo {
		return nil, ociregistry.ErrDenied
	}
	if staging {
		if err := g.AuthorizeStaging(); err != nil {
			return nil, ociregistry.ErrDenied
		}
	}
	return g, nil
}
func charge(ctx context.Context, g *delivery.Grant, n int64) error {
	if g != nil && !bodyAccounted(ctx) {
		if err := g.ReserveBytes(n); err != nil {
			return ociregistry.ErrDenied
		}
	}
	return nil
}

type chargedReader struct {
	io.Reader
	ctx   context.Context
	grant *delivery.Grant
}

func (r chargedReader) Read(p []byte) (int, error) {
	if err := r.grant.AuthorizeStaging(); err != nil {
		return 0, ociregistry.ErrDenied
	}
	n, err := r.Reader.Read(p)
	if n > 0 {
		if e := charge(r.ctx, r.grant, int64(n)); e != nil {
			return 0, e
		}
	}
	return n, err
}

type authorizedBlobWriter struct {
	ociregistry.BlobWriter
	ctx   context.Context
	grant *delivery.Grant
}

func (w authorizedBlobWriter) Write(data []byte) (int, error) {
	if err := w.grant.AuthorizeStaging(); err != nil {
		return 0, ociregistry.ErrDenied
	}
	if err := charge(w.ctx, w.grant, int64(len(data))); err != nil {
		return 0, err
	}
	return w.BlobWriter.Write(data)
}
func (w authorizedBlobWriter) Commit(d ociregistry.Digest) (ociregistry.Descriptor, error) {
	if err := w.grant.AuthorizeStaging(); err != nil {
		return ociregistry.Descriptor{}, ociregistry.ErrDenied
	}
	return w.BlobWriter.Commit(d)
}

// Explicit Funcs leave future registry operations unsupported until reviewed.
// Devices receive metadata probes and scoped delivery writes, never ambient
// blob downloads, catalog enumeration or deletion authority.
func newAuthorizedRegistry(backend ociregistry.Interface) ociregistry.Interface {
	f := &ociregistry.Funcs{}
	f.GetBlob_ = func(ctx context.Context, repo string, d ociregistry.Digest) (ociregistry.BlobReader, error) {
		if delivery.FromContext(ctx) != nil {
			return nil, ociregistry.ErrDenied
		}
		return backend.GetBlob(ctx, repo, d)
	}
	f.GetBlobRange_ = func(ctx context.Context, repo string, d ociregistry.Digest, a, b int64) (ociregistry.BlobReader, error) {
		if delivery.FromContext(ctx) != nil {
			return nil, ociregistry.ErrDenied
		}
		return backend.GetBlobRange(ctx, repo, d, a, b)
	}
	f.GetManifest_ = func(ctx context.Context, repo string, d ociregistry.Digest) (ociregistry.BlobReader, error) {
		if delivery.FromContext(ctx) != nil {
			return nil, ociregistry.ErrDenied
		}
		return backend.GetManifest(ctx, repo, d)
	}
	f.GetTag_ = func(ctx context.Context, repo, tag string) (ociregistry.BlobReader, error) {
		if delivery.FromContext(ctx) != nil {
			return nil, ociregistry.ErrDenied
		}
		return backend.GetTag(ctx, repo, tag)
	}
	f.ResolveBlob_ = func(ctx context.Context, repo string, d ociregistry.Digest) (ociregistry.Descriptor, error) {
		if _, err := repoGrant(ctx, repo, false); err != nil {
			return ociregistry.Descriptor{}, err
		}
		return backend.ResolveBlob(ctx, repo, d)
	}
	f.ResolveManifest_ = func(ctx context.Context, repo string, d ociregistry.Digest) (ociregistry.Descriptor, error) {
		if _, err := repoGrant(ctx, repo, false); err != nil {
			return ociregistry.Descriptor{}, err
		}
		return backend.ResolveManifest(ctx, repo, d)
	}
	f.ResolveTag_ = func(ctx context.Context, repo, tag string) (ociregistry.Descriptor, error) {
		g, err := repoGrant(ctx, repo, false)
		if err != nil {
			return ociregistry.Descriptor{}, err
		}
		if g != nil && repo+":"+tag != g.Reference() {
			return ociregistry.Descriptor{}, ociregistry.ErrDenied
		}
		return backend.ResolveTag(ctx, repo, tag)
	}
	f.Repositories_ = func(ctx context.Context, after string) ociregistry.Seq[string] {
		if delivery.FromContext(ctx) != nil {
			return ociregistry.ErrorSeq[string](ociregistry.ErrDenied)
		}
		return backend.Repositories(ctx, after)
	}
	f.Tags_ = func(ctx context.Context, repo, after string) ociregistry.Seq[string] {
		if delivery.FromContext(ctx) != nil {
			return ociregistry.ErrorSeq[string](ociregistry.ErrDenied)
		}
		return backend.Tags(ctx, repo, after)
	}
	f.Referrers_ = func(ctx context.Context, repo string, d ociregistry.Digest, kind string) ociregistry.Seq[ociregistry.Descriptor] {
		if delivery.FromContext(ctx) != nil {
			return ociregistry.ErrorSeq[ociregistry.Descriptor](ociregistry.ErrDenied)
		}
		return backend.Referrers(ctx, repo, d, kind)
	}
	f.DeleteBlob_ = func(context.Context, string, ociregistry.Digest) error { return ociregistry.ErrUnsupported }
	f.DeleteManifest_ = func(context.Context, string, ociregistry.Digest) error { return ociregistry.ErrUnsupported }
	f.DeleteTag_ = func(context.Context, string, string) error { return ociregistry.ErrUnsupported }
	f.PushBlob_ = func(ctx context.Context, repo string, d ociregistry.Descriptor, r io.Reader) (ociregistry.Descriptor, error) {
		g, err := repoGrant(ctx, repo, true)
		if err != nil {
			return ociregistry.Descriptor{}, err
		}
		if g != nil {
			r = chargedReader{Reader: r, ctx: ctx, grant: g}
		}
		return backend.PushBlob(ctx, repo, d, r)
	}
	f.PushBlobChunked_ = func(ctx context.Context, repo string, size int) (ociregistry.BlobWriter, error) {
		g, err := repoGrant(ctx, repo, true)
		if err != nil {
			return nil, err
		}
		if g == nil {
			return backend.PushBlobChunked(ctx, repo, size)
		}
		id, err := g.NewUpload()
		if err != nil {
			return nil, ociregistry.ErrDenied
		}
		writer, err := backend.PushBlobChunkedResume(ctx, repo, id, 0, size)
		if err != nil {
			return nil, err
		}
		return authorizedBlobWriter{writer, ctx, g}, nil
	}
	f.PushBlobChunkedResume_ = func(ctx context.Context, repo, id string, offset int64, size int) (ociregistry.BlobWriter, error) {
		g, err := repoGrant(ctx, repo, true)
		if err != nil {
			return nil, err
		}
		if g != nil {
			if err = g.CheckUpload(id); err != nil {
				return nil, ociregistry.ErrDenied
			}
		}
		writer, err := backend.PushBlobChunkedResume(ctx, repo, id, offset, size)
		if err != nil {
			return nil, err
		}
		if g == nil {
			return writer, nil
		}
		return authorizedBlobWriter{writer, ctx, g}, nil
	}
	f.MountBlob_ = func(ctx context.Context, from, to string, d ociregistry.Digest) (ociregistry.Descriptor, error) {
		g, err := repoGrant(ctx, to, true)
		if err != nil {
			return ociregistry.Descriptor{}, err
		}
		if g != nil && from != g.Repository() {
			return ociregistry.Descriptor{}, ociregistry.ErrBlobUnknown
		}
		return backend.MountBlob(ctx, from, to, d)
	}
	f.PushManifest_ = func(ctx context.Context, repo, tag string, data []byte, kind string) (ociregistry.Descriptor, error) {
		g, err := repoGrant(ctx, repo, false)
		if err != nil {
			return ociregistry.Descriptor{}, err
		}
		if len(data) > maxManifestBody {
			return ociregistry.Descriptor{}, ociregistry.ErrSizeInvalid
		}
		if g != nil && tag != "" && repo+":"+tag != g.Reference() {
			return ociregistry.Descriptor{}, ociregistry.ErrDenied
		}
		if g != nil {
			output, completed, err := g.CompletedOutput()
			if err != nil {
				return ociregistry.Descriptor{}, ociregistry.ErrDenied
			}
			if completed {
				if tag == "" || digest.FromBytes(data).String() != output {
					return ociregistry.Descriptor{}, ociregistry.ErrDenied
				}
				current, err := backend.ResolveTag(ctx, repo, tag)
				if err != nil {
					return ociregistry.Descriptor{}, err
				}
				if current.Digest.String() != output {
					return ociregistry.Descriptor{}, ociregistry.ErrDenied
				}
				return current, nil
			}
			if err := g.AuthorizeStaging(); err != nil {
				return ociregistry.Descriptor{}, ociregistry.ErrDenied
			}
		}
		if err = charge(ctx, g, int64(len(data))); err != nil {
			return ociregistry.Descriptor{}, err
		}
		return backend.PushManifest(ctx, repo, tag, data, kind)
	}
	return f
}
