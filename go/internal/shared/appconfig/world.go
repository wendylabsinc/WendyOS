package appconfig

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const EntitlementWorld = "world"

// WorldPublication names one exact component/stream pair. Wildcards and task
// authority are deliberately absent from the first publication entitlement.
type WorldPublication struct {
	ComponentType string `json:"componentType"`
	Stream        string `json:"stream"`
}

// WorldSubscription selects one exact source. An omitted device means the local
// agent; an explicit principal still requires separately commissioned remote
// authority. Tenant and site remain agent-owned.
type WorldSubscription struct {
	App           string `json:"app"`
	Service       string `json:"service"`
	ComponentType string `json:"componentType"`
	Stream        string `json:"stream"`
	Device        string `json:"device,omitempty"`
}

type worldAccessAnnotation struct {
	Version   int                 `json:"version"`
	Publish   []WorldPublication  `json:"publish,omitempty"`
	Subscribe []WorldSubscription `json:"subscribe,omitempty"`
}

const maxWorldAnnotationBytes = 4096 - len(EntitlementAnnotationKeyPrefix+EntitlementWorld)

func ValidateWorldPublications(scopes []WorldPublication) error {
	if len(scopes) == 0 || len(scopes) > 8 {
		return fmt.Errorf("world publish requires 1..8 exact component/stream pairs")
	}
	seen := map[WorldPublication]bool{}
	for _, scope := range scopes {
		for _, value := range []string{scope.ComponentType, scope.Stream} {
			if value == "" || len(value) > 128 || !utf8.ValidString(value) || strings.ContainsAny(value, "*?") {
				return fmt.Errorf("invalid world publication scope")
			}
			for _, r := range value {
				if r < 0x20 || r == 0x7f {
					return fmt.Errorf("invalid world publication scope")
				}
			}
		}
		if seen[scope] {
			return fmt.Errorf("duplicate world publication scope")
		}
		seen[scope] = true
	}
	return nil
}

func ValidateWorldSubscriptions(scopes []WorldSubscription) error {
	if len(scopes) == 0 || len(scopes) > 32 {
		return fmt.Errorf("world subscribe requires 1..32 exact source selectors")
	}
	seen := map[WorldSubscription]bool{}
	for _, scope := range scopes {
		if scope.Device != "" {
			if device, err := meshid.DeviceFromPrincipal(scope.Device); err != nil || device.Principal() != scope.Device {
				return fmt.Errorf("invalid world subscription device")
			}
		}
		if ValidateAppID(scope.App) != nil || ValidateServiceName(scope.Service) != nil || scope.Service == "" || ValidateWorldPublications([]WorldPublication{{ComponentType: scope.ComponentType, Stream: scope.Stream}}) != nil {
			return fmt.Errorf("invalid world subscription scope")
		}
		if seen[scope] {
			return fmt.Errorf("duplicate world subscription scope")
		}
		seen[scope] = true
	}
	return nil
}

func encodeWorldAccess(publish []WorldPublication, subscribe []WorldSubscription) string {
	data, _ := json.Marshal(worldAccessAnnotation{Version: 2, Publish: publish, Subscribe: subscribe})
	return "access=" + base64.RawURLEncoding.EncodeToString(data)
}

func ValidateWorldAccess(publish []WorldPublication, subscribe []WorldSubscription) error {
	if len(publish) == 0 && len(subscribe) == 0 {
		return fmt.Errorf("world requires at least one publication or subscription")
	}
	if len(publish) > 0 {
		if err := ValidateWorldPublications(publish); err != nil {
			return err
		}
	}
	if len(subscribe) > 0 {
		if err := ValidateWorldSubscriptions(subscribe); err != nil {
			return err
		}
	}
	if len(encodeWorldAccess(publish, subscribe)) > maxWorldAnnotationBytes {
		return fmt.Errorf("world scopes exceed containerd's 4096-byte label budget")
	}
	return nil
}

func WorldPublications(entitlements []Entitlement) ([]WorldPublication, bool, error) {
	publish, _, found, err := WorldAccess(entitlements)
	return publish, found, err
}

func WorldAccess(entitlements []Entitlement) ([]WorldPublication, []WorldSubscription, bool, error) {
	var publish []WorldPublication
	var subscribe []WorldSubscription
	found := false
	for _, entitlement := range entitlements {
		if entitlement.Type != EntitlementWorld {
			continue
		}
		if found {
			return nil, nil, true, fmt.Errorf("only one world entitlement is allowed per service")
		}
		found = true
		if err := ValidateWorldAccess(entitlement.WorldPublish, entitlement.WorldSubscribe); err != nil {
			return nil, nil, true, err
		}
		publish = append([]WorldPublication(nil), entitlement.WorldPublish...)
		subscribe = append([]WorldSubscription(nil), entitlement.WorldSubscribe...)
	}
	return publish, subscribe, found, nil
}

func worldAnnotation(publish []WorldPublication, subscribe []WorldSubscription) string {
	if ValidateWorldAccess(publish, subscribe) != nil {
		return "invalid"
	}
	return encodeWorldAccess(publish, subscribe)
}

func parseWorldAnnotation(value string) Entitlement {
	invalid := Entitlement{Type: EntitlementWorld}
	if len(value) > maxWorldAnnotationBytes {
		return invalid
	}
	legacy := strings.HasPrefix(value, "publish=")
	prefix := "access="
	if legacy {
		prefix = "publish="
	}
	if !strings.HasPrefix(value, prefix) {
		return invalid
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil {
		return invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var access worldAccessAnnotation
	if legacy {
		if decoder.Decode(&access.Publish) != nil || ValidateWorldPublications(access.Publish) != nil {
			return invalid
		}
		canonical, _ := json.Marshal(access.Publish)
		if "publish="+base64.RawURLEncoding.EncodeToString(canonical) != value {
			return invalid
		}
	} else {
		if decoder.Decode(&access) != nil || access.Version != 2 || ValidateWorldAccess(access.Publish, access.Subscribe) != nil || encodeWorldAccess(access.Publish, access.Subscribe) != value {
			return invalid
		}
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return invalid
	}
	return Entitlement{Type: EntitlementWorld, WorldPublish: access.Publish, WorldSubscribe: access.Subscribe}
}

// WorldCgroupSuffix is the exact scope suffix passed to runc's systemd cgroup
// driver. The generation is a random deployment identity, not a source epoch.
func WorldCgroupSuffix(app, service string, generation uint64) (string, error) {
	if ValidateAppID(app) != nil || ValidateServiceName(service) != nil || service == "" || generation == 0 {
		return "", fmt.Errorf("world requires a valid explicit service and deployment generation")
	}
	return fmt.Sprintf("%s@%s.world-%016x", app, service, generation), nil
}
