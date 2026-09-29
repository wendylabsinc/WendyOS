package commands

import (
	"fmt"
	"maps"
	"slices"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// Resolve only after the selected connection has a managed robot profile.
// Never guess from hostname or use these overrides on physical devices.
func applyRobotSimulationConfig(cfg *appconfig.AppConfig, kind string) (*appconfig.AppConfig, error) {
	result := *cfg
	if cfg.Services == nil {
		return &result, nil
	}
	result.Services = make(map[string]*appconfig.ServiceConfig, len(cfg.Services))
	for name, svc := range cfg.Services {
		if svc == nil {
			return nil, fmt.Errorf("services.%s is null", name)
		}
		copy := *svc
		if sim := svc.Simulation; sim != nil {
			if sim.Profile != kind {
				return nil, fmt.Errorf("services.%s supports the %s simulator, but the selected VM runs %s", name, sim.Profile, kind)
			}
			copy.Entitlements = slices.Clone(sim.Entitlements)
			copy.Env = maps.Clone(svc.Env)
			if copy.Env == nil {
				copy.Env = make(map[string]string)
			}
			maps.Copy(copy.Env, sim.Env)
			copy.Simulation = nil
		}
		result.Services[name] = &copy
	}
	if err := result.Validate(); err != nil {
		return nil, err
	}
	return &result, nil
}
