package twin

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
)

func validateSite(site Site) error {
	if !validID(site.ID) || !validID(site.ModelRevision) || site.Version != 1 || strings.TrimSpace(site.Name) == "" || len(site.Name) > 800 || site.MetresPerModelUnit <= 0 || site.MetresPerModelUnit > 1000 || math.IsNaN(site.MetresPerModelUnit) || len(site.Cameras) > 256 {
		return errors.New("invalid site")
	}
	ids, sources := map[string]bool{}, map[string]bool{}
	for _, raw := range site.Cameras {
		var camera struct {
			ID        string `json:"id"`
			GatewayID string `json:"gatewayID"`
			SourceID  string `json:"sourceID"`
			Name      string `json:"name"`
			Position  *Point `json:"position"`
		}
		if e := json.Unmarshal(raw, &camera); e != nil {
			return e
		}
		key := camera.GatewayID + "\x00" + camera.SourceID
		if !validID(camera.ID) || camera.GatewayID == "" || len(camera.GatewayID) > 1024 || camera.SourceID == "" || len(camera.SourceID) > 1024 || len(camera.Name) > 800 || ids[strings.ToLower(camera.ID)] || sources[key] {
			return errors.New("invalid camera")
		}
		ids[strings.ToLower(camera.ID)] = true
		sources[key] = true
		if camera.Position != nil {
			for _, v := range []float64{camera.Position.X, camera.Position.Y, camera.Position.Z} {
				if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 1e6 {
					return errors.New("invalid camera position")
				}
			}
		}
	}
	return nil
}
