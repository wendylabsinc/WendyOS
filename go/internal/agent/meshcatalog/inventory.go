package meshcatalog

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
)

const inventoryChunkEntries = 128
const maxInventoryEntries = 1024

// Version 3 alone adds a current cache inventory. The v1/v2 Message parser is
// deliberately unchanged: old peers must never receive this frame shape.
type inventoryMessage struct {
	Message
	Fingerprints []byte `json:"fingerprints,omitempty"`
}

func (m inventoryMessage) validate() error {
	switch m.Kind {
	case "identity-cache":
		if m.Record != nil || len(m.Bundle) != 0 || m.Fingerprint != "" || len(m.Fingerprints) == 0 || len(m.Fingerprints)%32 != 0 || len(m.Fingerprints) > inventoryChunkEntries*32 {
			return errors.New("invalid catalog identity inventory")
		}
	case "identity-cache-done":
		if m.Record != nil || len(m.Bundle) != 0 || m.Fingerprint != "" || len(m.Fingerprints) != 0 {
			return errors.New("invalid catalog inventory completion")
		}
	default:
		if len(m.Fingerprints) != 0 {
			return errors.New("unexpected catalog inventory field")
		}
		return m.Message.validate()
	}
	return nil
}
func inventoryWire(m inventoryMessage) ([]byte, error) {
	if err := m.validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 || len(b) > MaxMessageBytes {
		return nil, errors.New("oversized catalog inventory frame")
	}
	return b, nil
}
func writeInventoryMessage(w io.Writer, m inventoryMessage) error {
	b, err := inventoryWire(m)
	if err != nil {
		return err
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if err = writeFull(w, hdr[:]); err != nil {
		return err
	}
	return writeFull(w, b)
}
func readInventoryMessage(r io.Reader) (inventoryMessage, error) {
	var m inventoryMessage
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return m, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > MaxMessageBytes {
		return m, errors.New("invalid catalog inventory frame length")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return m, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, err
	}
	if d.Decode(new(any)) != io.EOF {
		return m, errors.New("trailing catalog inventory data")
	}
	return m, m.validate()
}
func inventoryMessages(fingerprints []string) []inventoryMessage {
	var out []inventoryMessage
	for len(fingerprints) > 0 {
		n := min(inventoryChunkEntries, len(fingerprints))
		packed := make([]byte, 0, n*32)
		for _, fp := range fingerprints[:n] {
			b, err := hex.DecodeString(fp)
			if err != nil || len(b) != 32 {
				panic("invalid internal identity fingerprint")
			}
			packed = append(packed, b...)
		}
		out = append(out, inventoryMessage{Message: Message{Kind: "identity-cache"}, Fingerprints: packed})
		fingerprints = fingerprints[n:]
	}
	return append(out, inventoryMessage{Message: Message{Kind: "identity-cache-done"}})
}

type receivedInventory struct {
	fingerprints map[string]bool
	done         bool
}

func (i *receivedInventory) receive(m inventoryMessage) error {
	if i.done {
		return errors.New("catalog inventory already complete")
	}
	if err := m.validate(); err != nil {
		return err
	}
	if m.Kind == "identity-cache-done" {
		i.done = true
		return nil
	}
	if m.Kind != "identity-cache" {
		return errors.New("catalog data before inventory completion")
	}
	if i.fingerprints == nil {
		i.fingerprints = make(map[string]bool)
	}
	if len(i.fingerprints)+len(m.Fingerprints)/32 > maxInventoryEntries {
		return errors.New("catalog inventory exceeds limit")
	}
	for p := 0; p < len(m.Fingerprints); p += 32 {
		fp := hex.EncodeToString(m.Fingerprints[p : p+32])
		if i.fingerprints[fp] {
			return errors.New("duplicate catalog inventory identity")
		}
		i.fingerprints[fp] = true
	}
	return nil
}

// Unlike previous-session hints, this set was reported by the current receiver.
// It changes only bundle transmission; Catalog.Accept still checks current trust.
func (s *Synchronizer) useCurrentInventory(i receivedInventory) {
	for fp := range i.fingerprints {
		s.sent[fp] = true
	}
}
