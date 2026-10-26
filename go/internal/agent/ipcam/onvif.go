package ipcam

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
)

// ONVIF WS-Discovery constants. Cameras join this multicast group and answer a
// Probe for the NetworkVideoTransmitter type; it is the one discovery mechanism
// essentially every IP camera implements.
const (
	DiscoveryMulticastAddr = "239.255.255.250:3702"
	probeAction            = "http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe"
	probeTo                = "urn:schemas-xmlsoap-org:ws:2005:04:discovery"
	videoTransmitterType   = "dn:NetworkVideoTransmitter"
)

// Scope prefixes defined by ONVIF for device metadata.
const (
	scopeNamePrefix     = "onvif://www.onvif.org/name/"
	scopeHardwarePrefix = "onvif://www.onvif.org/hardware/"
)

// ProbeMatch is a camera's answer to a discovery Probe.
type ProbeMatch struct {
	XAddrs []string
	Scopes []string
	Model  string
	MAC    string
}

// BuildProbe returns the SOAP envelope for a WS-Discovery Probe. messageID must
// be unique per probe; responses echo it back.
func BuildProbe(messageID string) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>` +
		`<e:Envelope xmlns:e="http://www.w3.org/2003/05/soap-envelope"` +
		` xmlns:w="http://schemas.xmlsoap.org/ws/2004/08/addressing"` +
		` xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery"` +
		` xmlns:dn="http://www.onvif.org/ver10/network/wsdl">` +
		`<e:Header>` +
		`<w:MessageID>` + messageID + `</w:MessageID>` +
		`<w:To>` + probeTo + `</w:To>` +
		`<w:Action>` + probeAction + `</w:Action>` +
		`</e:Header>` +
		`<e:Body><d:Probe><d:Types>` + videoTransmitterType + `</d:Types></d:Probe></e:Body>` +
		`</e:Envelope>`)
}

// probeMatchEnvelope mirrors only the fields we consume. Namespace prefixes vary
// by vendor and firmware, so the decoder is left to match on local names.
type probeMatchEnvelope struct {
	XMLName xml.Name `xml:"Envelope"`
	Body    struct {
		ProbeMatches struct {
			ProbeMatch []struct {
				Types  string `xml:"Types"`
				XAddrs string `xml:"XAddrs"`
				Scopes string `xml:"Scopes"`
			} `xml:"ProbeMatch"`
		} `xml:"ProbeMatches"`
	} `xml:"Body"`
}

// ErrNoProbeMatch is returned when a payload parses as XML but carries no usable
// ProbeMatch, which happens for unrelated multicast traffic on port 3702.
var ErrNoProbeMatch = errors.New("payload contains no ProbeMatch")

// ParseProbeMatch extracts the first usable video-transmitter match from a
// discovery response. Other WS-Discovery devices (printers, computers, etc.)
// can answer on the same port and must not enter the camera registry.
func ParseProbeMatch(payload []byte) (ProbeMatch, error) {
	var env probeMatchEnvelope
	if err := xml.Unmarshal(payload, &env); err != nil {
		return ProbeMatch{}, fmt.Errorf("parsing probe response: %w", err)
	}
	for _, match := range env.Body.ProbeMatches.ProbeMatch {
		if !hasVideoTransmitterType(match.Types) {
			continue
		}
		out := ProbeMatch{
			XAddrs: strings.Fields(match.XAddrs),
			Scopes: strings.Fields(match.Scopes),
		}
		// A match with no service address tells us nothing we can act on.
		if len(out.XAddrs) == 0 {
			continue
		}
		out.Model = modelFromScopes(out.Scopes)
		return out, nil
	}
	return ProbeMatch{}, ErrNoProbeMatch
}

func hasVideoTransmitterType(types string) bool {
	for _, typ := range strings.Fields(types) {
		// Like element names above, QName prefixes vary by vendor. Match
		// the complete local name, never a substring or a fixed prefix.
		_, local, prefixed := strings.Cut(typ, ":")
		if !prefixed {
			local = typ
		}
		if local == "NetworkVideoTransmitter" {
			return true
		}
	}
	return false
}

// modelFromScopes prefers the ONVIF name scope, falling back to hardware so a
// camera that omits a friendly name still lists as something recognisable.
func modelFromScopes(scopes []string) string {
	hardware := ""
	for _, scope := range scopes {
		switch {
		case strings.HasPrefix(scope, scopeNamePrefix):
			if name := strings.TrimPrefix(scope, scopeNamePrefix); name != "" {
				return name
			}
		case strings.HasPrefix(scope, scopeHardwarePrefix):
			if hardware == "" {
				hardware = strings.TrimPrefix(scope, scopeHardwarePrefix)
			}
		}
	}
	return hardware
}
