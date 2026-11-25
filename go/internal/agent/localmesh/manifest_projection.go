package localmesh

import (
	"errors"
	"regexp"

	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// Optional entries retain the enclosing V4 artifact's byte/count limits even
// when this implementation cannot project their discriminator locally.
const maxManifestAppEndpointEntries = 16

var extensibleServiceType = regexp.MustCompile(`^_[a-z0-9-]{1,15}\._[a-z0-9-]{1,15}$`)

func manifestEntryText(fields map[string]netwire.RawMessage, key string) (string, error) {
	var value string
	raw, ok := fields[netwire.Key(key)]
	if !ok || netwire.Unmarshal(raw, &value) != nil {
		return "", errors.New("missing or invalid manifest entry discriminator")
	}
	return value, nil
}

func projectAppEndpoints(raw netwire.RawMessage) ([]AppEndpoint, error) {
	out := []AppEndpoint{}
	if len(raw) == 0 {
		return out, nil
	}
	var entries []map[string]netwire.RawMessage
	if err := netwire.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}
	if len(entries) > maxManifestAppEndpointEntries {
		return nil, errors.New("too many app session endpoint entries")
	}
	for _, fields := range entries {
		transport, err := manifestEntryText(fields, "transport")
		if err != nil || len(transport) == 0 || len(transport) > 64 {
			return nil, errors.New("invalid app session endpoint transport")
		}
		if transport != AppTLS && transport != AppQUIC {
			continue
		}
		if err := manifestFieldTypes(fields, map[string]byte{"port": 0, "transport": 3}, "port", "transport"); err != nil {
			return nil, err
		}
		encoded, err := netwire.Marshal(fields)
		if err != nil {
			return nil, err
		}
		var endpoint AppEndpoint
		if err := netwire.Unmarshal(encoded, &endpoint); err != nil {
			return nil, err
		}
		out = append(out, endpoint)
	}
	if err := validateAppEndpoints(out); err != nil {
		return nil, err
	}
	return out, nil
}

func projectManifestServices(raw netwire.RawMessage) ([]ManifestService, error) {
	out := []ManifestService{}
	if len(raw) == 0 {
		return out, nil
	}
	var entries []map[string]netwire.RawMessage
	if err := netwire.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}
	if len(entries) > MaxManifestServices {
		return nil, errors.New("too many services for one manifest")
	}
	seen := make(map[[3]string]bool, len(entries))
	for _, fields := range entries {
		mesh, err := manifestEntryText(fields, "mesh")
		if err != nil {
			return nil, err
		}
		app, err := manifestEntryText(fields, "appId")
		if err != nil {
			return nil, err
		}
		service, err := manifestEntryText(fields, "serviceId")
		if err != nil {
			return nil, err
		}
		if !serviceLabel.MatchString(mesh) || appconfig.ValidateAppID(app) != nil || !serviceLabel.MatchString(service) {
			return nil, errors.New("invalid manifest service identity")
		}
		key := [3]string{mesh, app, service}
		if seen[key] {
			return nil, errors.New("duplicate manifest service")
		}
		seen[key] = true
		if app == "wendy.mesh.system" && service != "internet-gateway" {
			continue
		}
		if app != "wendy.mesh.system" && service != "internet-gateway" {
			kind, err := manifestEntryText(fields, "type")
			if err != nil || !extensibleServiceType.MatchString(kind) {
				return nil, errors.New("invalid manifest service type")
			}
			if !serviceType.MatchString(kind) {
				continue
			}
		}
		encoded, err := netwire.Marshal(fields)
		if err != nil {
			return nil, err
		}
		var entry ManifestService
		if err := netwire.Unmarshal(encoded, &entry); err != nil {
			return nil, err
		}
		if err := entry.Validate(); err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, nil
}

// Both projection decoders already checked these arrays and their limits.
func manifestHasEntries(raw netwire.RawMessage) bool {
	var entries []netwire.RawMessage
	_ = netwire.Unmarshal(raw, &entries)
	return len(entries) != 0
}
