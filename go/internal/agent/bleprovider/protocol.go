// Package bleprovider discovers Wendy peers with BlueZ and carries authenticated
// wendy net links over LE L2CAP credit-based channels and TLS 1.3.
package bleprovider

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math/big"
	"strings"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const (
	// ALPN is distinct from the QUIC carrier. A BLE link is a reliable byte
	// stream and uses TLS directly, with no QUIC framing or second encryption.
	ALPN = localmesh.StreamALPN
	// LinkCost keeps BLE available as a fallback while preferring TCP (256)
	// and NAN (512) routes when they exist.
	LinkCost   uint16 = 4096
	DefaultPSM uint16 = 0x0081
	// AdvertisementSize leaves room for flags (3), ServiceData header (2),
	// and the 128-bit tenant service UUID (16) in a legacy 31-byte packet.
	AdvertisementSize         = 10
	LocalNameSize             = 13
	AdvertisementVersion byte = 1
)

const (
	CapabilityNativeAccepting byte = 1 << iota
	CapabilityTimeRelay
)

var ErrInvalidAdvertisement = errors.New("invalid Wendy BLE advertisement")

// ServiceUUID is the tenant-scoped discovery UUID: the tenant UUID itself.
// Advertisements are untrusted input; TLS authenticates both devices.
func ServiceUUID(tenant meshid.Tenant) string {
	if tenant == (meshid.Tenant{}) {
		return ""
	}
	return tenant.String()
}

// Advertisement is an untrusted, tenant-scoped discovery hint. Binary v1 is
// version/capabilities/native PSM (BE)/network hash/UUID prefix, exactly ten
// bytes. Capabilities describe independent endpoints, never authentication.
type Advertisement struct {
	Version      byte
	Capabilities byte
	DeviceHint   [4]byte
	NetworkHash  [2]byte
	PSM          uint16
}

func NetworkNameHash(name string) [2]byte {
	sum := sha256.Sum256([]byte(name))
	return [2]byte(sum[:2])
}

func ValidPSM(psm uint16) bool { return psm >= 0x0080 && psm <= 0x00ff }

func NewAdvertisement(device uuid.UUID, networkName string, psm uint16) (Advertisement, error) {
	if device == uuid.Nil || networkName == "" || !ValidPSM(psm) {
		return Advertisement{}, ErrInvalidAdvertisement
	}
	return Advertisement{Version: AdvertisementVersion, Capabilities: CapabilityNativeAccepting | CapabilityTimeRelay, DeviceHint: deviceHint(device), NetworkHash: NetworkNameHash(networkName), PSM: psm}, nil
}

func (a Advertisement) MarshalBinary() ([]byte, error) {
	if a.Version != AdvertisementVersion || !ValidPSM(a.PSM) {
		return nil, ErrInvalidAdvertisement
	}
	b := make([]byte, AdvertisementSize)
	b[0] = a.Version
	b[1] = a.Capabilities
	binary.BigEndian.PutUint16(b[2:4], a.PSM)
	copy(b[4:6], a.NetworkHash[:])
	copy(b[6:10], a.DeviceHint[:])
	return b, nil
}

func ParseAdvertisement(data []byte, networkName string) (Advertisement, error) {
	if len(data) != AdvertisementSize || networkName == "" {
		return Advertisement{}, ErrInvalidAdvertisement
	}
	a := Advertisement{Version: data[0], Capabilities: data[1], PSM: binary.BigEndian.Uint16(data[2:4])}
	copy(a.NetworkHash[:], data[4:6])
	copy(a.DeviceHint[:], data[6:10])
	if a.Version != AdvertisementVersion || !ValidPSM(a.PSM) || a.NetworkHash != NetworkNameHash(networkName) {
		return Advertisement{}, ErrInvalidAdvertisement
	}
	return a, nil
}

func (a Advertisement) NativeAccepting() bool    { return a.Capabilities&CapabilityNativeAccepting != 0 }
func (a Advertisement) TimeRelayAvailable() bool { return a.Capabilities&CapabilityTimeRelay != 0 }

// MarshalLocalName encodes the exact canonical binary bytes as thirteen
// fixed-width base94 characters. No alternate compressed field layout exists.
func (a Advertisement) MarshalLocalName() (string, error) {
	raw, err := a.MarshalBinary()
	if err != nil {
		return "", err
	}
	return encodeLocalName(raw)
}

func encodeLocalName(raw []byte) (string, error) {
	if len(raw) != AdvertisementSize {
		return "", ErrInvalidAdvertisement
	}
	value := new(big.Int).SetBytes(raw)
	radix, digit := big.NewInt(94), new(big.Int)
	var name [LocalNameSize]byte
	for i := len(name) - 1; i >= 0; i-- {
		value.QuoRem(value, radix, digit)
		name[i] = '!' + byte(digit.Uint64())
	}
	return string(name[:]), nil
}

func decodeLocalName(name string) ([]byte, error) {
	if len(name) != LocalNameSize {
		return nil, ErrInvalidAdvertisement
	}
	value, radix, digit := new(big.Int), big.NewInt(94), new(big.Int)
	for i := range len(name) {
		if name[i] < '!' || name[i] > '~' {
			return nil, ErrInvalidAdvertisement
		}
		value.Mul(value, radix).Add(value, digit.SetUint64(uint64(name[i]-'!')))
	}
	if value.BitLen() > AdvertisementSize*8 {
		return nil, ErrInvalidAdvertisement
	}
	return value.FillBytes(make([]byte, AdvertisementSize)), nil
}

func ParseLocalName(name, networkName string) (Advertisement, error) {
	raw, err := decodeLocalName(name)
	if err != nil {
		return Advertisement{}, err
	}
	return ParseAdvertisement(raw, networkName)
}

func matchingServiceData(data map[string][]byte, serviceUUID, networkName string) (Advertisement, bool) {
	for key, raw := range data {
		if strings.EqualFold(key, serviceUUID) {
			a, err := ParseAdvertisement(raw, networkName)
			return a, err == nil
		}
	}
	return Advertisement{}, false
}

// The prefix is a discovery hint only; complete identity is checked by TLS.
func deviceHint(device uuid.UUID) [4]byte { return [4]byte(device[:4]) }
