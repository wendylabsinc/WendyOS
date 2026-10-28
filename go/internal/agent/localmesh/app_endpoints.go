package localmesh

import "errors"

const AppQUIC = "quic-v1"
const AppTLS = "tls-v1"

// AgentMTLS declares that AgentPort is independently authenticated end to end.
const AgentMTLS = "mtls-v1"
const PlatformLite = "Lite"
const PlatformWendyOS = "WendyOS"
const CapabilitySensors = "sensors"
const AppSessionTLSPort = 43022

func validateNativeAgentMetadata(platform string, caps []string) error {
	if len(platform) > 32 {
		return errors.New("native agent metadata exceeds bounds")
	}
	return validateManifestNames(caps)
}

// AppEndpoint is a signed device capability, independent of its hop carrier.
// A TLS-only device can be reached through QUIC, BLE or TCP carrier hops.
type AppEndpoint struct {
	Transport string `json:"transport" cbor:"t"`
	Port      uint16 `json:"port" cbor:"p"`
}

func validateAppEndpoints(endpoints []AppEndpoint) error {
	if len(endpoints) > 2 {
		return errors.New("too many app session endpoints")
	}
	seen := map[string]bool{}
	for _, endpoint := range endpoints {
		if endpoint.Transport != AppQUIC && endpoint.Transport != AppTLS {
			return &UnsupportedManifestError{Version: ManifestVersionRoute, Transport: endpoint.Transport}
		}
		if endpoint.Port == 0 || seen[endpoint.Transport] {
			return errors.New("invalid or duplicate app session endpoint")
		}
		seen[endpoint.Transport] = true
	}
	return nil
}
func cloneManifest(m Manifest) Manifest {
	m.Services = cloneServices(m.Services)
	if m.Caps != nil {
		m.Caps = append([]string{}, m.Caps...)
	}
	if m.Requires != nil {
		m.Requires = append([]string{}, m.Requires...)
	}
	if m.Route != nil {
		route := *m.Route
		m.Route = &route
	}
	if m.AppEndpoints != nil {
		endpoints := make([]AppEndpoint, len(m.AppEndpoints))
		copy(endpoints, m.AppEndpoints)
		m.AppEndpoints = endpoints
	}
	return m
}
