package mesh

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
)

// Legacy Cloud asset IDs were encoded directly in 10.99.0.0/16. These
// addresses have meaning only under a numeric enrollment, never in VIPTable.
func LegacyVIPForDevice(assetID int32) (netip.Addr, error) {
	if assetID < 1 || assetID > 65534 {
		return netip.Addr{}, fmt.Errorf("legacy asset ID %d is outside [1,65534]", assetID)
	}
	return netip.AddrFrom4([4]byte{10, 99, byte(assetID >> 8), byte(assetID)}), nil
}
func LegacyDeviceForVIP(vip netip.Addr) (int32, error) {
	if !vip.Is4() || !meshPrefix.Contains(vip) {
		return 0, fmt.Errorf("%s is outside the legacy service CIDR", vip)
	}
	octets := vip.As4()
	asset := int32(octets[2])<<8 | int32(octets[3])
	if _, err := LegacyVIPForDevice(asset); err != nil {
		return 0, err
	}
	return asset, nil
}

// LegacyResolver preserves the old LAN-first numeric name directory. The
// organization is read per request so setup/PKI enrollment never enables it.
type LegacyResolver interface {
	LegacyOrgID() int32
	ResolveLegacy(name, orgSlug string) (int32, bool)
}
type LegacyAppDialer interface {
	LegacyOrgID() int32
	DialLegacyApp(context.Context, int32, int32, uint16) (net.Conn, string, error)
}
type legacyConnMetrics interface{ RecordAssetBytes(int32, string, int64) }

var legacyMeshNameRE = regexp.MustCompile(`^device-([0-9]+)\.` + meshDNSSuffixRE + `\.$`)

func (s *DNSServer) SetLegacyResolver(resolver LegacyResolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.legacy = resolver
}

func legacyAssetID(text string) (int32, error) {
	asset, err := strconv.ParseInt(text, 10, 32)
	if err != nil {
		return 0, err
	}
	if _, err := LegacyVIPForDevice(int32(asset)); err != nil {
		return 0, err
	}
	return int32(asset), nil
}
