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
	AdvertisementSize         = 9
	LocalNameSize             = 10
	AdvertisementVersion byte = 0
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

// Advertisement holds the small, non-secret peer hint carried as ServiceData.
// Binary v0 is version/reserved (1), PSM (2, BE), network hash (2), UUID prefix
// (4). With a 128-bit ServiceData UUID and flags it occupies 30 bytes.
// Neither this hint nor its local-name representation grants authority.
type Advertisement struct {
	Version     byte
	DeviceHint  [4]byte
	NetworkHash [2]byte
	PSM         uint16
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
	return Advertisement{DeviceHint: deviceHint(device), NetworkHash: NetworkNameHash(networkName), PSM: psm}, nil
}

func (a Advertisement) MarshalBinary() ([]byte, error) {
	if a.Version != AdvertisementVersion || !ValidPSM(a.PSM) {
		return nil, ErrInvalidAdvertisement
	}
	b := make([]byte, AdvertisementSize)
	b[0] = a.Version
	binary.BigEndian.PutUint16(b[1:3], a.PSM)
	copy(b[3:5], a.NetworkHash[:])
	copy(b[5:9], a.DeviceHint[:])
	return b, nil
}

func ParseAdvertisement(data []byte, networkName string) (Advertisement, error) {
	if len(data) != AdvertisementSize || networkName == "" {
		return Advertisement{}, ErrInvalidAdvertisement
	}
	a := Advertisement{Version: data[0], PSM: binary.BigEndian.Uint16(data[1:3])}
	copy(a.NetworkHash[:], data[3:5])
	copy(a.DeviceHint[:], data[5:9])
	if a.Version != AdvertisementVersion || !ValidPSM(a.PSM) || a.NetworkHash != NetworkNameHash(networkName) {
		return Advertisement{}, ErrInvalidAdvertisement
	}
	return a, nil
}

// MarshalLocalName encodes the logical fields as one 65-bit unsigned integer:
// version (1), PSM (16), network hash (16), UUID prefix (32), most significant
// first. Ten base-94 digits use only ASCII '!'..'~'; leading '!' are required.
// This representation is for platforms unable to advertise ServiceData.
func (a Advertisement) MarshalLocalName() (string, error) {
	raw, err := a.MarshalBinary()
	if err != nil {
		return "", err
	}
	return encodeLocalName(raw)
}

func encodeLocalName(raw []byte) (string, error) {
	if len(raw) != AdvertisementSize || raw[0] > 1 {
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
	if value.BitLen() > 65 {
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
