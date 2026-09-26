package discovery

import (
	"strconv"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// WendyLiteServiceType is the mDNS service type advertised by Wendy Lite
// (ESP32) boards. Distinct from wendyServiceType ("_wendyos._udp"): a Wendy
// Lite board is never a WendyOS agent, but once cloud-enrolled it advertises
// the same assetid/orgid/caps TXT keys on this service, plus its own mtls
// key (see LANDeviceFromWendyLiteService).
const WendyLiteServiceType = "_wendy-lite._tcp"

// LANDeviceFromWendyLiteService converts a resolved _wendy-lite._tcp mDNS
// sighting into a models.LANDevice, for callers outside this package (the
// sensor-pairing picker, the agent's LAN resolve) that already consume
// lanDeviceFromService's _wendyos._udp devices in the same shape. Identity
// precedence mirrors providers.MicroWendyProvider.mdnsExternalDevice: TXT
// "id" then "name" then hostname for the id; TXT "displayname" then the
// DNS-SD instance name then hostname for display.
//
// IsMTLS reads the "mtls" TXT key, not "tls": Wendy Lite is always
// TLS-encrypted, and mtls=false only means the server doesn't validate the
// client's certificate (the factory-default, unenrolled state) rather than
// no encryption at all, unlike _wendyos._udp's tls key.
func LANDeviceFromWendyLiteService(svc MDNSService) models.LANDevice {
	id := svc.TXTRecords["id"]
	if id == "" {
		id = svc.TXTRecords["name"]
	}
	if id == "" {
		id = svc.Hostname
	}

	displayName := svc.TXTRecords["displayname"]
	if displayName == "" {
		displayName = svc.InstanceName
	}
	if displayName == "" {
		displayName = svc.Hostname
	}

	dev := models.LANDevice{
		ID:               id,
		DisplayName:      displayName,
		Hostname:         svc.Hostname,
		IPAddress:        svc.IPAddress,
		Port:             svc.Port,
		IsMTLS:           svc.TXTRecords["mtls"] == "true",
		InterfaceType:    string(models.InterfaceLAN),
		IsWendyDevice:    true,
		NetworkInterface: svc.InterfaceName,
		WendyLite:        true,
	}
	if dev.IPAddress != "" {
		dev.Addresses = []string{dev.IPAddress}
	}
	if v, ok := svc.TXTRecords["assetid"]; ok {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil && n > 0 {
			dev.AssetID = int32(n)
		}
	}
	if v, ok := svc.TXTRecords["orgid"]; ok {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil && n > 0 {
			dev.OrgID = int32(n)
		}
	}
	if v, ok := svc.TXTRecords["caps"]; ok {
		for _, c := range strings.Split(v, ",") {
			if c = strings.TrimSpace(c); c != "" {
				dev.Caps = append(dev.Caps, c)
			}
		}
	}
	dev.Sensorlink = contains(dev.Caps, "sensors")
	return dev
}
