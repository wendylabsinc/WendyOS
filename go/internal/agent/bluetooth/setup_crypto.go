package bluetooth

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

// Each connection has fresh keys and accepts exactly one request per counter.
type setupCipher struct {
	read, write cipher.AEAD
	counter     uint64
}

func newSetupCipher(client []byte) (*setupCipher, []byte, error) {
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	return setupCipherWithKey(key, client)
}

func setupCipherWithKey(key *ecdh.PrivateKey, client []byte) (*setupCipher, []byte, error) {
	peer, err := ecdh.X25519().NewPublicKey(client)
	if err != nil {
		return nil, nil, err
	}
	shared, err := key.ECDH(peer)
	if err != nil {
		return nil, nil, err
	}
	public := key.PublicKey().Bytes()
	material, err := hkdf.Key(sha256.New, shared, append(append([]byte{}, client...), public...), "wendy-ble-setup-v1", 64)
	if err != nil {
		return nil, nil, err
	}
	defer clear(shared)
	defer clear(material)
	readBlock, _ := aes.NewCipher(material[:32])
	writeBlock, _ := aes.NewCipher(material[32:])
	read, _ := cipher.NewGCM(readBlock)
	write, _ := cipher.NewGCM(writeBlock)
	return &setupCipher{read: read, write: write}, public, nil
}
func (c *setupCipher) open(data []byte) ([]byte, error) {
	if len(data) < 28 || c.counter == ^uint64(0) {
		return nil, fmt.Errorf("invalid setup frame")
	}
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[4:], c.counter+1)
	if !bytes.Equal(data[:12], nonce) {
		return nil, fmt.Errorf("invalid setup counter")
	}
	plain, err := c.read.Open(nil, nonce, data[12:], nil)
	if err != nil {
		return nil, err
	}
	c.counter++
	return plain, nil
}
func (c *setupCipher) seal(data []byte) []byte {
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[4:], c.counter)
	return c.write.Seal(nonce, nonce, data, nil)
}
