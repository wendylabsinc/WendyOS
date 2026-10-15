// Package bleprovider discovers Wendy peers with BlueZ and carries authenticated
// local-mesh links over LE L2CAP credit-based channels and TLS 1.3.
package bleprovider

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

const (
	// ALPN is distinct from the QUIC carrier. A BLE link is a reliable byte
	// stream and uses TLS directly, with no QUIC framing or second encryption.
	ALPN = "wendy-local-mesh-ble/1"
	// LinkCost keeps BLE available as a fallback while preferring TCP (256)
	// and NAN (512) routes when they exist.
	LinkCost           uint16 = 4096
	DefaultPSM         uint16 = 0x0081
	DefaultTargetPeers        = 3
	// serviceUUIDPrefix is a randomly selected, fixed 128-bit namespace. The
	// final 32 bits are offset by the organization ID (modulo 2^32).
	serviceUUIDPrefix        = "9fd0d83a-47dc-41ef-b85d-"
	serviceUUIDLow    uint32 = 0x211be774
	AdvertisementSize        = 10
)

var ErrInvalidAdvertisement = errors.New("invalid Wendy BLE advertisement")

// ServiceUUID is the org-scoped discovery UUID. The offset is a bijection
// across all 32-bit organization IDs; TLS still authenticates both org and
// asset, since an advertisement is untrusted input.
func ServiceUUID(org int32) (string, error) {
	if org <= 0 {
		return "", errors.New("BLE organization ID must be positive")
	}
	return serviceUUIDPrefix + fmt.Sprintf("%012x", uint64(serviceUUIDLow+uint32(org))), nil
}

// Advertisement holds the small, non-secret peer hint carried as ServiceData.
// It is exactly ten bytes so a 128-bit ServiceData field and LE flags fit in
// a legacy 31-byte advertising packet. No trust decision uses this alone.
type Advertisement struct {
	Asset    int32
	MeshHash [4]byte
	PSM      uint16
}

func MeshNameHash(name string) [4]byte {
	sum := sha256.Sum256([]byte(name))
	return [4]byte(sum[:4])
}

func ValidPSM(psm uint16) bool { return psm >= 0x0080 && psm <= 0x00ff }

func NewAdvertisement(asset int32, meshName string, psm uint16) (Advertisement, error) {
	if asset <= 0 || meshName == "" || !ValidPSM(psm) {
		return Advertisement{}, ErrInvalidAdvertisement
	}
	return Advertisement{Asset: asset, MeshHash: MeshNameHash(meshName), PSM: psm}, nil
}

func (a Advertisement) MarshalBinary() ([]byte, error) {
	if a.Asset <= 0 || !ValidPSM(a.PSM) {
		return nil, ErrInvalidAdvertisement
	}
	b := make([]byte, AdvertisementSize)
	binary.BigEndian.PutUint32(b[:4], uint32(a.Asset))
	copy(b[4:8], a.MeshHash[:])
	binary.BigEndian.PutUint16(b[8:10], a.PSM)
	return b, nil
}

func ParseAdvertisement(data []byte, meshName string) (Advertisement, error) {
	if len(data) != AdvertisementSize || meshName == "" {
		return Advertisement{}, ErrInvalidAdvertisement
	}
	a := Advertisement{Asset: int32(binary.BigEndian.Uint32(data[:4])), PSM: binary.BigEndian.Uint16(data[8:10])}
	copy(a.MeshHash[:], data[4:8])
	if a.Asset <= 0 || !ValidPSM(a.PSM) || a.MeshHash != MeshNameHash(meshName) {
		return Advertisement{}, ErrInvalidAdvertisement
	}
	return a, nil
}

func matchingServiceData(data map[string][]byte, uuid, meshName string) (Advertisement, bool) {
	for key, raw := range data {
		if strings.EqualFold(key, uuid) {
			a, err := ParseAdvertisement(raw, meshName)
			return a, err == nil
		}
	}
	return Advertisement{}, false
}
