// Package livefusion runs the current synthetic multimodal rule as an ordinary
// World app. The agent socket supplies scoped reads and signing; this package
// holds only public verification roots and explicit simulation commissioning.
package livefusion

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"golang.org/x/sys/unix"
)

var (
	ErrConfig    = errors.New("invalid commissioned fusion configuration")
	ErrSource    = errors.New("exact current capture source unavailable")
	ErrMedia     = errors.New("exact authorized media unavailable")
	ErrUncertain = errors.New("result publication outcome unknown; no automatic retry")
)

const MaxResultBytes = 32 << 20

// One ordinary Query must return the complete source set without pagination.
const maxSources = serviceaccess.MaxQueryRecords

type Source struct {
	SensorID string `json:"sensor_id"`
	Device   string `json:"device,omitempty"`
	App      string `json:"app"`
	Service  string `json:"service"`
}

func (s Source) selector() serviceaccess.Source {
	return serviceaccess.Source{Device: s.Device, App: s.App, Service: s.Service, ComponentType: sensorrecord.ComponentType, Stream: s.SensorID}
}

type Config struct {
	Version                int            `json:"version"`
	Tenant                 string         `json:"tenant"`
	Site                   string         `json:"site"`
	Device                 string         `json:"device"`
	App                    string         `json:"app"`
	Service                string         `json:"service"`
	ExpectedGeneration     uint64         `json:"expected_generation,omitempty"`
	RootsDER               [][]byte       `json:"roots_der"`
	RunID                  string         `json:"run_id"`
	SceneBootID            string         `json:"scene_boot_id"`
	PackageSHA256          string         `json:"package_sha256"`
	SimulationAnchorUnixNS int64          `json:"simulation_anchor_unix_ns"`
	Frame                  world.FrameRef `json:"frame"`
	ScenarioVersion        string         `json:"scenario_version"`
	Seed                   uint64         `json:"seed"`
	Sources                []Source       `json:"sources"`
	ResultStream           string         `json:"result_stream"`
	DurationMS             int64          `json:"duration_ms"`
	RequestTimeoutMS       int64          `json:"request_timeout_ms,omitempty"`
	PollIntervalMS         int64          `json:"poll_interval_ms,omitempty"`
	MaxAttempts            int            `json:"max_attempts,omitempty"`
	MaxTicks               int            `json:"max_ticks,omitempty"`
	RetainForMS            int64          `json:"retain_for_ms,omitempty"`
	ClockMaxWidthMS        int64          `json:"clock_max_width_ms,omitempty"`
	MaxSkewMS              int64          `json:"max_skew_ms,omitempty"`
}

func (c Config) normalized() (Config, error) {
	if c.RequestTimeoutMS == 0 {
		c.RequestTimeoutMS = 1000
	}
	if c.PollIntervalMS == 0 {
		c.PollIntervalMS = 500
	}
	if c.MaxTicks == 0 {
		c.MaxTicks = 240
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 8
	}
	if c.RetainForMS == 0 {
		c.RetainForMS = 60000
	}
	if c.ClockMaxWidthMS == 0 {
		c.ClockMaxWidthMS = 5000
	}
	if c.MaxSkewMS == 0 {
		c.MaxSkewMS = 350
	}
	scope := serviceorigin.Scope{Tenant: c.Tenant, Site: c.Site, Device: c.Device, App: c.App, Service: c.Service, DeploymentGeneration: max(c.ExpectedGeneration, 1), ExecutionDomain: "simulation"}
	if c.Version != 1 || scope.Validate() != nil || c.Frame.Validate() != nil || c.Seed >= 1<<32 || len(c.RootsDER) < 1 || len(c.RootsDER) > 8 || !sensing.DigestValid(c.PackageSHA256) || c.SimulationAnchorUnixNS <= 0 || c.SimulationAnchorUnixNS > int64(^uint64(0)>>1)-int64(24*time.Hour) || len(c.Sources) < 4 || len(c.Sources) > maxSources || c.DurationMS < 1 || c.DurationMS > 120000 || c.RequestTimeoutMS < 10 || c.RequestTimeoutMS > 5000 || c.PollIntervalMS < 500 || c.PollIntervalMS > 10000 || c.MaxAttempts < 1 || c.MaxAttempts > 16 || c.MaxTicks < 1 || c.MaxTicks > 240 || c.RetainForMS < 1000 || c.RetainForMS > int64((7*24*time.Hour)/time.Millisecond) || c.ClockMaxWidthMS < 1 || c.ClockMaxWidthMS > 10000 || c.MaxSkewMS < 1 || c.MaxSkewMS > 1000 {
		return c, ErrConfig
	}
	for _, s := range []string{c.RunID, c.SceneBootID, c.ScenarioVersion} {
		if s == "" || len(s) > 128 || strings.TrimSpace(s) != s || strings.ContainsAny(s, "\r\n\x00") {
			return c, ErrConfig
		}
	}
	if (serviceaccess.Source{App: c.App, Service: c.Service, ComponentType: "wendy.inference.result.v1", Stream: c.ResultStream}).Validate() != nil {
		return c, ErrConfig
	}
	rootBytes := 0
	for _, root := range c.RootsDER {
		rootBytes += len(root)
		if len(root) == 0 || rootBytes > 64<<10 {
			return c, ErrConfig
		}
	}
	seen := map[string]bool{}
	services := map[string]bool{}
	c.Sources = append([]Source(nil), c.Sources...)
	for i, source := range c.Sources {
		resolved, err := source.selector().Resolve(c.Device)
		if err != nil {
			return c, ErrConfig
		}
		source.Device = resolved.Device
		key := source.Device + "\x00" + source.App + "\x00" + source.Service
		if seen[source.SensorID] || services[key] || key == c.Device+"\x00"+c.App+"\x00"+c.Service {
			return c, ErrConfig
		}
		switch source.SensorID {
		case "camera-a", "camera-b", "gas-1", "audio-1":
		default:
			if !sensing.LidarSensorID(source.SensorID) {
				return c, ErrConfig
			}
		}
		c.Sources[i] = source
		seen[source.SensorID] = true
		services[key] = true
	}
	for _, required := range []string{"camera-a", "camera-b", "gas-1", "audio-1"} {
		if !seen[required] {
			return c, ErrConfig
		}
	}
	roots := make([][]byte, len(c.RootsDER))
	for i, root := range c.RootsDER {
		roots[i] = bytes.Clone(root)
	}
	c.RootsDER = roots
	return c, nil
}

func LoadConfig(path string) (Config, error) {
	var c Config
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return c, ErrConfig
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return c, ErrConfig
	}
	f := os.NewFile(uintptr(fd), "live-fusion-config")
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0022 != 0 || st.Size() < 1 || st.Size() > 128<<10 {
		return c, ErrConfig
	}
	raw, err := io.ReadAll(io.LimitReader(f, 128<<10+1))
	if err != nil || len(raw) > 128<<10 || jws.DecodeStrict(raw, &c) != nil {
		return c, ErrConfig
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields["seed"]) == 0 || bytes.Equal(bytes.TrimSpace(fields["seed"]), []byte("null")) {
		return c, ErrConfig
	}
	return c.normalized()
}
