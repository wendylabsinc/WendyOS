package gateway

import (
	"strings"

	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

func validateGo2TaskConfiguration(cfg config) error {
	if cfg.Tasks == nil {
		return nil
	}
	tc := cfg.Tasks
	_, enabled := tc.Catalog[tasking.Go2GoToType]
	if !enabled {
		if tc.Go2Binding != nil {
			return errGo2Binding
		}
		return nil
	}
	b := tc.Go2Binding
	if len(tc.Catalog) != 1 || tc.EnableSupervision || b == nil || b.Validate() != nil || cfg.ClockDomain != "simulation:"+b.RunID || !strings.HasPrefix(cfg.ClockDomain, "simulation:") || tc.MaxTelemetryAgeMS > 250 {
		return errGo2Binding
	}
	for _, f := range cfg.Frames {
		if f == b.Frame {
			return nil
		}
	}
	return errGo2Binding
}
