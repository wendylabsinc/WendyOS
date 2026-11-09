package delivery

import (
	"context"
	"crypto/x509"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/builddelivery"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type Grant struct {
	store   *builddelivery.Store
	session string
	leaf    *x509.Certificate
	claims  builddelivery.Claims
	prefix  string
}
type grantKey struct{}

func Leaf(ctx context.Context) *x509.Certificate {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || !info.State.HandshakeComplete || len(info.State.PeerCertificates) == 0 {
		return nil
	}
	return info.State.PeerCertificates[0]
}

// RPC runs after the existing mTLS/role interceptors. Ambient operator and
// local-admin behavior remains there; every device staging call additionally
// requires a durable grant bound to this connection's actual certificate key.
func RPC(ctx context.Context, source Source, imageName string) (context.Context, *Grant, error) {
	leaf := Leaf(ctx)
	if leaf == nil {
		return ctx, nil, nil
	}
	identity, found, err := certs.IdentityFromCert(leaf)
	if err != nil {
		return ctx, nil, status.Error(codes.PermissionDenied, "invalid delivery caller")
	}
	if found && identity.EntityType == certs.EntityUser {
		return ctx, nil, nil
	}
	if !found || identity.EntityType != certs.EntityAsset {
		return ctx, nil, status.Error(codes.PermissionDenied, "delivery requires an operator or an authorized builder")
	}
	if !time.Now().Before(leaf.NotAfter) {
		return ctx, nil, status.Error(codes.Unauthenticated, "builder certificate expired")
	}
	if source == nil {
		return ctx, nil, status.Error(codes.PermissionDenied, "operator delivery authorization required")
	}
	snapshot, err := source()
	if err != nil || snapshot.Store == nil {
		return ctx, nil, status.Error(codes.FailedPrecondition, "operator delivery authorization is unavailable")
	}
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get(builddelivery.MetadataKey)
	if len(values) != 1 || values[0] == "" {
		return ctx, nil, status.Error(codes.PermissionDenied, "one operator delivery session is required")
	}
	claims, err := snapshot.Store.Policy(values[0], leaf)
	if err != nil {
		return ctx, nil, status.Error(codes.PermissionDenied, err.Error())
	}
	if imageName != "" && imageName != snapshot.ImagePrefix+claims.Reference {
		return ctx, nil, status.Error(codes.PermissionDenied, "image reference is outside the operator delivery grant")
	}
	grant := &Grant{store: snapshot.Store, session: values[0], leaf: leaf, claims: claims, prefix: snapshot.ImagePrefix}
	// Handlers apply Deadline to their work and release its timer on return.
	return context.WithValue(ctx, grantKey{}, grant), grant, nil
}
func (g *Grant) Deadline() time.Time {
	deadline := time.Unix(g.claims.ValidUntil, 0)
	if g.leaf.NotAfter.Before(deadline) {
		return g.leaf.NotAfter
	}
	return deadline
}
func (g *Grant) ReserveBytes(n int64) error {
	if err := g.store.ReserveBytes(g.session, g.leaf, n); err != nil {
		return status.Error(codes.PermissionDenied, err.Error())
	}
	return nil
}
func (g *Grant) AuthorizeStaging() error {
	_, err := g.store.Authorize(g.session, g.leaf, "")
	if err != nil {
		return status.Error(codes.PermissionDenied, err.Error())
	}
	return nil
}
func FromContext(ctx context.Context) *Grant { g, _ := ctx.Value(grantKey{}).(*Grant); return g }

// Commit is the single final image-reference mutation boundary. CAS and the
// output choice are serialized against both transports and ambient writers.
// current returns an empty digest for an absent reference.
func Commit(ctx context.Context, source Source, imageName, output string, current func() (string, error), apply func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if g := FromContext(ctx); g != nil {
		if imageName != g.prefix+g.claims.Reference {
			return errors.New("publication reference is outside delivery grant")
		}
		return g.store.Publish(g.session, g.leaf, g.claims.Reference, output, func(expected, output string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			actual, err := current()
			if err != nil {
				return err
			}
			if actual == output {
				return nil
			}
			if actual != expected {
				return errors.New("image reference changed since operator authorization")
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			return apply()
		})
	}
	if source == nil {
		return apply()
	}
	snapshot, err := source()
	if err != nil {
		return err
	}
	if snapshot.Store == nil {
		return apply()
	}
	return snapshot.Store.Direct(apply)
}

func (g *Grant) Reference() string  { return g.claims.Reference }
func (g *Grant) Repository() string { name, _, _ := strings.Cut(g.claims.Reference, ":"); return name }
func (g *Grant) NewUpload() (string, error) {
	id := uuid.NewString()
	if err := g.store.RegisterUpload(g.session, g.leaf, id); err != nil {
		return "", err
	}
	return "delivery-" + g.session + "-" + id, nil
}
func (g *Grant) CheckUpload(id string) error {
	prefix := "delivery-" + g.session + "-"
	if !strings.HasPrefix(id, prefix) {
		return errors.New("upload belongs to another delivery session")
	}
	return g.store.CheckUpload(g.session, g.leaf, strings.TrimPrefix(id, prefix))
}

func (g *Grant) CompletedOutput() (string, bool, error) {
	return g.store.CompletedOutput(g.session, g.leaf)
}
