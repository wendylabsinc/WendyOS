package roughtime

import (
	"encoding/binary"
	"fmt"
	"sort"
)

// Tag constants: 4-byte ASCII strings interpreted as little-endian uint32.
// To compute: given ASCII string s, tag = s[0] | s[1]<<8 | s[2]<<16 | s[3]<<24.
const (
	TagCERT uint32 = 0x54524543 // "CERT"
	TagDELE uint32 = 0x454C4544 // "DELE"
	TagSIG  uint32 = 0x00474953 // "SIG\x00"
	TagVER  uint32 = 0x00524556 // "VER\x00"
	TagMAXT uint32 = 0x5458414D // "MAXT"
	TagNONC uint32 = 0x434E4F4E // "NONC"
	TagMINT uint32 = 0x544E494D // "MINT"
	TagPATH uint32 = 0x48544150 // "PATH"
	TagPUBK uint32 = 0x4B425550 // "PUBK"
	TagRADI uint32 = 0x49444152 // "RADI"
	TagMIDP uint32 = 0x5044494D // "MIDP"
	TagSREP uint32 = 0x50455253 // "SREP"
	TagROOT uint32 = 0x544F4F52 // "ROOT"
	TagINDX uint32 = 0x58444E49 // "INDX"
	TagSRV  uint32 = 0x00565253 // "SRV\x00"
	TagZZZZ uint32 = 0x5A5A5A5A // "ZZZZ"

	VersionDraft08 uint32 = 0x80000008
	VersionDraft11 uint32 = 0x8000000b

	// SigContext is the domain-separation string prepended to SREP before signing.
	SigContext = "RoughTime v1 response signature\x00"
	// CertContext is the domain-separation string prepended to DELE before signing.
	CertContext = "RoughTime v1 delegation signature--\x00"
)

// EncodeMessage encodes a map of tag→value pairs into the IETF Roughtime TLV format.
// Tags are sorted in ascending order as required by the spec.
func EncodeMessage(m map[uint32][]byte) []byte {
	if len(m) == 0 {
		return make([]byte, 4) // Zero tags still require the tag-count word.
	}
	type pair struct {
		tag uint32
		val []byte
	}
	pairs := make([]pair, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, pair{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].tag < pairs[j].tag })

	n := uint32(len(pairs))
	totalValLen := 0
	for _, p := range pairs {
		totalValLen += len(p.val)
	}
	// Layout: [num_tags uint32] [(n-1) offsets uint32 each] [n tags uint32 each] [values]
	headerLen := int(4 + (n-1)*4 + n*4)
	buf := make([]byte, headerLen+totalValLen)

	binary.LittleEndian.PutUint32(buf[0:], n)

	cumulative := 0
	for i, p := range pairs {
		if i < int(n)-1 {
			cumulative += len(p.val)
			binary.LittleEndian.PutUint32(buf[4+i*4:], uint32(cumulative))
		}
		binary.LittleEndian.PutUint32(buf[4+int(n-1)*4+i*4:], p.tag)
	}

	offset := headerLen
	for _, p := range pairs {
		copy(buf[offset:], p.val)
		offset += len(p.val)
	}
	return buf
}

// DecodeMessage parses an IETF Roughtime TLV message.
func DecodeMessage(b []byte) (map[uint32][]byte, error) {
	if len(b) < 4 {
		return nil, fmt.Errorf("roughtime message too short: %d bytes", len(b))
	}
	n := binary.LittleEndian.Uint32(b[0:4])
	if n == 0 {
		return map[uint32][]byte{}, nil
	}
	// Each tag entry occupies at least 4 bytes; n cannot exceed the buffer.
	if uint64(n) > uint64(len(b)) {
		return nil, fmt.Errorf("roughtime message: implausible num_tags %d for %d-byte buffer", n, len(b))
	}
	// Compute header length in uint64 to avoid uint32 overflow on large n.
	// Header layout: [num_tags:4] [(n-1) offsets:4 each] [n tags:4 each]
	headerLen64 := uint64(4) + uint64(n-1)*4 + uint64(n)*4
	if uint64(len(b)) < headerLen64 {
		return nil, fmt.Errorf("roughtime message header truncated (need %d have %d)", headerLen64, len(b))
	}
	headerLen := uint32(headerLen64)

	tags := make([]uint32, n)
	for i := uint32(0); i < n; i++ {
		tags[i] = binary.LittleEndian.Uint32(b[4+(n-1)*4+i*4:])
	}

	// Collect end-offsets; the last value ends at len(b)-headerLen.
	endOffsets := make([]uint32, n)
	for i := uint32(0); i < n-1; i++ {
		endOffsets[i] = binary.LittleEndian.Uint32(b[4+i*4:])
	}
	endOffsets[n-1] = uint32(len(b)) - headerLen

	m := make(map[uint32][]byte, n)
	start := uint32(0)
	for i := uint32(0); i < n; i++ {
		end := endOffsets[i]
		// Use uint64 for the upper-bound check to avoid overflow when headerLen+end wraps.
		if end < start || uint64(headerLen)+uint64(end) > uint64(len(b)) {
			return nil, fmt.Errorf("roughtime message: value for tag %d out of bounds", i)
		}
		val := make([]byte, end-start)
		copy(val, b[headerLen+start:headerLen+end])
		m[tags[i]] = val
		start = end
	}
	return m, nil
}
