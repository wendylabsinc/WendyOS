package cloudrequest

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"
)

// TestEnrollmentRequestMatchesPKICoreContract pins the shape pki-core's fabric
// relay verifies. Every assertion here is a rejection at the far end if it
// drifts, and the credential is single-use, so a malformed request costs a
// real mint attempt rather than a retry.
func TestEnrollmentRequestMatchesPKICoreContract(t *testing.T) {
	auth, key, leafDER := testAuth(t)
	signer, err := NewEnrollmentSigner(auth)
	if err != nil {
		t.Fatalf("NewEnrollmentSigner: %v", err)
	}
	fixed := time.Unix(1_784_659_200, 0)
	signer.now = func() time.Time { return fixed }

	jws, err := signer.SignEnrollmentRequest("fleet-a/box-01", DeviceClassB)
	if err != nil {
		t.Fatalf("SignEnrollmentRequest: %v", err)
	}

	compact := string(jws)
	segments := strings.Split(compact, ".")
	if len(segments) != 3 {
		t.Fatalf("compact JWS has %d segments, want 3", len(segments))
	}
	for i, segment := range segments {
		if segment == "" {
			t.Fatalf("segment %d is empty; pki-core rejects a detached payload", i)
		}
		// pki-core is stricter than base64.RawURLEncoding: only the base64url
		// alphabet, no padding, no whitespace.
		if strings.ContainsAny(segment, "=+/ \r\n") {
			t.Errorf("segment %d = %q is not strict unpadded base64url", i, segment)
		}
	}

	header := decodeSegment(t, segments[0])
	var hdr struct {
		Alg  string   `json:"alg"`
		X5c  [][]byte `json:"x5c"`
		Crit []string `json:"crit"`
	}
	if err := json.Unmarshal(header, &hdr); err != nil {
		t.Fatalf("header is not JSON: %v", err)
	}
	if hdr.Alg != "ES256" {
		t.Errorf("alg = %q, want ES256 (the ruling until WDY-2967 lands)", hdr.Alg)
	}
	if len(hdr.Crit) != 0 {
		t.Errorf("crit = %v; any non-empty crit is rejected", hdr.Crit)
	}
	// x5c is base64-STANDARD per RFC 7515 4.1.6, leaf first.
	if len(hdr.X5c) == 0 || string(hdr.X5c[0]) != string(leafDER) {
		t.Errorf("x5c[0] is not the operator leaf DER")
	}

	payload := decodeSegment(t, segments[1])
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if claims["tenant"] != testTenant {
		t.Errorf("tenant = %v, want %s (must string-equal the relay envelope tenant)", claims["tenant"], testTenant)
	}
	if claims["device_id"] != "fleet-a/box-01" {
		t.Errorf("device_id = %v", claims["device_id"])
	}
	if claims["device_class"] != "B" {
		t.Errorf("device_class = %v, want B", claims["device_class"])
	}
	if jti, _ := claims["jti"].(string); jti == "" {
		t.Error("jti is empty; pki-core burns it for replay protection")
	}
	// Rejected outright by pki-core rather than ignored, so they must be absent.
	for _, forbidden := range []string{"csr_key_binding", "attestation_ref"} {
		if _, present := claims[forbidden]; present {
			t.Errorf("payload carries %q, which pki-core refuses", forbidden)
		}
	}
	iat, exp := claims["iat"].(float64), claims["exp"].(float64)
	if int64(iat) != fixed.Unix() {
		t.Errorf("iat = %v, want %d", iat, fixed.Unix())
	}
	if exp <= iat {
		t.Errorf("exp %v must be after iat %v", exp, iat)
	}
	if window := int64(exp - iat); window > int64(24*time.Hour/time.Second) {
		t.Errorf("exp-iat = %ds, over pki-core's 24h cap", window)
	}

	// Raw fixed-width r||s, not DER.
	sig := decodeSegment(t, segments[2])
	if len(sig) != 64 {
		t.Fatalf("signature is %d bytes, want 64 (raw r||s)", len(sig))
	}
	digest := sha256.Sum256([]byte(segments[0] + "." + segments[1]))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&key.PublicKey, digest[:], r, s) {
		t.Fatal("enrollment JWS signature does not verify")
	}
}

func TestEnrollmentRequestJTIIsFreshPerRequest(t *testing.T) {
	auth, _, _ := testAuth(t)
	signer, err := NewEnrollmentSigner(auth)
	if err != nil {
		t.Fatal(err)
	}
	// pki-core burns a jti before it mints, so a reused one is unusable even
	// after a failure.
	seen := map[string]bool{}
	for range 3 {
		jws, err := signer.SignEnrollmentRequest("box-01", DeviceClassB)
		if err != nil {
			t.Fatal(err)
		}
		var claims struct {
			JTI string `json:"jti"`
		}
		if err := json.Unmarshal(decodeSegment(t, strings.Split(string(jws), ".")[1]), &claims); err != nil {
			t.Fatal(err)
		}
		if seen[claims.JTI] {
			t.Fatalf("jti %q reused", claims.JTI)
		}
		seen[claims.JTI] = true
	}
}

func TestEnrollmentSignerRefusesSessionsWithoutAnOperatorCert(t *testing.T) {
	auth, _, _ := testAuth(t)
	// A legacy/token-only session: the interceptor stays silent for these, but
	// enrolling needs an operator signature and must say so.
	auth.OAuthIssuer = ""
	if _, err := NewEnrollmentSigner(auth); err == nil {
		t.Error("NewEnrollmentSigner accepted a session with no operator certificate")
	}
}

func TestDeviceClassLetters(t *testing.T) {
	for class, want := range map[DeviceClass]string{DeviceClassA: "A", DeviceClassB: "B", DeviceClassC: "C"} {
		got, err := class.letter()
		if err != nil || got != want {
			t.Errorf("DeviceClass(%d).letter() = %q, %v; want %q", int(class), got, err, want)
		}
	}
	if _, err := DeviceClass(0).letter(); err == nil {
		t.Error("unset device class must not silently become a real one")
	}
}

func decodeSegment(t *testing.T, segment string) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		t.Fatalf("segment %q is not base64url: %v", segment, err)
	}
	return raw
}
