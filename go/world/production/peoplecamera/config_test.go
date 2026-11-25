package peoplecamera

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/production/liveapp"
	"github.com/wendylabsinc/wendy/go/world/production/livecapture"
)

func TestLoadConfigAcceptsExactCameraIDsAndDefaults(t *testing.T) {
	for _, sensor := range []string{"camera-a", "camera-b", "people-front", "people-oblique", "gas-1", "audio-1", "lidar-atlas", "people-other"} {
		t.Run(sensor, func(t *testing.T) {
			_, cfg := fixture(t)
			cfg.SceneURL, cfg.ReadTokenFile = "http://127.0.0.1:8091", "/config/read-token"
			cfg.SensorID, cfg.DurationMS, cfg.MaxSamples = sensor, 3000, 1
			raw, _ := json.Marshal(cfg)
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			got, err := LoadConfig(path)
			wantCamera := sensor == "camera-a" || sensor == "camera-b" || sensor == "people-front" || sensor == "people-oblique"
			if !wantCamera {
				if !errors.Is(err, liveapp.ErrConfig) {
					t.Fatal("non-camera configuration accepted", err)
				}
				return
			}
			if err != nil || got.SensorID != sensor || got.TTLMS != 3000 || got.PollIntervalMS != 1000 || got.RequestTimeoutMS != 1500 {
				t.Fatal("camera defaults changed", got, err)
			}
			shared, err := livecapture.LoadConfig(path)
			if err != nil || shared != got {
				t.Fatal("camera loader differs from shared capture loader", err)
			}
			_, err = liveapp.LoadConfig(path)
			if sensor == "camera-a" || sensor == "camera-b" {
				if err != nil {
					t.Fatal("existing camera config refused by legacy loader", err)
				}
			} else if !errors.Is(err, liveapp.ErrConfig) {
				t.Fatal("legacy whole-scene loader accepted a new camera ID", err)
			}
		})
	}
}

func TestDecodeRefusesUnknownCameraID(t *testing.T) {
	f, cfg := namedFixture(t, "people-other")
	raw, _ := json.Marshal(f)
	if _, err := Decode(raw, cfg); !errors.Is(err, liveapp.ErrCapture) {
		t.Fatal("unknown camera ID accepted", err)
	}
}
