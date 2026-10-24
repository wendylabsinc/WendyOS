// Package registrycrypto is the byte-level crypto of the encrypted app
// registry (wendycloud/v2/registry.proto). The CLI and the console (through
// wendy-web) encrypt, wrap and seal with it; the agent unwraps, opens and
// decrypts with it. Layouts and vectors: service-protos conformance/registry
// (ruling: orchestrator 2026-10-22 20:26 under the 2026-10-20 primitives
// decisions).
//
// Keys: a release has one random 32-byte content key; each blob is encrypted
// under its own key derived from the content key and the blob's plaintext
// digest. A deployment has one random 32-byte deployment key that seals the
// content key. Content and deployment keys reach their recipients as HPKE
// wraps (ML-KEM-768 + X25519).
package registrycrypto

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hpke"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"strings"
)

const (
	// KeySize is the size of content, deployment and per-blob keys.
	KeySize = 32
	// DefaultChunkSize is ArtifactManifest.chunk_size when the manifest sets none.
	DefaultChunkSize = 65536

	tagSize   = 16
	nonceSize = 12
	maxChunk  = 1 << 32 // highest chunk index a blob may use

	wrapLabel = "wendy-registry-wrap-v1"
	sealLabel = "wendy-registry-seal-v1"
)

// KEM is the one wrap suite (KemSuite KEM_SUITE_MLKEM768_X25519): HPKE base
// mode with ML-KEM-768 + X25519, HKDF-SHA256 and AES-256-GCM. A recipient's
// public key on the wire is KEM's SerializePublicKey output; its private key
// at rest is PrivateKey.Bytes().
var KEM = hpke.MLKEM768X25519()

var (
	kdf  = hpke.HKDFSHA256()
	aead = hpke.AES256GCM()
)

// ErrOpen is every authentication failure: a tampered, truncated, reordered
// or misdirected ciphertext, wrap or sealed key.
var ErrOpen = errors.New("registry ciphertext does not authenticate")

// KeyID is the lower-case hex SHA-256 of a raw key: content_key_id,
// deployment_key_id, key_id and recipient_key_id.
func KeyID(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])
}

// NewKey returns a fresh random content or deployment key.
func NewKey() []byte {
	key := make([]byte, KeySize)
	rand.Read(key)
	return key
}

// BlobKey derives a blob's own key from the release content key:
// HKDF-SHA256(IKM = content key, no salt, info = plaintext_digest ‖ u32 BE
// chunk_size, L = 32). The chunk size is in the key so that one blob chunked
// two ways never reuses a (key, nonce) pair.
func BlobKey(contentKey []byte, plaintextDigest string, chunkSize uint32) ([]byte, error) {
	if len(contentKey) != KeySize {
		return nil, fmt.Errorf("content key must be %d bytes", KeySize)
	}
	return hkdf.Key(sha256.New, contentKey, nil, string(binary.BigEndian.AppendUint32([]byte(plaintextDigest), chunkSize)), KeySize)
}

// Blob is the digest and size pair an EncryptedBlob carries.
type Blob struct {
	PlaintextDigest  string // "sha256:<hex>"
	CiphertextDigest string // "sha256:<hex>", the registry address
	PlaintextSize    uint64
	CiphertextSize   uint64
}

// chunkNonce is the 11-byte big-endian chunk index followed by the last-chunk
// flag (0x01 on the last chunk, else 0x00).
func chunkNonce(index uint64, last bool) []byte {
	nonce := make([]byte, nonceSize)
	binary.BigEndian.PutUint64(nonce[3:11], index)
	if last {
		nonce[11] = 1
	}
	return nonce
}

func blobAEAD(contentKey []byte, plaintextDigest string, chunkSize int) (cipher.AEAD, error) {
	if chunkSize <= 0 || chunkSize > math.MaxInt32-tagSize {
		return nil, fmt.Errorf("invalid chunk size %d", chunkSize)
	}
	if !strings.HasPrefix(plaintextDigest, "sha256:") {
		return nil, fmt.Errorf("plaintext digest must be sha256:<hex>")
	}
	key, err := BlobKey(contentKey, plaintextDigest, uint32(chunkSize))
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func digest(h hash.Hash) string { return "sha256:" + hex.EncodeToString(h.Sum(nil)) }

type countingHash struct {
	hash.Hash
	n uint64
}

func (c *countingHash) Write(p []byte) (int, error) {
	c.n += uint64(len(p))
	return c.Hash.Write(p)
}

// EncryptBlob reads one blob's plaintext from src and writes its ciphertext to
// dst: chunkSize plaintext bytes per AES-256-GCM chunk (the last holds
// 1..chunkSize; an empty blob is one empty last chunk), each chunk ct‖tag,
// concatenated, no AAD. plaintextDigest is the digest the manifest names (the
// per-blob key derives from it); the plaintext read must match it. On any
// error the caller must discard dst and never upload it: ciphertext of a
// plaintext that does not match its digest shares keys and nonces with the
// real blob's.
func EncryptBlob(dst io.Writer, src io.Reader, contentKey []byte, plaintextDigest string, chunkSize int) (Blob, error) {
	gcm, err := blobAEAD(contentKey, plaintextDigest, chunkSize)
	if err != nil {
		return Blob{}, err
	}
	in := bufio.NewReaderSize(src, chunkSize)
	plain := &countingHash{Hash: sha256.New()}
	cipherSum := &countingHash{Hash: sha256.New()}
	out := io.MultiWriter(dst, cipherSum)
	buf := make([]byte, chunkSize, chunkSize+tagSize)
	for index := uint64(0); ; index++ {
		if index > maxChunk {
			return Blob{}, errors.New("blob has too many chunks")
		}
		n, err := io.ReadFull(in, buf[:chunkSize])
		last := err == io.EOF || err == io.ErrUnexpectedEOF
		if err != nil && !last {
			return Blob{}, err
		}
		if !last {
			if _, err := in.Peek(1); err == io.EOF {
				last = true
			} else if err != nil {
				return Blob{}, err
			}
		}
		plain.Write(buf[:n])
		if _, err := out.Write(gcm.Seal(buf[:0], chunkNonce(index, last), buf[:n], nil)); err != nil {
			return Blob{}, err
		}
		buf = buf[:chunkSize]
		if last {
			break
		}
	}
	if got := digest(plain); got != plaintextDigest {
		return Blob{}, fmt.Errorf("blob plaintext is %s, manifest names %s", got, plaintextDigest)
	}
	return Blob{PlaintextDigest: plaintextDigest, CiphertextDigest: digest(cipherSum), PlaintextSize: plain.n, CiphertextSize: cipherSum.n}, nil
}

// DecryptBlob reads a blob's ciphertext from src and writes its plaintext to
// dst, chunk by chunk as each authenticates. It checks the ciphertext digest
// and size and the plaintext digest and size against want. On any error the
// caller must discard everything written to dst: the digests are only known
// at the end.
func DecryptBlob(dst io.Writer, src io.Reader, contentKey []byte, want Blob, chunkSize int) error {
	gcm, err := blobAEAD(contentKey, want.PlaintextDigest, chunkSize)
	if err != nil {
		return err
	}
	cipherSum := &countingHash{Hash: sha256.New()}
	in := bufio.NewReaderSize(io.TeeReader(src, cipherSum), chunkSize+tagSize)
	plain := &countingHash{Hash: sha256.New()}
	out := io.MultiWriter(dst, plain)
	buf := make([]byte, chunkSize+tagSize)
	for index := uint64(0); ; index++ {
		if index > maxChunk {
			return errors.New("blob has too many chunks")
		}
		n, err := io.ReadFull(in, buf)
		last := err == io.EOF || err == io.ErrUnexpectedEOF
		if err != nil && !last {
			return err
		}
		if !last {
			if _, err := in.Peek(1); err == io.EOF {
				last = true
			} else if err != nil {
				return err
			}
		}
		// Only an empty blob has an empty last chunk.
		if n < tagSize || last && n == tagSize && index > 0 {
			return ErrOpen
		}
		pt, err := gcm.Open(buf[:0], chunkNonce(index, last), buf[:n], nil)
		if err != nil {
			return ErrOpen
		}
		if _, err := out.Write(pt); err != nil {
			return err
		}
		if last {
			break
		}
	}
	if digest(cipherSum) != want.CiphertextDigest || cipherSum.n != want.CiphertextSize {
		return fmt.Errorf("blob ciphertext does not match its digest or size")
	}
	if digest(plain) != want.PlaintextDigest || plain.n != want.PlaintextSize {
		return fmt.Errorf("blob plaintext does not match its digest or size")
	}
	return nil
}

// appendLP appends a uint16 big-endian length prefix and the value.
func appendLP(b []byte, v string) ([]byte, error) {
	if len(v) > math.MaxUint16 {
		return nil, errors.New("registry binding field too long")
	}
	return append(binary.BigEndian.AppendUint16(b, uint16(len(v))), v...), nil
}

func appendAll(b []byte, fields ...string) ([]byte, error) {
	var err error
	for _, f := range fields {
		if b, err = appendLP(b, f); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// Scope kinds of a WrapScope, as encoded in the wrap info.
const (
	ScopeDeployment byte = 1 // WrapScope.deployment_id: the wrap opens a deployment key
	ScopeRelease    byte = 2 // WrapScope.app_release_id: the wrap opens a content key
)

// Wrap binds one KeyWrap to everything it may open for: tenant, scope, the
// wrapped key and the recipient. RecipientKind is the RecipientKind enum
// value; Recipient is empty for the custodian.
type Wrap struct {
	OrganizationID string `json:"organizationId"`
	Scope          byte   `json:"scope"`
	ScopeID        string `json:"scopeId"`
	KeyID          string `json:"keyId"`
	RecipientKind  byte   `json:"recipientKind"`
	Recipient      string `json:"recipient"`
	RecipientKeyID string `json:"recipientKeyId"`
}

// info = "wendy-registry-wrap-v1" ‖ lp(org) ‖ u8 scope kind ‖ lp(scope id) ‖
// lp(key_id) ‖ u8 recipient_kind ‖ lp(recipient) ‖ lp(recipient_key_id).
func (w Wrap) info() ([]byte, error) {
	if w.Scope != ScopeDeployment && w.Scope != ScopeRelease {
		return nil, fmt.Errorf("unknown wrap scope %d", w.Scope)
	}
	b, err := appendLP([]byte(wrapLabel), w.OrganizationID)
	if err != nil {
		return nil, err
	}
	if b, err = appendAll(append(b, w.Scope), w.ScopeID, w.KeyID); err != nil {
		return nil, err
	}
	return appendAll(append(b, w.RecipientKind), w.Recipient, w.RecipientKeyID)
}

// Seal wraps key (a content or deployment key, w.KeyID) to the recipient
// public key (w.RecipientKeyID) and returns KeyWrap.enc and .ciphertext.
func (w Wrap) Seal(recipientPublicKey, key []byte) (enc, ciphertext []byte, err error) {
	if len(key) != KeySize || KeyID(key) != w.KeyID {
		return nil, nil, errors.New("wrapped key does not match the wrap's key_id")
	}
	if KeyID(recipientPublicKey) != w.RecipientKeyID {
		return nil, nil, errors.New("recipient public key does not match recipient_key_id")
	}
	pub, err := KEM.NewPublicKey(recipientPublicKey)
	if err != nil {
		return nil, nil, err
	}
	info, err := w.info()
	if err != nil {
		return nil, nil, err
	}
	enc, sender, err := hpke.NewSender(pub, kdf, aead, info)
	if err != nil {
		return nil, nil, err
	}
	ciphertext, err = sender.Seal(nil, key)
	return enc, ciphertext, err
}

// Open unwraps a KeyWrap with the recipient's private key and checks that the
// result is the key w.KeyID names.
func (w Wrap) Open(priv hpke.PrivateKey, enc, ciphertext []byte) ([]byte, error) {
	if KeyID(priv.PublicKey().Bytes()) != w.RecipientKeyID {
		return nil, errors.New("wrap is for another recipient key")
	}
	info, err := w.info()
	if err != nil {
		return nil, err
	}
	recipient, err := hpke.NewRecipient(enc, priv, kdf, aead, info)
	if err != nil {
		return nil, ErrOpen
	}
	key, err := recipient.Open(nil, ciphertext)
	if err != nil || len(key) != KeySize || KeyID(key) != w.KeyID {
		return nil, ErrOpen
	}
	return key, nil
}

// Rewrap opens a wrap this recipient holds and wraps the same key onward to
// another recipient: how a known operator device grants a newly signed-in one
// (GetMyWraps, then AddWraps). The key never leaves this function.
func Rewrap(priv hpke.PrivateKey, held Wrap, enc, ciphertext []byte, onward Wrap, recipientPublicKey []byte) (onwardEnc, onwardCiphertext []byte, err error) {
	key, err := held.Open(priv, enc, ciphertext)
	if err != nil {
		return nil, nil, err
	}
	defer clear(key)
	return onward.Seal(recipientPublicKey, key)
}

// SealedKey binds one SealedContentKey: the content key sealed under a
// deployment key for one deployment and manifest.
type SealedKey struct {
	OrganizationID  string `json:"organizationId"`
	DeploymentID    string `json:"deploymentId"`
	ManifestDigest  string `json:"manifestDigest"`
	ContentKeyID    string `json:"contentKeyId"`
	DeploymentKeyID string `json:"deploymentKeyId"`
}

// aad = "wendy-registry-seal-v1" ‖ lp(org) ‖ lp(deployment_id) ‖
// lp(manifest_digest) ‖ lp(content_key_id) ‖ lp(deployment_key_id).
func (s SealedKey) aad() ([]byte, error) {
	return appendAll([]byte(sealLabel), s.OrganizationID, s.DeploymentID, s.ManifestDigest, s.ContentKeyID, s.DeploymentKeyID)
}

func deploymentAEAD(deploymentKey []byte, id string) (cipher.AEAD, error) {
	if len(deploymentKey) != KeySize || KeyID(deploymentKey) != id {
		return nil, errors.New("deployment key does not match deployment_key_id")
	}
	block, err := aes.NewCipher(deploymentKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal seals the content key under the deployment key with a random 12-byte
// nonce and returns SealedContentKey.nonce and .ciphertext.
func (s SealedKey) Seal(deploymentKey, contentKey []byte) (nonce, ciphertext []byte, err error) {
	if len(contentKey) != KeySize || KeyID(contentKey) != s.ContentKeyID {
		return nil, nil, errors.New("content key does not match content_key_id")
	}
	gcm, err := deploymentAEAD(deploymentKey, s.DeploymentKeyID)
	if err != nil {
		return nil, nil, err
	}
	aad, err := s.aad()
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, nonceSize)
	rand.Read(nonce)
	return nonce, gcm.Seal(nil, nonce, contentKey, aad), nil
}

// Open recovers the content key and checks it is the one ContentKeyID names.
func (s SealedKey) Open(deploymentKey, nonce, ciphertext []byte) ([]byte, error) {
	gcm, err := deploymentAEAD(deploymentKey, s.DeploymentKeyID)
	if err != nil {
		return nil, err
	}
	aad, err := s.aad()
	if err != nil {
		return nil, err
	}
	if len(nonce) != nonceSize {
		return nil, ErrOpen
	}
	key, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil || len(key) != KeySize || KeyID(key) != s.ContentKeyID {
		return nil, ErrOpen
	}
	return key, nil
}
