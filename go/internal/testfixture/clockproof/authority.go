// Package clockproof supplies isolated signed authority fixtures for clock tests.
package clockproof

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/roughtime"
)

type Authority struct {
	Server roughtime.Server
	key    ed25519.PrivateKey
	at     time.Time
}

func New(t testing.TB, name string, at time.Time) Authority {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return Authority{Server: roughtime.Server{Name: name, Address: name + ":2002", PublicKey: pub}, key: key, at: at}
}

func (a Authority) Reply(nonce []byte) []byte {
	return a.ReplyAt(nonce, a.at)
}

// ReplyAt lets multi-round integration fixtures model a live UTC authority.
func (a Authority) ReplyAt(nonce []byte, at time.Time) []byte {
	mid := make([]byte, 8)
	binary.LittleEndian.PutUint64(mid, uint64(at.Unix()))
	radius := make([]byte, 4)
	binary.LittleEndian.PutUint32(radius, 1)
	leaf := sha512.Sum512(append([]byte{0}, nonce...))
	srep := roughtime.EncodeMessage(map[uint32][]byte{roughtime.TagMIDP: mid, roughtime.TagRADI: radius, roughtime.TagROOT: leaf[:32]})
	version := make([]byte, 4)
	binary.LittleEndian.PutUint32(version, roughtime.VersionDraft08)
	body := roughtime.EncodeMessage(map[uint32][]byte{
		roughtime.TagSIG:  ed25519.Sign(a.key, append([]byte(roughtime.SigContext), srep...)),
		roughtime.TagSREP: srep, roughtime.TagINDX: make([]byte, 4), roughtime.TagPATH: {}, roughtime.TagNONC: nonce, roughtime.TagVER: version,
	})
	framed := append([]byte("ROUGHTIM"), make([]byte, 4)...)
	binary.LittleEndian.PutUint32(framed[8:12], uint32(len(body)))
	return append(framed, body...)
}
