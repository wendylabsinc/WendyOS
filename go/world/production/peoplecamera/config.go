package peoplecamera

import (
	"github.com/wendylabsinc/wendy/go/world/production/liveapp"
	"github.com/wendylabsinc/wendy/go/world/production/livecapture"
)

func cameraID(id string) bool {
	switch id {
	case "camera-a", "camera-b", "people-front", "people-oblique":
		return true
	default:
		return false
	}
}

// LoadConfig uses the bounded capture loader and requires a camera source.
func LoadConfig(path string) (liveapp.Config, error) {
	c, err := livecapture.LoadConfig(path)
	if err != nil || !cameraID(c.SensorID) {
		return c, liveapp.ErrConfig
	}
	return c, nil
}
