package ipcam

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
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

// ErrProbeMatchTypeMismatch is returned when no ProbeMatch identifies itself as
// a video transmitter: typed matches lack the exact NetworkVideoTransmitter
// local name, and untyped ones carry no ONVIF scope.
var ErrProbeMatchTypeMismatch = errors.New("ProbeMatch types do not include NetworkVideoTransmitter")

// onvifScopePrefix starts every scope the ONVIF Core specification defines.
const onvifScopePrefix = "onvif://www.onvif.org/"

// ParseProbeMatch extracts the first usable video-transmitter match from a
// discovery response. Explicit non-camera types are rejected. Omitted or empty
// Types are accepted only alongside an ONVIF scope: WS-Discovery allows a target
// to omit types even when answering our NetworkVideoTransmitter-filtered probe,
// and every ONVIF device publishes those scopes, while a printer or computer
// that answers any probe without types does not.
func ParseProbeMatch(payload []byte) (ProbeMatch, error) {
	var env probeMatchEnvelope
	if err := xml.Unmarshal(payload, &env); err != nil {
		return ProbeMatch{}, fmt.Errorf("parsing probe response: %w", err)
	}
	matches := env.Body.ProbeMatches.ProbeMatch
	hasCompatibleType := false
	for _, match := range matches {
		scopes := strings.Fields(match.Scopes)
		if strings.TrimSpace(match.Types) == "" {
			if !hasONVIFScope(scopes) {
				continue
			}
		} else if !hasVideoTransmitterType(match.Types) {
			continue
		}
		hasCompatibleType = true
		out := ProbeMatch{
			XAddrs: strings.Fields(match.XAddrs),
			Scopes: scopes,
		}
		// A match with no service address tells us nothing we can act on.
		if len(out.XAddrs) == 0 {
			continue
		}
		out.Model = modelFromScopes(out.Scopes)
		return out, nil
	}
	if len(matches) > 0 && !hasCompatibleType {
		return ProbeMatch{}, ErrProbeMatchTypeMismatch
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

func hasONVIFScope(scopes []string) bool {
	for _, scope := range scopes {
		if strings.HasPrefix(scope, onvifScopePrefix) && len(scope) > len(onvifScopePrefix) {
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
				return decodeScopeValue(name)
			}
		case strings.HasPrefix(scope, scopeHardwarePrefix):
			if hardware == "" {
				hardware = decodeScopeValue(strings.TrimPrefix(scope, scopeHardwarePrefix))
			}
		}
	}
	return hardware
}

// decodeScopeValue undoes the percent-encoding a scope URI needs for spaces and
// other reserved characters, so "Tapo%20C200" lists as "Tapo C200". A value that
// is not valid percent-encoding, or that decodes to invalid UTF-8 or a control
// character, is returned as sent: `camera list` prints names verbatim, and a
// decoded escape sequence would reach the user's terminal.
func decodeScopeValue(raw string) string {
	decoded, err := url.PathUnescape(raw)
	if err != nil || !utf8.ValidString(decoded) {
		return raw
	}
	for _, r := range decoded {
		if unicode.IsControl(r) {
			return raw
		}
	}
	return decoded
}
