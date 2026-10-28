package localmesh

import (
	"errors"
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
)

// ErrUnsupportedManifest describes an intact artifact whose interpretation is
// unavailable. It is not a carrier failure and must never advance replay state.
var ErrUnsupportedManifest = errors.New("unsupported wendy net manifest")

type UnsupportedManifestError struct {
	Version     int
	Requirement string
	Transport   string
}

func (e *UnsupportedManifestError) Error() string {
	if e.Requirement != "" {
		return fmt.Sprintf("%v version %d: required extension %q", ErrUnsupportedManifest, e.Version, e.Requirement)
	}
	if e.Transport != "" {
		return fmt.Sprintf("%v version %d: endpoint transport %q", ErrUnsupportedManifest, e.Version, e.Transport)
	}
	return fmt.Sprintf("%v version %d", ErrUnsupportedManifest, e.Version)
}
func (*UnsupportedManifestError) Unwrap() error { return ErrUnsupportedManifest }

// DecodeManifest returns only the understood view of a version-4 signed body.
// Unknown optional fields, including nested metadata, remain in the immutable
// SignedManifest.Body used for signatures, hashes, receipts and forwarding.
// Lease, identity and route ownership are checked by the admitting directory.
func DecodeManifest(body []byte) (Manifest, error) {
	var m Manifest
	var fields map[string]netwire.RawMessage
	if err := netwire.UnmarshalExtensibleArtifact(body, &fields, MaxManifestBytes); err != nil {
		return m, err
	}
	if err := manifestFieldTypes(fields, map[string]byte{"version": 0}, "version"); err != nil {
		return m, err
	}
	if err := netwire.Unmarshal(fields[netwire.Key("version")], &m.Version); err != nil {
		return m, err
	}
	if m.Version != ManifestVersionRoute {
		return m, &UnsupportedManifestError{Version: m.Version}
	}
	types := map[string]byte{
		"version": 0, "device": 3, "revision": 0, "Issued": 0, "Expires": 0,
		"name": 3, "agentPort": 0, "agentTransport": 3, "platform": 3,
		"owner": 5, "caps": 4, "requires": 4, "withdraw": 7, "route": 5, "appEndpoints": 4, "s": 4,
	}
	if err := manifestFieldTypes(fields, types, "version", "device", "revision", "Issued", "Expires", "agentPort", "route"); err != nil {
		return m, err
	}
	var route map[string]netwire.RawMessage
	if err := netwire.Unmarshal(fields[netwire.Key("route")], &route); err != nil {
		return m, err
	}
	if err := manifestFieldTypes(route, map[string]byte{"prefix": 3, "routerId": 0, "seqno": 0}, "prefix", "routerId", "seqno"); err != nil {
		return m, err
	}
	if raw, ok := fields[netwire.Key("appEndpoints")]; ok {
		var endpoints []map[string]netwire.RawMessage
		if err := netwire.Unmarshal(raw, &endpoints); err != nil {
			return m, err
		}
		for _, endpoint := range endpoints {
			if err := manifestFieldTypes(endpoint, map[string]byte{"port": 0, "transport": 3}, "port", "transport"); err != nil {
				return m, err
			}
		}
	}
	if err := netwire.UnmarshalExtensibleArtifact(body, &m, MaxManifestBytes); err != nil {
		return m, err
	}
	if err := validateNativeAgentMetadata(m.Platform, m.Caps); err != nil {
		return m, err
	}
	if err := validateServices(m.Services); err != nil {
		return m, err
	}
	if err := validateManifestRequirements(m.Requires); err != nil {
		return m, err
	}
	if m.AgentTransport != "" && m.AgentTransport != AgentMTLS {
		return m, &UnsupportedManifestError{Version: m.Version, Transport: m.AgentTransport}
	}
	if err := validateAppEndpoints(m.AppEndpoints); err != nil {
		return m, err
	}
	if m.AppEndpoints == nil {
		// No advertised app transport means unknown/unavailable, never a
		// protocol downgrade to the pre-v4 implicit QUIC interpretation.
		m.AppEndpoints = []AppEndpoint{}
	}
	return m, nil
}

func manifestFieldTypes(fields map[string]netwire.RawMessage, known map[string]byte, required ...string) error {
	for _, name := range required {
		if _, ok := fields[netwire.Key(name)]; !ok {
			return fmt.Errorf("missing manifest field %q", name)
		}
	}
	for name, major := range known {
		if raw, ok := fields[netwire.Key(name)]; ok {
			if len(raw) == 0 || raw[0]>>5 != major || (major == 7 && raw[0] != 0xf4 && raw[0] != 0xf5) {
				return fmt.Errorf("invalid manifest field type %q", name)
			}
		}
	}
	return nil
}

func validateManifestNames(names []string) error {
	if len(names) > 16 {
		return errors.New("manifest name array exceeds bound")
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if len(name) == 0 || len(name) > 64 || seen[name] {
			return errors.New("invalid or duplicate manifest name")
		}
		for i, c := range []byte(name) {
			if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
				continue
			}
			if i > 0 && (c == '.' || c == '_' || c == '/' || c == '-') {
				continue
			}
			return errors.New("manifest name is not normalized")
		}
		seen[name] = true
	}
	return nil
}

func validateManifestRequirements(requires []string) error {
	if err := validateManifestNames(requires); err != nil {
		return err
	}
	// V4 understands the requires mechanism but defines no named extensions.
	if len(requires) != 0 {
		return &UnsupportedManifestError{Version: ManifestVersionRoute, Requirement: requires[0]}
	}
	return nil
}
