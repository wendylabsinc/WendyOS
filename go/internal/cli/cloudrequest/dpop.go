package cloudrequest

import (
	"context"
	"crypto"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// dpopMLDSAAlg is the JWS "alg" for the operator key. It must match pki-core's
// reqsig.AlgMLDSA65 exactly — the value is carried in the DPoP header AND in the
// JWK whose RFC 7638 thumbprint has to equal the token's cnf.jkt, so a different
// spelling fails the binding rather than merely looking odd.
const dpopMLDSAAlg = "ML-DSA-65"

func dpopBase64URL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func dpopRandomURLSafe(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return dpopBase64URL(raw), nil
}

// errOperatorKeyNotMLDSA is the one message every operator-credential path gives
// for a pre-WDY-3032 session: the operator credential is ML-DSA-65 with no
// negotiation and no fallback, so a non-ML-DSA key is a session to establish
// again, not one to sign with more carefully.
func errOperatorKeyNotMLDSA(pub any) error {
	return fmt.Errorf("this session's operator key is %T, but Wendy now requires an ML-DSA-65 operator credential; re-run 'wendy auth login'", pub)
}

// mldsaPublicJWK builds the RFC 9964 AKP JWK for an ML-DSA public key. "pub" is
// the raw FIPS-204 public key; unlike EC there is no crv, and "alg" is a
// required member of the thumbprint input for this key type.
func mldsaPublicJWK(pub *mldsa.PublicKey) (map[string]string, error) {
	if pub == nil {
		return nil, fmt.Errorf("nil public key")
	}
	return map[string]string{
		"alg": dpopMLDSAAlg,
		"kty": "AKP",
		"pub": dpopBase64URL(pub.Bytes()),
	}, nil
}

// OperatorPublicJWK returns the RFC 9964 AKP JWK and JWS alg for the operator
// key. ML-DSA-65 only: WDY-3032 is a hard cutover, so a session holding the old
// ECDSA key is refused here rather than signed with.
func OperatorPublicJWK(signer crypto.Signer) (map[string]string, string, error) {
	pub, ok := signer.Public().(*mldsa.PublicKey)
	if !ok {
		return nil, "", errOperatorKeyNotMLDSA(signer.Public())
	}
	jwk, err := mldsaPublicJWK(pub)
	return jwk, dpopMLDSAAlg, err
}

// OperatorJWKThumbprint computes the RFC 7638 thumbprint used for cnf.jkt. The
// canonical JSON is the required members in lexical order with no whitespace —
// {alg,kty,pub} for AKP (RFC 9964 §5 includes alg, unlike EC). Member order is
// part of the hash input, so it is written out literally.
func OperatorJWKThumbprint(signer crypto.Signer) (string, error) {
	jwk, _, err := OperatorPublicJWK(signer)
	if err != nil {
		return "", err
	}
	canonical := fmt.Sprintf(`{"alg":"%s","kty":"%s","pub":"%s"}`, jwk["alg"], jwk["kty"], jwk["pub"])
	sum := sha256.Sum256([]byte(canonical))
	return dpopBase64URL(sum[:]), nil
}

// signOperatorJWS signs the JWS signing input with the operator key. ML-DSA
// signs the input bytes directly with empty Options, matching how pki-core
// verifies; the signature is the raw FIPS-204 value.
func signOperatorJWS(signer crypto.Signer, signingInput string) (string, error) {
	key, ok := signer.(*mldsa.PrivateKey)
	if !ok {
		return "", errOperatorKeyNotMLDSA(signer.Public())
	}
	sig, err := key.Sign(rand.Reader, []byte(signingInput), &mldsa.Options{})
	if err != nil {
		return "", fmt.Errorf("signing: %w", err)
	}
	return dpopBase64URL(sig), nil
}

// NewDPoPProof builds an RFC 9449 proof JWT for a single request. htu must be
// the request URI with query and fragment removed; htm the method. nonce is
// included only when the server has demanded one.
func NewDPoPProof(key crypto.Signer, htm, htu, nonce string) (string, error) {
	return newDPoPProofWithAccessToken(key, htm, htu, nonce, "")
}

// NewDPoPAccessProof builds the proof used at a protected resource. In addition
// to the request URI and method it binds the proof to the exact access-token
// bytes through RFC 9449's ath claim.
func NewDPoPAccessProof(key crypto.Signer, htm, htu, accessToken string) (string, error) {
	if accessToken == "" {
		return "", fmt.Errorf("access token is empty")
	}
	return newDPoPProofWithAccessToken(key, htm, htu, "", accessToken)
}

func newDPoPProofWithAccessToken(key crypto.Signer, htm, htu, nonce, accessToken string) (string, error) {
	jwk, alg, err := OperatorPublicJWK(key)
	if err != nil {
		return "", err
	}
	header := map[string]any{"typ": "dpop+jwt", "alg": alg, "jwk": jwk}
	jti, err := dpopRandomURLSafe(16)
	if err != nil {
		return "", fmt.Errorf("generating DPoP jti: %w", err)
	}
	payload := map[string]any{"jti": jti, "htm": htm, "htu": htu, "iat": time.Now().Unix()}
	if nonce != "" {
		payload["nonce"] = nonce
	}
	if accessToken != "" {
		sum := sha256.Sum256([]byte(accessToken))
		payload["ath"] = dpopBase64URL(sum[:])
	}
	return SignJWS(key, header, payload)
}

// SignJWS serializes header and payload and signs them with the operator key
// as a compact JWS. The caller sets header "alg" (see OperatorPublicJWK).
func SignJWS(key crypto.Signer, header, payload map[string]any) (string, error) {
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("marshaling JWS header: %w", err)
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshaling JWS payload: %w", err)
	}
	signingInput := dpopBase64URL(headerJSON) + "." + dpopBase64URL(payloadJSON)
	sig, err := signOperatorJWS(key, signingInput)
	if err != nil {
		return "", err
	}
	return signingInput + "." + sig, nil
}

// CanonicalHTU strips query and fragment, which RFC 9449 excludes from htu.
func CanonicalHTU(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parsing endpoint URL %q: %w", raw, err)
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

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
	if token == "" {
		return nil, fmt.Errorf("DPoP: session has no access token")
	}
	if key == nil {
		return nil, fmt.Errorf("DPoP: session has no bound key")
	}
	// htu is the RS URL: the broker authority we dialed plus the gRPC method
	// path (fullMethod already begins with "/"). No query or fragment, so it is
	// already RFC 9449-canonical.
	htu := "https://" + auth.CloudGRPC + fullMethod
	proof, err := NewDPoPAccessProof(key, http.MethodPost, htu, token)
	if err != nil {
		return nil, fmt.Errorf("DPoP: building proof: %w", err)
	}

	md, ok := metadata.FromOutgoingContext(ctx)
	if ok {
		md = md.Copy()
	} else {
		md = metadata.MD{}
	}
	md.Set("authorization", "DPoP "+token)
	md.Set("dpop", proof)
	return metadata.NewOutgoingContext(ctx, md), nil
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
