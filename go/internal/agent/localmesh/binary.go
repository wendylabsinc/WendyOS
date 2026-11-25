package localmesh

import (
	"bytes"
	"crypto/sha256"

	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
)

type manifestBinary struct {
	Principal string             `json:"principal" cbor:"p"`
	Body      netwire.RawMessage `json:"body" cbor:"b"`
	Signature []byte             `json:"signature" cbor:"s"`
}

// manifestEnvelope preserves optional transport metadata for gossip's exact
// artifact digest. The known-field fingerprint invalidates it if a caller edits
// any public field, including an in-place body or signature slice mutation.
// Its bytes are privately owned and immutable, so directory clones may share it.
type manifestEnvelope struct {
	data   []byte
	fields [sha256.Size]byte
}

func (m SignedManifest) MarshalCBOR() ([]byte, error) {
	known, err := netwire.Marshal(manifestBinary{m.Principal, netwire.RawMessage(m.Body), m.Signature})
	if err != nil {
		return nil, err
	}
	if m.envelope != nil && sha256.Sum256(known) == m.envelope.fields {
		return bytes.Clone(m.envelope.data), nil
	}
	return known, nil
}
func (m *SignedManifest) UnmarshalCBOR(data []byte) error {
	var wire manifestBinary
	// The signed versioned body remains exact RawMessage bytes. The surrounding
	// envelope carries optional metadata within the existing control-frame cap.
	if err := netwire.UnmarshalExtensibleArtifact(data, &wire, MaxControlMessage); err != nil {
		return err
	}
	known, err := netwire.Marshal(wire)
	if err != nil {
		return err
	}
	next := SignedManifest{Principal: wire.Principal, Body: wire.Body, Signature: wire.Signature}
	if !bytes.Equal(data, known) {
		next.envelope = &manifestEnvelope{data: bytes.Clone(data), fields: sha256.Sum256(known)}
	}
	*m = next
	return nil
}

type chainBinary struct {
	Principal string   `json:"principal" cbor:"p"`
	Hashes    [][]byte `json:"hashes" cbor:"h"`
}

func (m CertificateChain) MarshalCBOR() ([]byte, error) {
	wire := chainBinary{Principal: m.Principal, Hashes: make([][]byte, len(m.Hashes))}
	for i, text := range m.Hashes {
		bytes, err := netwire.HashBytes(text)
		if err != nil {
			return nil, err
		}
		wire.Hashes[i] = bytes
	}
	return netwire.Marshal(wire)
}
func (m *CertificateChain) UnmarshalCBOR(data []byte) error {
	var wire chainBinary
	if err := netwire.Unmarshal(data, &wire); err != nil {
		return err
	}
	result := CertificateChain{Principal: wire.Principal, Hashes: make([]string, len(wire.Hashes))}
	for i, bytes := range wire.Hashes {
		text, err := netwire.HashText(bytes)
		if err != nil {
			return err
		}
		result.Hashes[i] = text
	}
	*m = result
	return nil
}

type requestBinary struct {
	Hash   []byte `json:"hash" cbor:"h"`
	Offset int    `json:"offset" cbor:"f"`
}

func (m CertificateRequest) MarshalCBOR() ([]byte, error) {
	hash, err := netwire.HashBytes(m.Hash)
	if err != nil {
		return nil, err
	}
	return netwire.Marshal(requestBinary{hash, m.Offset})
}
func (m *CertificateRequest) UnmarshalCBOR(data []byte) error {
	var wire requestBinary
	if err := netwire.Unmarshal(data, &wire); err != nil {
		return err
	}
	hash, err := netwire.HashText(wire.Hash)
	if err != nil {
		return err
	}
	*m = CertificateRequest{hash, wire.Offset}
	return nil
}

type chunkBinary struct {
	Hash   []byte `json:"hash" cbor:"h"`
	Offset int    `json:"offset" cbor:"f"`
	Total  int    `json:"total" cbor:"l"`
	Data   []byte `json:"data" cbor:"d"`
}

func (m CertificateChunk) MarshalCBOR() ([]byte, error) {
	hash, err := netwire.HashBytes(m.Hash)
	if err != nil {
		return nil, err
	}
	return netwire.Marshal(chunkBinary{hash, m.Offset, m.Total, m.Data})
}
func (m *CertificateChunk) UnmarshalCBOR(data []byte) error {
	var wire chunkBinary
	if err := netwire.Unmarshal(data, &wire); err != nil {
		return err
	}
	hash, err := netwire.HashText(wire.Hash)
	if err != nil {
		return err
	}
	*m = CertificateChunk{hash, wire.Offset, wire.Total, wire.Data}
	return nil
}
