package gpgverify_test

import (
	"bytes"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"

	"github.com/wendylabsinc/wendy/internal/agent/gpgverify"
)

// generateTestKey creates a fresh in-memory ed25519 keypair for testing.
func generateTestKey(t *testing.T) (pubKeyArmor []byte, signFn func(data []byte) []byte) {
	t.Helper()

	entity, err := openpgp.NewEntity("Test Signer", "", "test@example.com", nil)
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}

	var pubBuf bytes.Buffer
	w, err := armor.Encode(&pubBuf, "PGP PUBLIC KEY BLOCK", nil)
	if err != nil {
		t.Fatalf("armor.Encode public: %v", err)
	}
	if err := entity.Serialize(w); err != nil {
		t.Fatalf("Serialize public key: %v", err)
	}
	w.Close()

	signFn = func(data []byte) []byte {
		var sigBuf bytes.Buffer
		w, err := armor.Encode(&sigBuf, "PGP SIGNATURE", nil)
		if err != nil {
			t.Fatalf("armor.Encode sig: %v", err)
		}
		if err := openpgp.DetachSign(w, entity, bytes.NewReader(data), nil); err != nil {
			t.Fatalf("DetachSign: %v", err)
		}
		w.Close()
		return sigBuf.Bytes()
	}

	return pubBuf.Bytes(), signFn
}

func TestVerifyBinary_ValidSignature(t *testing.T) {
	pubKey, sign := generateTestKey(t)
	data := []byte("fake binary data for testing")
	sig := sign(data)

	if err := gpgverify.VerifyBinary(data, sig, pubKey); err != nil {
		t.Fatalf("expected valid signature to pass, got: %v", err)
	}
}

func TestVerifyBinary_InvalidSignature(t *testing.T) {
	pubKey, sign := generateTestKey(t)
	data := []byte("fake binary data for testing")
	sig := sign(data)

	tampered := append(data, 0xFF)
	err := gpgverify.VerifyBinary(tampered, sig, pubKey)
	if err == nil {
		t.Fatal("expected tampered data to fail verification, got nil error")
	}
}

func TestVerifyBinary_WrongKey(t *testing.T) {
	_, sign := generateTestKey(t)
	otherPubKey, _ := generateTestKey(t)
	data := []byte("fake binary data for testing")
	sig := sign(data)

	err := gpgverify.VerifyBinary(data, sig, otherPubKey)
	if err == nil {
		t.Fatal("expected wrong key to fail verification, got nil error")
	}
}

func TestVerifyBinary_EmptySignature(t *testing.T) {
	pubKey, _ := generateTestKey(t)
	data := []byte("fake binary data for testing")

	err := gpgverify.VerifyBinary(data, nil, pubKey)
	if err == nil {
		t.Fatal("expected nil signature to return error")
	}
}

func TestVerifyBinary_EmptyData(t *testing.T) {
	pubKey, sign := generateTestKey(t)
	sig := sign([]byte{})

	if err := gpgverify.VerifyBinary([]byte{}, sig, pubKey); err != nil {
		t.Fatalf("expected valid empty-data signature to pass, got: %v", err)
	}
}
