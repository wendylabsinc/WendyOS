// Package meshalpn carries bounded certificate-availability hints alongside
// Wendy Net's application protocol negotiation. TLS still verifies every path.
package meshalpn

import (
	"crypto/sha256"
	"strings"
)

const WendyCom = "wendy-com/2"

const FingerprintSize = 16
const MaxChain = 8
const MaxCached = 64
const marker = "\x00WCI\x01"

// Inventory offers are auxiliary and are never selected as an application.
const inventory = "wendy-net-cached\x00"

type Fingerprint [FingerprintSize]byte

func Hash(der []byte) Fingerprint {
	sum := sha256.Sum256(der)
	return Fingerprint(sum[:FingerprintSize])
}

func variant(protocol string, hashes []Fingerprint) string {
	var encoded strings.Builder
	encoded.WriteString(protocol)
	encoded.WriteString(marker)
	for _, hash := range hashes {
		encoded.Write(hash[:])
	}
	return encoded.String()
}

// Decode recognizes only complete, bounded Wendy variants. Ordinary protocol
// names are returned unchanged. Fingerprints are opaque binary, not base64.
func Decode(protocol string) (string, []Fingerprint) {
	index := strings.Index(protocol, marker)
	if index <= 0 {
		return protocol, nil
	}
	data := protocol[index+len(marker):]
	if len(protocol) > 255 || len(data) == 0 || len(data)%FingerprintSize != 0 || len(data)/FingerprintSize > MaxChain {
		return protocol, nil
	}
	hashes := make([]Fingerprint, len(data)/FingerprintSize)
	for i := range hashes {
		copy(hashes[i][:], data[i*FingerprintSize:])
	}
	return protocol[:index], hashes
}

func Application(protocol string) string { base, _ := Decode(protocol); return base }

// Offer lists root-first chain prefixes, longest first. Selecting one explicitly
// acknowledges that the server possesses that exact prefix. The final ordinary
// name permits a full-chain handshake when no prefix is shared. Additional cached
// CA fingerprints are availability hints only, separate from the chain order.
func Offer(protocol string, chain, cached []Fingerprint) []string {
	result := make([]string, 0, len(chain)+2)
	if len(chain) > MaxChain {
		chain = chain[:MaxChain]
	}
	for count := len(chain); count > 0; count-- {
		encoded := variant(protocol, chain[:count])
		if len(encoded) <= 255 {
			result = append(result, encoded)
		}
	}
	result = append(result, protocol)
	seen := make(map[Fingerprint]bool, len(chain))
	for _, hash := range chain {
		seen[hash] = true
	}
	var extra []Fingerprint
	for _, hash := range cached {
		if !seen[hash] {
			extra = append(extra, hash)
			seen[hash] = true
		}
		if len(extra) == MaxCached {
			break
		}
	}
	for len(extra) > 0 {
		count := min(len(extra), MaxChain)
		var encoded strings.Builder
		encoded.WriteString(inventory)
		for _, hash := range extra[:count] {
			encoded.Write(hash[:])
		}
		result = append(result, encoded.String())
		extra = extra[count:]
	}
	return result
}

// Select preserves server application preference while choosing the longest
// client prefix for which every CA is available locally. It does not add trust.
func Select(supported, offered []string, available func(Fingerprint) bool) string {
	for _, protocol := range supported {
		best := ""
		length := -1
		for _, candidate := range offered {
			base, hashes := Decode(candidate)
			if base != protocol || len(hashes) <= length {
				continue
			}
			known := true
			for _, hash := range hashes {
				if !available(hash) {
					known = false
					break
				}
			}
			if known {
				best = candidate
				length = len(hashes)
			}
		}
		if best != "" {
			return best
		}
	}
	return ""
}

func Offered(offered []string, protocol string) map[Fingerprint]bool {
	result := make(map[Fingerprint]bool)
	for _, candidate := range offered {
		base, hashes := Decode(candidate)
		if base == protocol || (protocol == "" && len(hashes) > 0) {
			for _, hash := range hashes {
				result[hash] = true
			}
			continue
		}
		if !strings.HasPrefix(candidate, inventory) {
			continue
		}
		data := candidate[len(inventory):]
		if len(data) == 0 || len(data)%FingerprintSize != 0 || len(data) > MaxChain*FingerprintSize {
			continue
		}
		for offset := 0; offset < len(data); offset += FingerprintSize {
			var hash Fingerprint
			copy(hash[:], data[offset:])
			result[hash] = true
		}
	}
	return result
}
