package roughtime

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha512"
	"encoding/binary"
	"reflect"
	"testing"
)

func FuzzMessageDecoder(f *testing.F) {
	f.Add(EncodeMessage(map[uint32][]byte{TagNONC: make([]byte, 32), TagVER: {8, 0, 0, 128}}))
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64<<10 {
			return
		}
		message, err := DecodeMessage(data)
		if err != nil {
			return
		}
		// Successful decodes must survive canonical encoding without losing
		// tag values, including zero-length values and empty messages.
		again, err := DecodeMessage(EncodeMessage(message))
		if err != nil || !reflect.DeepEqual(message, again) {
			t.Fatalf("round trip: %v", err)
		}
	})
}

func FuzzResponseVerification(f *testing.F) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	online := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, ed25519.SeedSize))
	srv := Server{Name: "fuzz", PublicKey: key.Public().(ed25519.PublicKey)}
	nonce := bytes.Repeat([]byte{3}, nonceSize)
	leaf := sha512.Sum512(append([]byte{0}, nonce...))
	srep := EncodeMessage(map[uint32][]byte{
		TagMIDP: binary.LittleEndian.AppendUint64(nil, 1790000000),
		TagRADI: binary.LittleEndian.AppendUint32(nil, 1), TagROOT: leaf[:32],
	})
	dele := EncodeMessage(map[uint32][]byte{
		TagMINT: make([]byte, 8), TagMAXT: bytes.Repeat([]byte{0xff}, 8),
		TagPUBK: online.Public().(ed25519.PublicKey),
	})
	cert := EncodeMessage(map[uint32][]byte{TagDELE: dele, TagSIG: ed25519.Sign(key, append([]byte(CertContext), dele...))})
	sig := ed25519.Sign(online, append([]byte(SigContext), srep...))
	outer := map[uint32][]byte{
		TagCERT: cert, TagNONC: nonce, TagSIG: sig, TagSREP: srep,
		TagINDX: make([]byte, 4), TagPATH: {}, TagVER: binary.LittleEndian.AppendUint32(nil, VersionDraft08),
	}
	frame := func(message map[uint32][]byte) []byte {
		body := EncodeMessage(message)
		return append(binary.LittleEndian.AppendUint32([]byte("ROUGHTIM"), uint32(len(body))), body...)
	}
	valid := frame(outer)
	if _, err := VerifyResponse(valid, nonce, srv); err != nil {
		f.Fatal(err)
	}
	for mode := byte(0); mode < 6; mode++ {
		f.Add([]byte{}, uint16(0), mode)
	}
	f.Add(valid, uint16(0), byte(5))
	f.Fuzz(func(t *testing.T, data []byte, offset uint16, mode byte) {
		if len(data) > 64<<10 {
			return
		}
		raw, requestNonce := bytes.Clone(valid), bytes.Clone(nonce)
		switch mode % 6 {
		case 0:
			requestNonce[int(offset)%len(requestNonce)] ^= 1
		case 1:
			raw = raw[:int(offset)%len(raw)] // Every strict prefix is incomplete.
		case 2:
			// Force an offset beyond the TLV body without breaking framing.
			binary.LittleEndian.PutUint32(raw[16:20], ^uint32(0))
		case 3, 4:
			copyOuter := make(map[uint32][]byte, len(outer))
			for k, v := range outer {
				copyOuter[k] = v
			}
			tag := TagSIG
			if mode%6 == 4 {
				tag = TagSREP
			}
			bad := bytes.Clone(copyOuter[tag])
			bad[int(offset)%len(bad)] ^= 1
			copyOuter[tag] = bad
			raw = frame(copyOuter)
		case 5:
			// Arbitrary responses may be valid; verify their nonce binding too.
			if result, err := VerifyResponse(data, requestNonce, srv); err == nil {
				if !bytes.Equal(result.Nonce, nonce) {
					t.Fatal("lost nonce binding")
				}
				requestNonce[int(offset)%len(requestNonce)] ^= 1
				if _, err := VerifyResponse(data, requestNonce, srv); err == nil {
					t.Fatal("accepted another nonce")
				}
			}
			return
		}
		if _, err := VerifyResponse(raw, requestNonce, srv); err == nil {
			t.Fatalf("accepted mutation %d", mode%6)
		}
	})
}
