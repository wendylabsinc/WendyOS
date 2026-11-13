package cloudrequest

import (
	"bytes"
	"crypto/mldsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"

	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/protobuf/proto"
)

// testdata/signed-request-vectors-v1.json is service-protos
// conformance/reqsig/signed-request-vectors-v1.json at 09097e83 (the WDY-3458
// pin in scripts/generate-proto.sh); re-copy it when re-pinning.
//
// A producer given the vector's inputs must reproduce payload_b64 and
// claims_jcs byte for byte; the signature itself is hedged, so the reference
// JWS is checked by verifying it, not by reproducing it.
func TestSignedRequestConformanceVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/signed-request-vectors-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Vectors []struct {
			Name             string          `json:"name"`
			Expect           string          `json:"expect"`
			SignedRequestB64 string          `json:"signed_request_b64"`
			PayloadB64       string          `json:"payload_b64"`
			Claims           json.RawMessage `json:"claims"`
			ClaimsJCS        string          `json:"claims_jcs"`
			JWSCompact       string          `json:"jws_compact"`
			LeafCertDERB64   string          `json:"leaf_cert_der_b64"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	valid := 0
	for _, v := range file.Vectors {
		if v.Expect != "valid" {
			continue
		}
		valid++
		t.Run(v.Name, func(t *testing.T) {
			wire, _ := base64.StdEncoding.DecodeString(v.SignedRequestB64)
			var signed cloudpbv2.SignedRequest
			if err := proto.Unmarshal(wire, &signed); err != nil {
				t.Fatal(err)
			}
			wantPayload, _ := base64.StdEncoding.DecodeString(v.PayloadB64)
			var req cloudpbv2.CreateAssetRequest
			if err := proto.Unmarshal(wantPayload, &req); err != nil {
				t.Fatal(err)
			}
			if got, err := proto.Marshal(&req); err != nil || !bytes.Equal(got, wantPayload) || !bytes.Equal(signed.GetPayload(), wantPayload) {
				t.Fatal("payload bytes do not reproduce the vector")
			}
			if signed.GetPayloadType() != string(req.ProtoReflect().Descriptor().FullName()) {
				t.Fatalf("payload_type = %q", signed.GetPayloadType())
			}

			dec := json.NewDecoder(bytes.NewReader(v.Claims))
			dec.UseNumber()
			var claims map[string]any
			if err := dec.Decode(&claims); err != nil {
				t.Fatal(err)
			}
			jcs, err := canonicalJSON(claims)
			if err != nil || string(jcs) != v.ClaimsJCS {
				t.Fatalf("canonicalJSON = %s, want %s", jcs, v.ClaimsJCS)
			}
			sum := sha256.Sum256(wantPayload)
			if claims["body_sha256"] != base64.RawURLEncoding.EncodeToString(sum[:]) {
				t.Fatal("body_sha256 does not cover the payload")
			}

			if string(signed.GetSignature()) != v.JWSCompact {
				t.Fatal("envelope signature is not the vector's JWS")
			}
			parts := strings.Split(v.JWSCompact, ".")
			if len(parts) != 3 || parts[1] != base64.RawURLEncoding.EncodeToString(jcs) {
				t.Fatal("JWS payload is not the claims' JCS")
			}
			der, _ := base64.StdEncoding.DecodeString(v.LeafCertDERB64)
			leaf, err := x509.ParseCertificate(der)
			if err != nil {
				t.Fatal(err)
			}
			pub, ok := leaf.PublicKey.(*mldsa.PublicKey)
			if !ok || pub.Parameters() != mldsa.MLDSA65() {
				t.Fatalf("vector leaf key %T, want ML-DSA-65", leaf.PublicKey)
			}
			sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
			if err := mldsa.Verify(pub, []byte(parts[0]+"."+parts[1]), sig, nil); err != nil {
				t.Fatalf("vector JWS does not verify the way signPayload signs: %v", err)
			}
		})
	}
	if valid == 0 {
		t.Fatal("no valid vectors")
	}
}
