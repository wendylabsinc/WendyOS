package roughtime

import (
	"crypto/ed25519"
	"crypto/sha512"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// WENDY_RT_FIXTURES exports deterministic, test-key-only interoperability vectors
// for wendy-lite/components/wendy_pki/tests/roughtime_test.c.
func TestFirmwareFixtures(t *testing.T) {
	root := ed25519.NewKeyFromSeed(make([]byte, 32))
	seed := make([]byte, 32)
	seed[0] = 1
	delegated := ed25519.NewKeyFromSeed(seed)
	nonce := make([]byte, 32)
	for i := range nonce {
		nonce[i] = byte(i + 1)
	}
	word := func(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }
	long := func(v uint64) []byte { return binary.LittleEndian.AppendUint64(nil, v) }
	hash := sha512.Sum512(append([]byte{0}, nonce...))
	build := func(mid uint64, radius uint32) map[uint32][]byte {
		dele := EncodeMessage(map[uint32][]byte{TagPUBK: delegated.Public().(ed25519.PublicKey), TagMINT: long(1700000000), TagMAXT: long(1900000000)})
		cert := EncodeMessage(map[uint32][]byte{TagDELE: dele, TagSIG: ed25519.Sign(root, append([]byte(CertContext), dele...))})
		rep := EncodeMessage(map[uint32][]byte{TagMIDP: long(mid), TagRADI: word(radius), TagROOT: hash[:32]})
		return map[uint32][]byte{TagVER: word(VersionDraft11), TagNONC: append([]byte(nil), nonce...), TagCERT: cert, TagSREP: rep, TagSIG: ed25519.Sign(delegated, append([]byte(SigContext), rep...)), TagINDX: word(0), TagPATH: {}}
	}
	frame := func(m map[uint32][]byte) []byte {
		body := EncodeMessage(m)
		b := append([]byte(ietfFrameMagic), word(uint32(len(body)))...)
		return append(b, body...)
	}
	pub := root.Public().(ed25519.PublicKey)
	fixtures := map[string][]byte{"key.bin": pub, "nonce.bin": nonce, "request.bin": encodeFramedRequest(nonce, pub)}
	fixtures["valid.bin"] = frame(build(1800000000, 1))
	if _, err := VerifyResponse(fixtures["valid.bin"], nonce, Server{Name: "fixture", PublicKey: pub}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bad-signature", "bad-delegation", "bad-path", "bad-index", "bad-radius", "bad-midpoint", "bad-offset", "bad-version", "bad-nonce"} {
		m := build(1800000000, 1)
		switch name {
		case "bad-signature":
			m[TagSIG][0] ^= 1
		case "bad-delegation":
			m[TagCERT][len(m[TagCERT])-1] ^= 1
		case "bad-path":
			m[TagPATH] = make([]byte, 4)
		case "bad-index":
			m[TagINDX] = word(1)
		case "bad-radius":
			m = build(1800000000, 4295) // catches uint32 seconds-to-microseconds overflow
		case "bad-midpoint":
			m = build(2000000000, 1)
		case "bad-version":
			m[TagVER] = word(7)
		case "bad-nonce":
			m[TagNONC][0] ^= 1
		}
		b := frame(m)
		if name == "bad-offset" {
			binary.LittleEndian.PutUint32(b[16:20], 0xffffffff)
		}
		fixtures[name+".bin"] = b
	}
	dir := os.Getenv("WENDY_RT_FIXTURES")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	for name, b := range fixtures {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0644); err != nil {
			t.Fatal(err)
		}
	}
}
