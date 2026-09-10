package cloudrelay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type vectors struct {
	Keys map[string]struct {
		Scalar string `json:"private_scalar_hex"`
		SPKI   string `json:"public_spki_der_b64"`
	} `json:"keys"`
	Artifacts struct {
		JWKS              string `json:"jwks_json"`
		Presence, Session struct {
			Claims claims `json:"claims"`
			JWS    string `json:"compact_jws"`
		}
	} `json:"cloud_artifacts"`
	Envelope struct {
		Enc        string `json:"encapsulated_key_x963_b64"`
		Info       string `json:"info_b64"`
		AAD        string `json:"aad_b64"`
		Ciphertext string `json:"ciphertext_b64"`
	} `json:"hpke_envelope"`
	Instruction struct {
		Plaintext string `json:"canonical_plaintext_b64"`
	} `json:"dial_instruction"`
	Proofs []struct {
		Name, Domain string
		Audience     string `json:"broker_audience"`
		Opaque       string `json:"opaque_id"`
		JTI          string
		Role         string
		Challenge    string `json:"challenge_id_hex"`
		Nonce        string `json:"nonce_hex"`
		Preimage     string `json:"canonical_preimage_b64"`
		Signature    string `json:"signature_raw_b64url"`
	} `json:"proofs"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, e := os.ReadFile("testdata/tunnel-v2.json")
	if e != nil {
		t.Fatal(e)
	}
	var v vectors
	if e = json.Unmarshal(raw, &v); e != nil {
		t.Fatal(e)
	}
	return v
}
func std64(t *testing.T, s string) []byte {
	t.Helper()
	b, e := base64.StdEncoding.DecodeString(s)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, e := hex.DecodeString(s)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func scalarKey(t *testing.T, s string) *ecdsa.PrivateKey {
	t.Helper()
	b := unhex(t, s)
	curve := elliptic.P256()
	x, y := curve.ScalarBaseMult(b)
	return &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y}, D: new(big.Int).SetBytes(b)}
}
func fixtureVerifier(t *testing.T, v vectors) *Verifier {
	t.Helper()
	var doc struct{ Keys []jwk }
	if e := json.Unmarshal([]byte(v.Artifacts.JWKS), &doc); e != nil {
		t.Fatal(e)
	}
	keys := map[string]jwk{}
	for _, k := range doc.Keys {
		keys[k.Kid] = k
	}
	return &Verifier{Issuer: v.Artifacts.Presence.Claims.Iss, keys: keys, fetched: time.Now()}
}
func TestCloudArtifactConformance(t *testing.T) {
	v := loadVectors(t)
	verifier := fixtureVerifier(t, v)
	now := time.Unix(v.Artifacts.Presence.Claims.Iat+1, 0)
	for _, tt := range []struct {
		name, typ, jws string
		c              claims
	}{{"presence", leaseType, v.Artifacts.Presence.JWS, v.Artifacts.Presence.Claims}, {"session", grantType, v.Artifacts.Session.JWS, v.Artifacts.Session.Claims}} {
		t.Run(tt.name, func(t *testing.T) {
			got, e := verifier.verify(context.Background(), tt.jws, tt.typ, tt.c.Aud, now)
			if e != nil {
				t.Fatal(e)
			}
			if got != tt.c {
				t.Fatal("claim mismatch")
			}
			for _, mutation := range []struct {
				name, jws, typ, aud string
				when                time.Time
			}{
				{"expired", tt.jws, tt.typ, tt.c.Aud, time.Unix(tt.c.Exp, 0)},
				{"future", tt.jws, tt.typ, tt.c.Aud, time.Unix(tt.c.Iat-1, 0)},
				{"wrong audience", tt.jws, tt.typ, tt.c.Aud + "x", now},
				{"wrong type", tt.jws, "other", tt.c.Aud, now},
				{"signature", tt.jws[:len(tt.jws)-8] + "AAAAAAAA", tt.typ, tt.c.Aud, now},
				{"whitespace", tt.jws + "\n", tt.typ, tt.c.Aud, now},
			} {
				t.Run(mutation.name, func(t *testing.T) {
					if _, e := verifier.verify(context.Background(), mutation.jws, mutation.typ, mutation.aud, mutation.when); e == nil {
						t.Fatal("accepted invalid artifact")
					}
				})
			}
		})
	}
}
func TestInstructionHPKEConformance(t *testing.T) {
	v := loadVectors(t)
	key := scalarKey(t, v.Keys["agent_key_agreement"].Scalar)
	envelope := &pb.EncryptedDialInstructionEnvelope{Suite: 1, EncapsulatedKey: std64(t, v.Envelope.Enc), Info: std64(t, v.Envelope.Info), Aad: std64(t, v.Envelope.AAD), Ciphertext: std64(t, v.Envelope.Ciphertext)}
	d, e := decryptInstruction(envelope, v.Artifacts.Session.Claims, v.Artifacts.Presence.Claims, key)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(d)
	if string(raw) != string(std64(t, v.Instruction.Plaintext)) {
		t.Fatal("decrypted bytes differ from cross-language vector")
	}
	// The fixture's custom UDP service is outside the current standard catalog.
	if validateService(d) == nil {
		t.Fatal("accepted an unsupported service")
	}
	for _, field := range []string{"ciphertext", "info", "aad", "enc", "suite", "hash", "key", "request", "session"} {
		t.Run(field, func(t *testing.T) {
			e := proto.Clone(envelope).(*pb.EncryptedDialInstructionEnvelope)
			c := v.Artifacts.Session.Claims
			k := key
			switch field {
			case "ciphertext":
				e.Ciphertext[0] ^= 1
			case "info":
				e.Info[0] ^= 1
			case "aad":
				e.Aad[0] ^= 1
			case "enc":
				e.EncapsulatedKey[1] ^= 1
			case "suite":
				e.Suite = 0
			case "hash":
				c.Dial = binding([]byte("wrong"))
			case "key":
				k = scalarKey(t, v.Keys["agent_signing"].Scalar)
			case "request":
				c.Request = binding([]byte("wrong"))
			case "session":
				c.Session = "other"
			}
			if _, err := decryptInstruction(e, c, v.Artifacts.Presence.Claims, k); err == nil {
				t.Fatal("accepted substituted HPKE material")
			}
		})
	}
}
func TestProofConformance(t *testing.T) {
	v := loadVectors(t)
	for _, p := range v.Proofs {
		t.Run(p.Name, func(t *testing.T) {
			artifact := v.Artifacts.Session.JWS
			domain := "join"
			keyName := "agent_signing"
			if p.Role == "agent-presence" {
				artifact = v.Artifacts.Presence.JWS
				domain = "presence"
			} else if p.Role == "caller" {
				keyName = "caller_ephemeral"
			}
			key := scalarKey(t, v.Keys[keyName].Scalar)
			ch := &pb.BrokerChallenge{ChallengeId: unhex(t, p.Challenge), Nonce: unhex(t, p.Nonce), ExpiresAt: timestamppb.New(time.Now().Add(10 * time.Second))}
			sig, e := proof(key, domain, p.Audience, p.Opaque, p.JTI, artifact, p.Role, ch)
			if e != nil {
				t.Fatal(e)
			}
			preimage := std64(t, p.Preimage)
			if !ecdsa.Verify(&key.PublicKey, digest(preimage), new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
				t.Fatal("proof differs from frozen domain/framing contract")
			}
			ch.ExpiresAt = timestamppb.New(time.Now().Add(-time.Second))
			if _, e = proof(key, domain, p.Audience, p.Opaque, p.JTI, artifact, p.Role, ch); e == nil {
				t.Fatal("signed expired challenge")
			}
		})
	}
}
func TestCloudMLDSAGrant(t *testing.T) {
	pub, key, e := mldsa65.GenerateKey(nil)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := pub.MarshalBinary()
	v := loadVectors(t)
	c := v.Artifacts.Presence.Claims
	verifier := &Verifier{Issuer: c.Iss, fetched: time.Now(), keys: map[string]jwk{"ml": {Kty: "AKP", Alg: "ML-DSA-65", Kid: "ml", Pub: b64.EncodeToString(raw)}}}
	header, _ := canonicalJSON(map[string]string{"alg": "ML-DSA-65", "kid": "ml", "typ": leaseType})
	payload := strings.Split(v.Artifacts.Presence.JWS, ".")[1]
	input := b64.EncodeToString(header) + "." + payload
	signature := make([]byte, mldsa65.SignatureSize)
	if e = mldsa65.SignTo(key, []byte(input), nil, false, signature); e != nil {
		t.Fatal(e)
	}
	if _, e = verifier.verify(context.Background(), input+"."+b64.EncodeToString(signature), leaseType, c.Aud, time.Unix(c.Iat+1, 0)); e != nil {
		t.Fatal(e)
	}
	verifier.keys["ml"] = jwk{Kty: "EC", Alg: "ES256", Crv: "P-256", Kid: "ml"}
	if _, e = verifier.verify(context.Background(), input+"."+b64.EncodeToString(signature), leaseType, c.Aud, time.Unix(c.Iat+1, 0)); e == nil {
		t.Fatal("accepted algorithm/key substitution")
	}
}
