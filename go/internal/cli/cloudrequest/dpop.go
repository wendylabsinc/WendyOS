package cloudrequest

import (
	"context"
	"crypto"
	"fmt"

	wendy "github.com/wendylabsinc/wendy-go"
	"github.com/wendylabsinc/wendy-go/cloud"
	"github.com/wendylabsinc/wendy-go/identity"
	"github.com/wendylabsinc/wendy/go/internal/cli/identityhost"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// OperatorPublicJWK retains the CLI key API while WendyGo owns proof encoding.
func OperatorPublicJWK(key crypto.Signer) (map[string]string, string, error) {
	signer, err := identityhost.OperatorKey(key)
	if err != nil {
		return nil, "", err
	}
	return identity.OperatorPublicJWK(context.Background(), signer)
}

// OperatorJWKThumbprint computes the shared RFC 7638/9964 key binding.
func OperatorJWKThumbprint(key crypto.Signer) (string, error) {
	signer, err := identityhost.OperatorKey(key)
	if err != nil {
		return "", err
	}
	return identity.OperatorJWKThumbprint(context.Background(), signer)
}

// NewDPoPProof delegates token-endpoint proofs to the shared protocol.
func NewDPoPProof(key crypto.Signer, method, target, nonce string) (string, error) {
	signer, err := identityhost.OperatorKey(key)
	if err != nil {
		return "", err
	}
	return (&identity.Client{Entropy: wendy.CryptoEntropy{}}).DPoPProof(context.Background(), signer, method, target, nonce)
}

// NewDPoPAccessProof binds the shared proof to exact access-token bytes.
func NewDPoPAccessProof(key crypto.Signer, method, target, access string) (string, error) {
	if access == "" {
		return "", fmt.Errorf("access token is empty")
	}
	signer, err := identityhost.OperatorKey(key)
	if err != nil {
		return "", err
	}
	return (&identity.Client{Entropy: wendy.CryptoEntropy{}}).DPoPAccessProof(context.Background(), signer, method, target, access)
}

// SignJWS serializes a compact JWS through WendyGo and the host signer.
func SignJWS(key crypto.Signer, header, payload map[string]any) (string, error) {
	signer, err := identityhost.OperatorKey(key)
	if err != nil {
		return "", err
	}
	return identity.SignJWS(context.Background(), signer, header, payload)
}

// CanonicalHTU strips query and fragment as RFC 9449 requires.
func CanonicalHTU(raw string) (string, error) { return identity.CanonicalHTU(raw) }

// IsDPoPBound reports whether the session's access token is sender-constrained
// to a stored ML-DSA DPoP key (an OAuth login, post-WDY-3032). Such a token must
// be presented as DPoP with a per-call proof, never as Bearer (RFC 9449 §7.2).
// A self-hosted API-key session or a legacy token session is not bound.
func IsDPoPBound(auth *config.AuthConfig) bool {
	return auth != nil && auth.OAuthIssuer != "" && auth.DPoPPrivateKey != ""
}

// DPoPTokenProvider returns the current access token and its bound signing key
// for a per-RPC proof. The CLI's provider refreshes an expired token; the MCP
// provider returns the stored token as-is (it does not auto-refresh).
type DPoPTokenProvider func(ctx context.Context) (token string, key crypto.Signer, err error)

// DPoPDialOptions installs per-RPC DPoP proof injection (unary + streaming) for
// a sender-constrained session. The proof is built per RPC — its htu is the
// invoked gRPC method path at the broker host — so it is built in the
// interceptor, where the method is known, not when the context is assembled.
// Unbound sessions (no provider, or not cnf-bound) return nil and keep Bearer.
func DPoPDialOptions(auth *config.AuthConfig, provider DPoPTokenProvider) []grpc.DialOption {
	if !IsDPoPBound(auth) || provider == nil {
		return nil
	}
	return []grpc.DialOption{
		grpc.WithChainUnaryInterceptor(dpopUnaryInterceptor(auth, provider)),
		grpc.WithChainStreamInterceptor(dpopStreamInterceptor(auth, provider)),
	}
}

// dpopAuthContext resolves a fresh token+key, builds a proof bound to
// (method, token), and returns a context whose outgoing metadata carries
// `authorization: DPoP <token>` and `dpop: <proof>`, overwriting any Bearer a
// caller may have set so a bound token never leaves as Bearer.
func dpopAuthContext(ctx context.Context, auth *config.AuthConfig, provider DPoPTokenProvider, fullMethod string) (context.Context, error) {
	token, key, err := provider(ctx)
	if err != nil {
		return nil, err
	}
	var signer wendy.Signer
	if token != "" && key != nil {
		wrapped, err := identityhost.OperatorKey(key)
		if err != nil {
			return nil, fmt.Errorf("DPoP: building proof: %w", err)
		}
		signer = wrapped
	}
	md, _ := metadata.FromOutgoingContext(ctx)
	signed, err := cloud.AuthenticationMetadata(ctx, wendy.CryptoEntropy{}, auth.CloudGRPC, fullMethod, token, signer, coreMetadata(md))
	if err != nil {
		return nil, err
	}
	return metadata.NewOutgoingContext(ctx, grpcMetadata(signed)), nil
}

func dpopUnaryInterceptor(auth *config.AuthConfig, provider DPoPTokenProvider) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx, err := dpopAuthContext(ctx, auth, provider, method)
		if err != nil {
			return err
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

func dpopStreamInterceptor(auth *config.AuthConfig, provider DPoPTokenProvider) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		ctx, err := dpopAuthContext(ctx, auth, provider, method)
		if err != nil {
			return nil, err
		}
		return streamer(ctx, desc, cc, method, opts...)
	}
}
