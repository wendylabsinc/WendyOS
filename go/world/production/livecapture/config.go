package livecapture

import (
	"io"
	"os"
	"path/filepath"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/liveapp"
	"golang.org/x/sys/unix"
)

// Config keeps the existing ordinary capture configuration shape. This loader
// additionally accepts lidar-* and the two people camera sources without
// changing liveapp's original whole-scene polling workflow.
type Config = liveapp.Config

func sensorKind(id string) string {
	switch id {
	case "camera-a", "camera-b", "people-front", "people-oblique":
		return "camera"
	case "gas-1":
		return "gas"
	case "audio-1":
		return "audio"
	default:
		if sensing.LidarSensorID(id) {
			return "lidar"
		}
		return ""
	}
}

func normalized(c Config) (Config, error) {
	if c.PollIntervalMS == 0 {
		c.PollIntervalMS = 1000
	}
	if c.TTLMS == 0 {
		c.TTLMS = 3000
	}
	if c.RequestTimeoutMS == 0 {
		c.RequestTimeoutMS = 1500
	}
	if c.SceneURL == "" || c.ReadTokenFile == "" || c.RunID == "" || len(c.RunID) > 128 || c.Site == "" || len(c.Site) > 256 || c.Frame.Validate() != nil || c.SimulationAnchorUnixNS <= 0 || c.DurationMS < 1 || c.DurationMS > 120000 || c.MaxSamples < 1 || c.MaxSamples > 64 || c.PollIntervalMS < 50 || c.PollIntervalMS > 10000 || c.TTLMS < 1000 || c.TTLMS > 10000 || c.RequestTimeoutMS < 10 || c.RequestTimeoutMS > 5000 || sensorKind(c.SensorID) == "" {
		return c, liveapp.ErrConfig
	}
	return c, nil
}

func LoadConfig(path string) (Config, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return Config{}, liveapp.ErrConfig
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return Config{}, liveapp.ErrConfig
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0022 != 0 || st.Size() < 1 || st.Size() > 16<<10 {
		return Config{}, liveapp.ErrConfig
	}
	raw, err := io.ReadAll(io.LimitReader(f, 16<<10+1))
	var cfg Config
	if err != nil || len(raw) > 16<<10 || jws.DecodeStrict(raw, &cfg) != nil {
		return cfg, liveapp.ErrConfig
	}
	return normalized(cfg)
}
