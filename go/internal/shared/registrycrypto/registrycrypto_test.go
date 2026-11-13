package registrycrypto

import (
	"bytes"
	"crypto/hpke"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// Vectors: service-protos conformance/registry and conformance/reqsig at the
// commit in testdata/SOURCE.

func load(t *testing.T, name string, v any) {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func unb64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type binding struct {
	OrganizationID  string `json:"organization_id"`
	ScopeKind       byte   `json:"scope_kind"`
	ScopeID         string `json:"scope_id"`
	KeyID           string `json:"key_id"`
	RecipientKind   byte   `json:"recipient_kind"`
	Recipient       string `json:"recipient"`
	RecipientKeyID  string `json:"recipient_key_id"`
	DeploymentID    string `json:"deployment_id"`
	ManifestDigest  string `json:"manifest_digest"`
	ContentKeyID    string `json:"content_key_id"`
	DeploymentKeyID string `json:"deployment_key_id"`
}

func (b binding) wrap() Wrap {
	return Wrap{b.OrganizationID, b.ScopeKind, b.ScopeID, b.KeyID, b.RecipientKind, b.Recipient, b.RecipientKeyID}
}

func (b binding) sealed() SealedKey {
	return SealedKey{b.OrganizationID, b.DeploymentID, b.ManifestDigest, b.ContentKeyID, b.DeploymentKeyID}
}

type registryVectors struct {
	PerBlobKey struct {
		ContentKeyHex string `json:"content_key_hex"`
		ContentKeyID  string `json:"content_key_id"`
		ChunkSize     uint32 `json:"chunk_size"`
		Blobs         []struct {
			BlobKeyHex      string `json:"blob_key_hex"`
			PlaintextDigest string `json:"plaintext_digest"`
		} `json:"blobs"`
	} `json:"per_blob_key"`
	HybridKEM struct {
		IKMHex        string `json:"ikm_hex"`
		PublicKeyHex  string `json:"public_key_hex"`
		KeyID         string `json:"key_id"`
		EncHex        string `json:"enc_hex"`
		CiphertextHex string `json:"ciphertext_hex"`
		InfoHex       string `json:"info_hex"`
		AADHex        string `json:"aad_hex"`
		PlaintextHex  string `json:"plaintext_hex"`
	} `json:"hybrid_kem"`
	ChunkAEAD struct {
		ContentKeyHex string `json:"content_key_hex"`
		ChunkSize     int    `json:"chunk_size"`
		Vectors       []struct {
			Name             string `json:"name"`
			Expect           string `json:"expect"`
			PlaintextHex     string `json:"plaintext_hex"`
			PlaintextDigest  string `json:"plaintext_digest"`
			PlaintextSize    uint64 `json:"plaintext_size"`
			CiphertextHex    string `json:"ciphertext_hex"`
			CiphertextDigest string `json:"ciphertext_digest"`
		} `json:"vectors"`
	} `json:"chunk_aead"`
	KeyWrap struct {
		RecipientIKMHex string `json:"recipient_ikm_hex"`
		Vectors         []struct {
			Name          string  `json:"name"`
			Expect        string  `json:"expect"`
			Binding       binding `json:"binding"`
			InfoHex       string  `json:"info_hex"`
			EncHex        string  `json:"enc_hex"`
			CiphertextHex string  `json:"ciphertext_hex"`
			KeyHex        string  `json:"wrapped_key_hex"`
		} `json:"vectors"`
	} `json:"key_wrap"`
	SealedContentKey struct {
		DeploymentKeyHex string `json:"deployment_key_hex"`
		Vectors          []struct {
			Name          string  `json:"name"`
			Expect        string  `json:"expect"`
			Binding       binding `json:"binding"`
			AADHex        string  `json:"aad_hex"`
			NonceHex      string  `json:"nonce_hex"`
			CiphertextHex string  `json:"ciphertext_hex"`
			ContentKeyHex string  `json:"content_key_hex"`
		} `json:"vectors"`
	} `json:"sealed_content_key"`
	Todo []json.RawMessage `json:"todo"`
}

func TestRegistryVectors(t *testing.T) {
	var v registryVectors
	load(t, "registry-vectors-v1.json", &v)

	contentKey := unhex(t, v.PerBlobKey.ContentKeyHex)
	if KeyID(contentKey) != v.PerBlobKey.ContentKeyID {
		t.Fatal("content_key_id")
	}
	for _, b := range v.PerBlobKey.Blobs {
		key, err := BlobKey(contentKey, b.PlaintextDigest, v.PerBlobKey.ChunkSize)
		if err != nil || hex.EncodeToString(key) != b.BlobKeyHex {
			t.Fatalf("per-blob key for %s", b.PlaintextDigest)
		}
	}

	h := v.HybridKEM
	priv, err := KEM.DeriveKeyPair(unhex(t, h.IKMHex))
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.PublicKey().Bytes()
	if hex.EncodeToString(pub) != h.PublicKeyHex || KeyID(pub) != h.KeyID || len(pub) != 1216 {
		t.Fatal("hybrid KEM public key or key_id")
	}
	// The suite check uses labelled info/aad, not the wrap layout, so it goes
	// through crypto/hpke directly with this package's suite.
	recipient, err := hpke.NewRecipient(unhex(t, h.EncHex), priv, kdf, aead, unhex(t, h.InfoHex))
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := recipient.Open(unhex(t, h.AADHex), unhex(t, h.CiphertextHex)); err != nil || hex.EncodeToString(pt) != h.PlaintextHex {
		t.Fatal("hybrid KEM vector does not open")
	}

	if len(v.Todo) != 0 {
		t.Fatalf("vendored vectors still list todo entries: %s", v.Todo)
	}
	c := v.ChunkAEAD
	if len(c.Vectors) == 0 || len(v.KeyWrap.Vectors) == 0 || len(v.SealedContentKey.Vectors) == 0 {
		t.Fatal("chunk, wrap or seal vectors missing")
	}
	for _, cv := range c.Vectors {
		want := Blob{PlaintextDigest: cv.PlaintextDigest, CiphertextDigest: cv.CiphertextDigest, PlaintextSize: cv.PlaintextSize, CiphertextSize: uint64(len(cv.CiphertextHex) / 2)}
		var out bytes.Buffer
		err := DecryptBlob(&out, bytes.NewReader(unhex(t, cv.CiphertextHex)), unhex(t, c.ContentKeyHex), want, c.ChunkSize)
		if cv.Expect == "valid" {
			if err != nil || hex.EncodeToString(out.Bytes()) != cv.PlaintextHex {
				t.Fatalf("%s: %v", cv.Name, err)
			}
			// Encryption is deterministic: a producer reproduces the bytes.
			var enc bytes.Buffer
			got, err := EncryptBlob(&enc, bytes.NewReader(unhex(t, cv.PlaintextHex)), unhex(t, c.ContentKeyHex), cv.PlaintextDigest, c.ChunkSize)
			if err != nil || got != want || hex.EncodeToString(enc.Bytes()) != cv.CiphertextHex {
				t.Fatalf("%s: encryption does not reproduce the vector", cv.Name)
			}
		} else if !errors.Is(err, ErrOpen) {
			t.Fatalf("%s: want ErrOpen, got %v", cv.Name, err)
		}
	}

	wrapPriv, err := KEM.DeriveKeyPair(unhex(t, v.KeyWrap.RecipientIKMHex))
	if err != nil {
		t.Fatal(err)
	}
	for _, wv := range v.KeyWrap.Vectors {
		w := wv.Binding.wrap()
		info, err := w.info()
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(info) != wv.InfoHex {
			t.Fatalf("%s: info bytes differ", wv.Name)
		}
		// An invalid wrap may be refused before HPKE (recipient key id).
		key, err := w.Open(wrapPriv, unhex(t, wv.EncHex), unhex(t, wv.CiphertextHex))
		if (err == nil) != (wv.Expect == "valid") || err == nil && hex.EncodeToString(key) != wv.KeyHex {
			t.Fatalf("%s: expect %s, got %v", wv.Name, wv.Expect, err)
		}
	}

	for _, sv := range v.SealedContentKey.Vectors {
		s := sv.Binding.sealed()
		aad, err := s.aad()
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(aad) != sv.AADHex {
			t.Fatalf("%s: aad bytes differ", sv.Name)
		}
		// An invalid seal may be refused before AES-GCM (deployment key id).
		key, err := s.Open(unhex(t, v.SealedContentKey.DeploymentKeyHex), unhex(t, sv.NonceHex), unhex(t, sv.CiphertextHex))
		if (err == nil) != (sv.Expect == "valid") || err == nil && hex.EncodeToString(key) != sv.ContentKeyHex {
			t.Fatalf("%s: expect %s, got %v", sv.Name, sv.Expect, err)
		}
	}
}

func encrypt(t *testing.T, contentKey, plaintext []byte, chunkSize int) ([]byte, Blob) {
	t.Helper()
	var ct bytes.Buffer
	b, err := EncryptBlob(&ct, bytes.NewReader(plaintext), contentKey, sha(plaintext), chunkSize)
	if err != nil {
		t.Fatal(err)
	}
	return ct.Bytes(), b
}

func TestBlobRoundTripAndTamper(t *testing.T) {
	contentKey := NewKey()
	for _, size := range []int{0, 1, 15, 16, 17, 32, 40} {
		pt := bytes.Repeat([]byte{byte(size)}, size)
		ct, b := encrypt(t, contentKey, pt, 16)
		chunks := max(1, (size+15)/16)
		if b.CiphertextSize != uint64(size+16*chunks) || b.PlaintextSize != uint64(size) {
			t.Fatalf("size %d: framing %+v", size, b)
		}
		var out bytes.Buffer
		if err := DecryptBlob(&out, bytes.NewReader(ct), contentKey, b, 16); err != nil || !bytes.Equal(out.Bytes(), pt) {
			t.Fatalf("size %d: round trip: %v", size, err)
		}
	}

	pt := bytes.Repeat([]byte("abcdefgh"), 6) // 48 bytes = 3 full chunks of 16
	ct, b := encrypt(t, contentKey, pt, 16)
	other := []byte("another blob, same release, same key")
	otherCT, otherB := encrypt(t, contentKey, other, 16)
	const c = 32 // ciphertext chunk
	cases := map[string]struct {
		ct   []byte
		want Blob
	}{
		"truncated last chunk": {ct[:2*c], b},
		"reordered chunks":     {append(append(append([]byte{}, ct[c:2*c]...), ct[:c]...), ct[2*c:]...), b},
		"trailing chunk":       {append(append([]byte{}, ct...), ct[2*c:]...), b},
		"flipped bit":          {append(append([]byte{}, ct[:5]...), append([]byte{ct[5] ^ 1}, ct[6:]...)...), b},
		"cross-blob splice":    {append(append([]byte{}, otherCT[:c]...), ct[c:]...), b},
		"other blob's key":     {otherCT, Blob{PlaintextDigest: b.PlaintextDigest, CiphertextDigest: otherB.CiphertextDigest, PlaintextSize: otherB.PlaintextSize, CiphertextSize: otherB.CiphertextSize}},
		"empty ciphertext":     {nil, b},
	}
	for name, tc := range cases {
		if err := DecryptBlob(&bytes.Buffer{}, bytes.NewReader(tc.ct), contentKey, tc.want, 16); !errors.Is(err, ErrOpen) {
			t.Errorf("%s: want ErrOpen, got %v", name, err)
		}
	}

	// Authentic chunks, wrong manifest digests: refused at the end.
	bad := b
	bad.CiphertextDigest = otherB.CiphertextDigest
	if err := DecryptBlob(&bytes.Buffer{}, bytes.NewReader(ct), contentKey, bad, 16); err == nil {
		t.Error("ciphertext digest mismatch accepted")
	}
	bad = b
	bad.PlaintextSize++
	if err := DecryptBlob(&bytes.Buffer{}, bytes.NewReader(ct), contentKey, bad, 16); err == nil {
		t.Error("plaintext size mismatch accepted")
	}
	if _, err := EncryptBlob(&bytes.Buffer{}, bytes.NewReader(pt), contentKey, sha(other), 16); err == nil {
		t.Error("encryption under a digest the plaintext does not have accepted")
	}
}

func TestWrapAndSeal(t *testing.T) {
	priv, err := KEM.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.PublicKey().Bytes()
	deploymentKey, contentKey := NewKey(), NewKey()
	w := Wrap{OrganizationID: "2558fd76-afc7-466e-9613-6b715296a526", Scope: ScopeDeployment, ScopeID: "dep-1", KeyID: KeyID(deploymentKey), RecipientKind: 2, Recipient: "spiffe://wendy.sh/tenant/2558fd76-afc7-466e-9613-6b715296a526/operator/u1", RecipientKeyID: KeyID(pub)}
	enc, ct, err := w.Seal(pub, deploymentKey)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := w.Open(priv, enc, ct); err != nil || !bytes.Equal(got, deploymentKey) {
		t.Fatalf("unwrap: %v", err)
	}
	for name, moved := range map[string]Wrap{
		"other tenant":    {"00000000-0000-4000-8000-000000000000", w.Scope, w.ScopeID, w.KeyID, w.RecipientKind, w.Recipient, w.RecipientKeyID},
		"release scope":   {w.OrganizationID, ScopeRelease, w.ScopeID, w.KeyID, w.RecipientKind, w.Recipient, w.RecipientKeyID},
		"other scope id":  {w.OrganizationID, w.Scope, "dep-2", w.KeyID, w.RecipientKind, w.Recipient, w.RecipientKeyID},
		"other kind":      {w.OrganizationID, w.Scope, w.ScopeID, w.KeyID, 1, w.Recipient, w.RecipientKeyID},
		"other recipient": {w.OrganizationID, w.Scope, w.ScopeID, w.KeyID, w.RecipientKind, w.Recipient + "x", w.RecipientKeyID},
		"other key id":    {w.OrganizationID, w.Scope, w.ScopeID, KeyID(contentKey), w.RecipientKind, w.Recipient, w.RecipientKeyID},
	} {
		if _, err := moved.Open(priv, enc, ct); !errors.Is(err, ErrOpen) {
			t.Errorf("%s: want ErrOpen, got %v", name, err)
		}
	}
	if _, _, err := w.Seal(pub, contentKey); err == nil {
		t.Error("wrapping a key other than key_id accepted")
	}
	stranger, _ := KEM.GenerateKey()
	if _, err := w.Open(stranger, enc, ct); err == nil {
		t.Error("another recipient key opened the wrap")
	}

	s := SealedKey{w.OrganizationID, "dep-1", "sha256:" + KeyID([]byte("manifest")), KeyID(contentKey), KeyID(deploymentKey)}
	nonce, sealed, err := s.Seal(deploymentKey, contentKey)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Open(deploymentKey, nonce, sealed); err != nil || !bytes.Equal(got, contentKey) {
		t.Fatalf("open sealed key: %v", err)
	}
	for name, moved := range map[string]SealedKey{
		"other tenant":     {"00000000-0000-4000-8000-000000000000", s.DeploymentID, s.ManifestDigest, s.ContentKeyID, s.DeploymentKeyID},
		"other deployment": {s.OrganizationID, "dep-2", s.ManifestDigest, s.ContentKeyID, s.DeploymentKeyID},
		"other manifest":   {s.OrganizationID, s.DeploymentID, "sha256:00", s.ContentKeyID, s.DeploymentKeyID},
		"other content id": {s.OrganizationID, s.DeploymentID, s.ManifestDigest, KeyID(deploymentKey), s.DeploymentKeyID},
	} {
		if _, err := moved.Open(deploymentKey, nonce, sealed); !errors.Is(err, ErrOpen) {
			t.Errorf("%s: want ErrOpen, got %v", name, err)
		}
	}
	if _, err := s.Open(NewKey(), nonce, sealed); err == nil {
		t.Error("another deployment key opened the sealed key")
	}

	long := strings.Repeat("x", 65536)
	tooLong := w
	tooLong.Recipient = long
	if _, _, err := tooLong.Seal(pub, deploymentKey); err == nil {
		t.Error("wrap with a field over 65535 bytes accepted")
	}
	tooLongSeal := s
	tooLongSeal.ManifestDigest = long
	if _, _, err := tooLongSeal.Seal(deploymentKey, contentKey); err == nil {
		t.Error("sealed key with a field over 65535 bytes accepted")
	}
	tooLong.Recipient = long[:65535]
	if _, _, err := tooLong.Seal(pub, deploymentKey); err != nil {
		t.Errorf("wrap with a 65535-byte field refused: %v", err)
	}
}

func TestRecipientKeyVectors(t *testing.T) {
	var v struct {
		Root    string `json:"root_cert_der_b64"`
		Vectors []struct {
			Name          string `json:"name"`
			Expect        string `json:"expect"`
			Organization  string `json:"organization_id"`
			Principal     string `json:"principal"`
			RecipientKey  string `json:"recipient_key_b64"`
			ExpectedKeyID string `json:"expected_key_id"`
		} `json:"recipient_key_vectors"`
	}
	load(t, "device-signed-request-vectors-v1.json", &v)
	root, err := x509.ParseCertificate(unb64(t, v.Root))
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Vectors) != 6 {
		t.Fatalf("want 6 recipient key vectors, got %d", len(v.Vectors))
	}
	for _, rv := range v.Vectors {
		var key cloudpbv2.RecipientKey
		if err := proto.Unmarshal(unb64(t, rv.RecipientKey), &key); err != nil || key.GetRegistration() == nil {
			t.Fatalf("%s: registration", rv.Name)
		}
		publicKey, err := VerifyRegistration(key.GetRegistration(), key.GetChainDer(), root, rv.Principal, rv.Organization)
		if (err == nil) != (rv.Expect == "valid") {
			t.Errorf("%s: expect %s, got %v", rv.Name, rv.Expect, err)
		}
		if err == nil && KeyID(publicKey) != rv.ExpectedKeyID {
			t.Errorf("%s: key_id %s", rv.Name, KeyID(publicKey))
		}
	}
}

func TestDeviceRegistrationVector(t *testing.T) {
	var v struct {
		Root     string `json:"root_cert_der_b64"`
		DeviceCA string `json:"device_ca_cert_der_b64"`
		Vectors  []struct {
			Name          string `json:"name"`
			Tenant        string `json:"expected_tenant"`
			SAN           string `json:"spiffe_san"`
			SignedRequest string `json:"signed_request_b64"`
		} `json:"vectors"`
	}
	load(t, "device-signed-request-vectors-v1.json", &v)
	root, _ := x509.ParseCertificate(unb64(t, v.Root))
	for _, dv := range v.Vectors {
		if dv.Name != "mldsa65-register-device-encryption-key-valid" {
			continue
		}
		var reg cloudpbv2.SignedRequest
		if err := proto.Unmarshal(unb64(t, dv.SignedRequest), &reg); err != nil {
			t.Fatal(err)
		}
		chain := [][]byte{unb64(t, v.DeviceCA)}
		if _, err := VerifyRegistration(&reg, chain, root, dv.SAN, dv.Tenant); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyRegistration(&reg, chain, root, dv.SAN, "00000000-0000-4000-8000-000000000000"); err == nil {
			t.Fatal("registration verified for another organization")
		}
		var claims struct {
			Iat int64 `json:"iat"`
		}
		parts := strings.Split(string(reg.GetSignature()), ".")
		raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
		if err := json.Unmarshal(raw, &claims); err != nil {
			t.Fatal(err)
		}
		if _, err := verifyRegistration(&reg, chain, root, dv.SAN, dv.Tenant, time.Unix(claims.Iat-3600, 0)); err == nil {
			t.Fatal("registration signed an hour in the future accepted")
		}
		// The signed payload, not the caller, binds organization and key.
		publicKey, err := registeredKey(reg.GetPayload(), dv.Tenant)
		if err != nil {
			t.Fatal(err)
		}
		request := func(org string, suite cloudpbv2.KemSuite) []byte {
			b, err := proto.Marshal(&cloudpbv2.RegisterEncryptionKeyRequest{OrganizationId: org, Suite: suite, PublicKey: publicKey})
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
		if _, err := registeredKey(request(dv.Tenant, cloudpbv2.KemSuite_KEM_SUITE_MLKEM768_X25519), dv.Tenant); err != nil {
			t.Fatalf("rebuilt request refused: %v", err)
		}
		for name, payload := range map[string][]byte{
			"other organization": request("00000000-0000-4000-8000-000000000000", cloudpbv2.KemSuite_KEM_SUITE_MLKEM768_X25519),
			"unknown suite":      request(dv.Tenant, 9),
			// Only raw wire bytes can repeat a field; proto.Unmarshal keeps the last.
			"duplicate key": append(reg.GetPayload(), protowire.AppendBytes(protowire.AppendTag(nil, 3, protowire.BytesType), publicKey)...),
			// Field 3 as a varint is an unknown field to proto.Unmarshal, not a second key.
			"wrong-type key": protowire.AppendVarint(protowire.AppendTag(append([]byte(nil), reg.GetPayload()...), 3, protowire.VarintType), 1),
		} {
			if _, err := registeredKey(payload, dv.Tenant); err == nil {
				t.Errorf("%s: payload accepted", name)
			}
		}

		return
	}
	t.Fatal("device registration vector missing")
}

// A known device opens its own wrap and wraps the same key onward; the new
// recipient opens it, and an onward wrap naming another key is refused.
func TestRewrap(t *testing.T) {
	known, err := KEM.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	newcomer, err := KEM.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	key := NewKey()
	held := Wrap{OrganizationID: "org", Scope: ScopeRelease, ScopeID: "release", KeyID: KeyID(key), RecipientKind: 1, Recipient: "operator-a", RecipientKeyID: KeyID(known.PublicKey().Bytes())}
	enc, ct, err := held.Seal(known.PublicKey().Bytes(), key)
	if err != nil {
		t.Fatal(err)
	}
	onward := held
	onward.Recipient, onward.RecipientKeyID = "operator-a-laptop", KeyID(newcomer.PublicKey().Bytes())
	enc2, ct2, err := Rewrap(known, held, enc, ct, onward, newcomer.PublicKey().Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := onward.Open(newcomer, enc2, ct2); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("onward wrap: %v", err)
	}
	other := onward
	other.KeyID = KeyID(NewKey())
	if _, _, err := Rewrap(known, held, enc, ct, other, newcomer.PublicKey().Bytes()); err == nil {
		t.Fatal("rewrapped under another key id")
	}
}

// The registration the CLI and wendy-web sign is one the verifier accepts.
func TestRegistrationRoundTrip(t *testing.T) {
	priv, err := KEM.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := proto.Marshal(Registration("org", priv.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := registeredKey(payload, "org"); err != nil || !bytes.Equal(got, priv.PublicKey().Bytes()) {
		t.Fatalf("registration refused: %v", err)
	}
}

// The dev custodian pin parses as an MLKEM768X25519 key whose key id is the
// one cloud-info read from KMS; prod is refused, never defaulted.
func TestCustodianKey(t *testing.T) {
	pub, keyID, err := CustodianKey(true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := KEM.NewPublicKey(pub); err != nil {
		t.Fatalf("dev custodian pin is not an MLKEM768X25519 key: %v", err)
	}
	if sum := sha256.Sum256(pub); keyID != hex.EncodeToString(sum[:]) || keyID != "b98c5d3b9da8d92be5a1c6f2277f05490fa1ea7a5be269e2b30808e22d3dae10" {
		t.Fatalf("dev custodian key id %s", keyID)
	}
	if _, _, err := CustodianKey(false); !errors.Is(err, ErrNoCustodian) {
		t.Fatalf("prod custodian key: %v, want ErrNoCustodian", err)
	}
}

// chunk_size comes from the Cloud-served manifest, outside manifest_digest:
// an oversized one is refused before it sizes a buffer.
func TestChunkSizeCap(t *testing.T) {
	contentKey := NewKey()
	ct, b := encrypt(t, contentKey, []byte("x"), 16)
	for _, size := range []int{0, -1, MaxChunkSize + 1, 1<<31 - 1 - 16} {
		if err := DecryptBlob(&bytes.Buffer{}, bytes.NewReader(ct), contentKey, b, size); !errors.Is(err, ErrChunkSize) {
			t.Errorf("decrypt chunk size %d: want ErrChunkSize, got %v", size, err)
		}
		if _, err := EncryptBlob(&bytes.Buffer{}, bytes.NewReader(nil), contentKey, sha(nil), size); !errors.Is(err, ErrChunkSize) {
			t.Errorf("encrypt chunk size %d: want ErrChunkSize, got %v", size, err)
		}
	}
}

func TestRegistrationClaimsStrict(t *testing.T) {
	var v struct {
		Vectors []struct {
			Name   string `json:"name"`
			Claims string `json:"claims_jcs"`
		} `json:"vectors"`
	}
	load(t, "device-signed-request-vectors-v1.json", &v)
	var valid string
	for _, dv := range v.Vectors {
		if dv.Name == "mldsa65-register-device-encryption-key-valid" {
			valid = dv.Claims
		}
	}
	if _, err := parseClaims([]byte(valid)); err != nil {
		t.Fatalf("vector claims refused: %v", err)
	}
	if !strings.HasPrefix(valid, `{"aud":"https://cloud.wendy.sh/broker",`) {
		t.Fatalf("vector claims shape changed: %s", valid)
	}
	rest := strings.TrimPrefix(valid, `{"aud":"https://cloud.wendy.sh/broker",`)
	for name, raw := range map[string]string{
		"duplicate key":  `{"aud":"https://cloud.wendy.sh/broker","aud":"https://cloud.wendy.sh/broker",` + rest,
		"unknown field":  `{"aud":"https://cloud.wendy.sh/broker","aux":1,` + rest,
		"key case":       `{"Aud":"https://cloud.wendy.sh/broker",` + rest,
		"missing aud":    `{` + rest,
		"other audience": `{"aud":"https://cloud.wendy.sh/other",` + rest,
		"whitespace":     `{"aud": "https://cloud.wendy.sh/broker",` + rest,
		"trailing value": valid + `{}`,
	} {
		if _, err := parseClaims([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
