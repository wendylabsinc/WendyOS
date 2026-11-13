package localmesh

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

const MaxManifestServices = 256

// ManifestService inherits its device, revision and lease from its manifest.
// App/service identity is stable across port, TXT and display-name changes.
type ManifestService struct {
	Mesh      string   `json:"mesh" cbor:"m"`
	AppID     string   `json:"appId" cbor:"a"`
	ServiceID string   `json:"serviceId" cbor:"s"`
	Type      string   `json:"type" cbor:"t"`
	Instance  string   `json:"instance" cbor:"n"`
	Port      uint16   `json:"port" cbor:"p"`
	TXT       []string `json:"txt,omitempty" cbor:"x,omitempty"`
}

var serviceLabel = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
var serviceType = regexp.MustCompile(`^_[a-z0-9-]{1,15}\._(tcp|udp)$`)

func (s ManifestService) Validate() error {
	if !serviceLabel.MatchString(s.Mesh) || appconfig.ValidateAppID(s.AppID) != nil || !serviceLabel.MatchString(s.ServiceID) {
		return errors.New("invalid manifest service identity")
	}
	// This capability has no socket. It inherits the same manifest lease.
	if s.AppID == "wendy.mesh.system" || s.ServiceID == "internet-gateway" {
		if s.AppID == "wendy.mesh.system" && s.ServiceID == "internet-gateway" && s.Type == "" && s.Instance == "" && s.Port == 0 && len(s.TXT) == 0 {
			return nil
		}
		return errors.New("invalid manifest gateway capability")
	}
	if !serviceType.MatchString(s.Type) || s.Port == 0 || len(s.Instance) == 0 || len(s.Instance) > 63 || !utf8.ValidString(s.Instance) || strings.ContainsAny(s.Instance, "\x00\r\n") || len(s.TXT) > 32 {
		return errors.New("invalid manifest service endpoint")
	}
	total := 0
	for _, txt := range s.TXT {
		if len(txt) == 0 || len(txt) > 255 || strings.ContainsAny(txt, "\x00\r\n") {
			return errors.New("invalid manifest service TXT")
		}
		total += len(txt) + 1
	}
	if total > 1200 {
		return errors.New("manifest service TXT exceeds bound")
	}
	return nil
}

func cloneServices(services []ManifestService) []ManifestService {
	out := slices.Clone(services)
	for i := range out {
		out[i].TXT = slices.Clone(out[i].TXT)
	}
	return out
}

func validateServices(services []ManifestService) error {
	if len(services) > MaxManifestServices {
		return errors.New("too many services for one manifest")
	}
	seen := make(map[[3]string]bool, len(services))
	for _, service := range services {
		if err := service.Validate(); err != nil {
			return err
		}
		key := [3]string{service.Mesh, service.AppID, service.ServiceID}
		if seen[key] {
			return errors.New("duplicate manifest service")
		}
		seen[key] = true
	}
	return nil
}

// CheckManifestServices checks the entire replacement before accepting intent.
// Reserve room for route/identity metadata; never truncate an authoritative list.
func CheckManifestServices(services []ManifestService) error {
	if err := validateServices(services); err != nil {
		return err
	}
	encoded, err := netwire.Marshal(services)
	if err != nil {
		return err
	}
	if len(encoded) > MaxManifestBytes-2048 {
		return errors.New("services do not fit in one manifest")
	}
	return nil
}
