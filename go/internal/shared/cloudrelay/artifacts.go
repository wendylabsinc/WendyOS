// Package cloudrelay implements the Cloud-authorized wendycloud.tunnel.v2
// protocol. Broker connections carry opaque leases and proofs, never identities.
package cloudrelay

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"google.golang.org/grpc"
)

const leaseType = "tunnel-presence-lease+jwt"
const grantType = "tunnel-session-grant+jws"

var b64 = base64.RawURLEncoding.Strict()

func digest(b []byte) []byte  { h := sha256.Sum256(b); return h[:] }
func binding(b []byte) string { return b64.EncodeToString(digest(b)) }
func canonical(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = binary.BigEndian.AppendUint32(out, uint32(len(p)))
		out = append(out, p...)
	}
	return out
}
func canonicalJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte{'\n'}), nil
}
func decode64(s string) ([]byte, error) {
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return nil, fmt.Errorf("invalid base64url")
		}
	}
	b, err := b64.DecodeString(s)
	if err != nil || len(b) == 0 {
		return nil, fmt.Errorf("invalid base64url")
	}
	return b, nil
}
func bounded(s string, max int) bool {
	if len(s) == 0 || len(s) > max {
		return false
	}
	for _, c := range s {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}

type claims struct {
	Iss         string `json:"iss"`
	Aud         string `json:"aud"`
	Iat         int64  `json:"iat"`
	Exp         int64  `json:"exp"`
	JTI         string `json:"jti"`
	Correlation string `json:"correlation_id"`
	Route       string `json:"routing_handle"`
	Signing     string `json:"agent_signing_key_binding,omitempty"`
	Encryption  string `json:"agent_encryption_key_binding,omitempty"`
	Session     string `json:"session_id,omitempty"`
	Presence    string `json:"presence_jti,omitempty"`
	Request     string `json:"request_hash,omitempty"`
	Attestation string `json:"principal_attestation_hash,omitempty"`
	Dial        string `json:"dial_instruction_hash,omitempty"`
	Caller      string `json:"caller_key_binding,omitempty"`
	Agent       string `json:"agent_key_binding,omitempty"`
	RelayExp    int64  `json:"relay_exp,omitempty"`
}
type jwk struct {
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	Pub string `json:"pub"`
	Use string `json:"use"`
}

// Verifier obtains keys only from the configured Cloud issuer, never from a
// lease, a broker-supplied URL, or a JWS header. Unknown kids refresh the cache.
type Verifier struct {
	relayDial func(string) (*grpc.ClientConn, error)
	Issuer    string
	HTTP      *http.Client
	mu        sync.Mutex
	keys      map[string]jwk
	fetched   time.Time
}

func (v *Verifier) key(ctx context.Context, kid string) (jwk, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if k, ok := v.keys[kid]; ok && time.Since(v.fetched) < 5*time.Minute {
		return k, nil
	}
	// Bound refresh frequency, including unknown-kid failures from the broker.
	if time.Since(v.fetched) < 5*time.Second {
		return jwk{}, fmt.Errorf("unknown Cloud grant signing key")
	}
	client := v.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimSuffix(v.Issuer, "/")+"/.well-known/wendy-cloud-grants/jwks.json", nil)
	if err != nil {
		return jwk{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return jwk{}, fmt.Errorf("fetching Cloud grant keys: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return jwk{}, fmt.Errorf("Cloud grant JWKS returned HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 128*1024+1))
	if err != nil || len(raw) > 128*1024 {
		return jwk{}, fmt.Errorf("invalid Cloud grant JWKS")
	}
	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if json.Unmarshal(raw, &doc) != nil || len(doc.Keys) > 32 {
		return jwk{}, fmt.Errorf("invalid Cloud grant JWKS")
	}
	keys := map[string]jwk{}
	for _, k := range doc.Keys {
		if !bounded(k.Kid, 128) {
			continue
		}
		if _, ok := keys[k.Kid]; ok {
			return jwk{}, fmt.Errorf("duplicate Cloud grant key ID")
		}
		keys[k.Kid] = k
	}
	v.keys = keys
	v.fetched = time.Now()
	k, ok := keys[kid]
	if !ok {
		return jwk{}, fmt.Errorf("unknown Cloud grant signing key")
	}
	return k, nil
}
func (v *Verifier) verify(ctx context.Context, compact, typ, audience string, now time.Time) (claims, error) {
	var c claims
	max := 16384
	if typ == leaseType {
		max = 8192
	}
	if len(compact) > max {
		return c, fmt.Errorf("oversized Cloud artifact")
	}
	p := strings.Split(compact, ".")
	if len(p) != 3 {
		return c, fmt.Errorf("invalid Cloud artifact")
	}
	h, err := decode64(p[0])
	if err != nil {
		return c, err
	}
	body, err := decode64(p[1])
	if err != nil {
		return c, err
	}
	sig, err := decode64(p[2])
	if err != nil {
		return c, err
	}
	var hdr map[string]string
	if json.Unmarshal(h, &hdr) != nil || len(hdr) != 3 || hdr["typ"] != typ || !bounded(hdr["kid"], 128) {
		return c, fmt.Errorf("invalid Cloud artifact header")
	}
	encoded, _ := canonicalJSON(hdr)
	if !bytes.Equal(h, encoded) {
		return c, fmt.Errorf("noncanonical Cloud artifact header")
	}
	k, err := v.key(ctx, hdr["kid"])
	if err != nil {
		return c, err
	}
	input := []byte(p[0] + "." + p[1])
	valid := false
	if k.Use != "" && k.Use != "sig" {
		return c, fmt.Errorf("Cloud key is not a signing key")
	}
	switch {
	case k.Kty == "AKP" && k.Alg == "ML-DSA-65" && hdr["alg"] == "ML-DSA-65":
		raw, e := decode64(k.Pub)
		if e == nil {
			var pub mldsa65.PublicKey
			if pub.UnmarshalBinary(raw) == nil {
				valid = mldsa65.Verify(&pub, input, nil, sig)
			}
		}
	case k.Kty == "EC" && k.Crv == "P-256" && (k.Alg == "" || k.Alg == "ES256") && hdr["alg"] == "ES256":
		x, ex := decode64(k.X)
		y, ey := decode64(k.Y)
		if ex == nil && ey == nil && len(x) == 32 && len(y) == 32 && len(sig) == 64 {
			pub := ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
			if pub.Curve.IsOnCurve(pub.X, pub.Y) {
				valid = ecdsa.Verify(&pub, digest(input), new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
			}
		}
	}
	if !valid {
		return c, fmt.Errorf("invalid Cloud artifact signature")
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return c, fmt.Errorf("invalid Cloud artifact claims")
	}
	expected := []string{"iss", "aud", "iat", "exp", "jti", "correlation_id", "routing_handle"}
	if typ == leaseType {
		expected = append(expected, "agent_signing_key_binding", "agent_encryption_key_binding")
	} else {
		expected = append(expected, "session_id", "presence_jti", "request_hash", "principal_attestation_hash", "dial_instruction_hash", "caller_key_binding", "agent_key_binding", "relay_exp")
	}
	if len(obj) != len(expected) {
		return c, fmt.Errorf("unexpected Cloud artifact claims")
	}
	for _, s := range expected {
		if _, ok := obj[s]; !ok {
			return c, fmt.Errorf("missing Cloud artifact claim")
		}
	}
	// Re-encoding rejects duplicate members and noncanonical values before use.
	if err = json.Unmarshal(body, &c); err != nil {
		return c, fmt.Errorf("invalid Cloud artifact claims")
	}
	raw, _ := json.Marshal(c)
	var sorted map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	_ = dec.Decode(&sorted)
	raw, _ = canonicalJSON(sorted)
	if !bytes.Equal(raw, body) {
		return c, fmt.Errorf("noncanonical Cloud artifact claims")
	}
	ttl := int64(60)
	if typ == leaseType {
		ttl = 900
	}
	if c.Iss != v.Issuer || c.Aud != audience || !bounded(audience, 512) {
		return c, fmt.Errorf("Cloud artifact issuer or audience mismatch")
	}
	if c.Iat < 0 || c.Exp <= c.Iat || c.Exp-c.Iat > ttl || c.Iat > now.Unix() || c.Exp <= now.Unix() {
		return c, fmt.Errorf("Cloud artifact outside validity window")
	}
	if !bounded(c.JTI, 128) || !bounded(c.Route, 128) || !bounded(c.Correlation, 128) {
		return c, fmt.Errorf("invalid Cloud artifact identifiers")
	}
	hashes := []string{c.Signing, c.Encryption}
	if typ == grantType {
		hashes = []string{c.Request, c.Attestation, c.Dial, c.Caller, c.Agent}
		if !bounded(c.Session, 128) || !bounded(c.Presence, 128) || c.RelayExp <= c.Exp || c.RelayExp-c.Iat > 3600 {
			return c, fmt.Errorf("invalid Cloud session lifetime or identifier")
		}
	}
	for _, s := range hashes {
		b, e := decode64(s)
		if e != nil || len(b) != 32 {
			return c, fmt.Errorf("invalid Cloud artifact key/hash binding")
		}
	}
	return c, nil
}
func proof(key *ecdsa.PrivateKey, domain, audience, id, jti, artifact, role string, ch *pb.BrokerChallenge) ([]byte, error) {
	if ch == nil || len(ch.ChallengeId) != 16 || len(ch.Nonce) != 32 || ch.ExpiresAt == nil || ch.ExpiresAt.CheckValid() != nil {
		return nil, fmt.Errorf("invalid broker challenge")
	}
	now := time.Now()
	expiry := ch.ExpiresAt.AsTime()
	if !expiry.After(now) || expiry.After(now.Add(30*time.Second)) {
		return nil, fmt.Errorf("expired or excessive broker challenge")
	}
	input := canonical([]byte("wendycloud.tunnel.v1/"+domain+"-proof/1"), []byte(audience), []byte(id), []byte(jti), digest([]byte(artifact)), []byte(role), ch.ChallengeId, ch.Nonce)
	r, s, err := ecdsa.Sign(rand.Reader, key, digest(input))
	if err != nil {
		return nil, err
	}
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	s.FillBytes(out[32:])
	return out, nil
}
func publicDER(key *ecdsa.PrivateKey) []byte {
	raw, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	return raw
}

func (v *Verifier) dialRelay(endpoint string) (*grpc.ClientConn, error) {
	if v.relayDial != nil {
		return v.relayDial(endpoint)
	}
	return DialBroker(endpoint)
}
