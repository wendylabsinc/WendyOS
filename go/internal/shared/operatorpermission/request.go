package operatorpermission

import (
	"encoding/json"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RequestPermission identifies a container configuration that grants host-level
// administration. Deploy permission alone must not grant the agent admin socket
// or the privileged-equivalent build entitlement.
func RequestPermission(req any) (string, error) {
	configured, ok := req.(interface{ GetAppConfig() []byte })
	if !ok || len(configured.GetAppConfig()) == 0 {
		return "", nil
	}
	var cfg appconfig.AppConfig
	if err := json.Unmarshal(configured.GetAppConfig(), &cfg); err != nil {
		return "", status.Error(codes.InvalidArgument, "invalid app config")
	}
	privileged := func(es []appconfig.Entitlement) bool {
		for _, e := range es {
			if e.Type == appconfig.EntitlementAdmin || e.Type == appconfig.EntitlementBuild {
				return true
			}
		}
		return false
	}
	if (cfg.Hooks != nil && cfg.Hooks.PostStart != nil && cfg.Hooks.PostStart.Agent != "") || privileged(cfg.Entitlements) {
		return "device:shell", nil
	}
	for _, service := range cfg.Services {
		if service != nil && privileged(service.Entitlements) {
			return "device:shell", nil
		}
	}
	return "", nil
}
