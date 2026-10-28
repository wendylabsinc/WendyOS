package localmesh

import "github.com/wendylabsinc/wendy/go/internal/agent/netwire"

type manifestBinary struct {
	Principal string             `json:"principal" cbor:"p"`
	Body      netwire.RawMessage `json:"body" cbor:"b"`
	Signature []byte             `json:"signature" cbor:"s"`
}

func (m SignedManifest) MarshalCBOR() ([]byte, error) {
	return netwire.Marshal(manifestBinary{m.Principal, netwire.RawMessage(m.Body), m.Signature})
}
func (m *SignedManifest) UnmarshalCBOR(data []byte) error {
	var wire manifestBinary
	if err := netwire.UnmarshalSigned(data, &wire); err != nil {
		return err
	}
	*m = SignedManifest{Principal: wire.Principal, Body: wire.Body, Signature: wire.Signature}
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
