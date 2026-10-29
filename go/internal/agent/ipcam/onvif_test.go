package ipcam

import (
	"encoding/xml"
	"errors"
	"strings"
	"testing"
)

// A ProbeMatches reply shaped like a Reolink RLC-520A's, which is the camera
// this transport was built against.
const reolinkProbeMatch = `<?xml version="1.0" encoding="UTF-8"?>
<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://www.w3.org/2003/05/soap-envelope"
 xmlns:wsa="http://schemas.xmlsoap.org/ws/2004/08/addressing"
 xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery">
 <SOAP-ENV:Header>
  <wsa:MessageID>uuid:2419d68a-2dd2-21b2-a205-ec71db2aae7e</wsa:MessageID>
  <wsa:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/ProbeMatches</wsa:Action>
 </SOAP-ENV:Header>
 <SOAP-ENV:Body>
  <d:ProbeMatches>
   <d:ProbeMatch>
    <wsa:EndpointReference>
     <wsa:Address>urn:uuid:2419d68a-2dd2-21b2-a205-ec71db2aae7e</wsa:Address>
    </wsa:EndpointReference>
    <d:Types>dn:NetworkVideoTransmitter tds:Device</d:Types>
    <d:Scopes>onvif://www.onvif.org/type/video_encoder onvif://www.onvif.org/name/RLC-520A onvif://www.onvif.org/hardware/IPC_MS4NA45MPS44E1W011000000010</d:Scopes>
    <d:XAddrs>http://10.98.0.50/onvif/device_service</d:XAddrs>
    <d:MetadataVersion>1</d:MetadataVersion>
   </d:ProbeMatch>
  </d:ProbeMatches>
 </SOAP-ENV:Body>
</SOAP-ENV:Envelope>`

func TestParseProbeMatchReolink(t *testing.T) {
	got, err := ParseProbeMatch([]byte(reolinkProbeMatch))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got.XAddrs) != 1 || got.XAddrs[0] != "http://10.98.0.50/onvif/device_service" {
		t.Fatalf("XAddrs = %v", got.XAddrs)
	}
	if got.Model != "RLC-520A" {
		t.Fatalf("Model = %q, want RLC-520A", got.Model)
	}
}

// Cameras that advertise no name scope should still parse, falling back to the
// hardware scope so the listing is not blank.
func TestParseProbeMatchHardwareFallback(t *testing.T) {
	payload := strings.Replace(reolinkProbeMatch,
		"onvif://www.onvif.org/name/RLC-520A ", "", 1)
	got, err := ParseProbeMatch([]byte(payload))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Model != "IPC_MS4NA45MPS44E1W011000000010" {
		t.Fatalf("Model = %q, want the hardware scope", got.Model)
	}
}

// Multiple XAddrs occur when a camera is multi-homed; all are kept, in order.
func TestParseProbeMatchMultipleXAddrs(t *testing.T) {
	payload := strings.Replace(reolinkProbeMatch,
		"<d:XAddrs>http://10.98.0.50/onvif/device_service</d:XAddrs>",
		"<d:XAddrs>http://10.98.0.50/onvif/device_service http://192.168.0.9/onvif/device_service</d:XAddrs>", 1)
	got, err := ParseProbeMatch([]byte(payload))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got.XAddrs) != 2 {
		t.Fatalf("XAddrs = %v, want 2", got.XAddrs)
	}
}

// Port 3702 carries traffic from plenty of things that are not cameras.
func TestParseProbeMatchRejectsNonMatch(t *testing.T) {
	cases := map[string]string{
		"no probe match": `<Envelope><Body></Body></Envelope>`,
		"not xml":        "not xml at all",
		"empty":          "",
		"no xaddrs": strings.Replace(reolinkProbeMatch,
			"<d:XAddrs>http://10.98.0.50/onvif/device_service</d:XAddrs>", "", 1),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseProbeMatch([]byte(payload)); err == nil {
				t.Fatalf("expected an error for %s", name)
			}
		})
	}
}

// The probe must be well-formed and ask for NetworkVideoTransmitter, or cameras
// will not answer it.
func TestBuildProbe(t *testing.T) {
	probe := string(BuildProbe("uuid:test-message-id"))
	for _, want := range []string{
		"uuid:test-message-id",
		"http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe",
		"NetworkVideoTransmitter",
		"urn:schemas-xmlsoap-org:ws:2005:04:discovery",
	} {
		if !strings.Contains(probe, want) {
			t.Fatalf("probe missing %q:\n%s", want, probe)
		}
	}
}

// A probe we build must be parseable as XML, which catches interpolation that
// breaks the envelope.
func TestBuildProbeIsWellFormed(t *testing.T) {
	if _, err := ParseProbeMatch(BuildProbe("uuid:x")); err == nil {
		t.Fatal("a Probe is not a ProbeMatch and must not parse as one")
	}
}

func TestParseProbeMatchTypes(t *testing.T) {
	for _, tc := range []struct {
		types string
		want  bool
	}{
		{"dn:NetworkVideoTransmitter tds:Device", true},
		{"camera:NetworkVideoTransmitter", true},
		{"NetworkVideoTransmitter", true},
		{"tds:Device", false},
		{"wsdp:Computer", false},
		{"wprt:PrintDeviceType", false},
		{"dn:NetworkVideoTransmitterExtra", false},
		{"dn:NotNetworkVideoTransmitter", false},
		{"", true},
		{" \t\n ", true},
	} {
		t.Run(tc.types, func(t *testing.T) {
			payload := strings.Replace(reolinkProbeMatch, "dn:NetworkVideoTransmitter tds:Device", tc.types, 1)
			_, err := ParseProbeMatch([]byte(payload))
			if tc.want && err != nil {
				t.Fatal(err)
			}
			if !tc.want && !errors.Is(err, ErrProbeMatchTypeMismatch) {
				t.Fatalf("error = %v, want ErrProbeMatchTypeMismatch", err)
			}
		})
	}
	payload := strings.Replace(reolinkProbeMatch, "<d:Types>dn:NetworkVideoTransmitter tds:Device</d:Types>", "", 1)
	got, err := ParseProbeMatch([]byte(payload))
	if err != nil || len(got.XAddrs) != 1 || got.Model != "RLC-520A" {
		t.Fatalf("missing Types: match = %+v, error = %v", got, err)
	}
}

func TestParseProbeMatchSkipsNonCameraMatch(t *testing.T) {
	printer := `<d:ProbeMatch><d:Types>wprt:PrintDeviceType</d:Types><d:XAddrs>http://printer/</d:XAddrs></d:ProbeMatch>`
	payload := strings.Replace(reolinkProbeMatch, "<d:ProbeMatches>", "<d:ProbeMatches>"+printer, 1)
	got, err := ParseProbeMatch([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "RLC-520A" {
		t.Fatalf("matched wrong device: %+v", got)
	}
}

func TestParseProbeMatchErrorKinds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		want    error
	}{
		{"no matches", "<Envelope/>", ErrNoProbeMatch},
		{"no address", "<Envelope><Body><ProbeMatches><ProbeMatch/></ProbeMatches></Body></Envelope>", ErrNoProbeMatch},
		{"wrong type", strings.Replace(reolinkProbeMatch, "dn:NetworkVideoTransmitter tds:Device", "tds:Device", 1), ErrProbeMatchTypeMismatch},
		{"compatible type without address", strings.Replace(reolinkProbeMatch,
			"<d:XAddrs>http://10.98.0.50/onvif/device_service</d:XAddrs>", "", 1), ErrNoProbeMatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseProbeMatch([]byte(tc.payload)); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
	_, err := ParseProbeMatch([]byte("<Envelope><broken></Envelope>"))
	var syntaxErr *xml.SyntaxError
	if !errors.As(err, &syntaxErr) || errors.Is(err, ErrNoProbeMatch) || errors.Is(err, ErrProbeMatchTypeMismatch) {
		t.Fatalf("malformed XML error = %v, want a separate XML syntax error", err)
	}
}
