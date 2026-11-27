// Package livepeople runs image-derived anonymous tracking as an ordinary World
// app. The app holds public verification roots and never an agent signing key.
package livepeople

import (
	"bytes"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"golang.org/x/sys/unix"
)

var (
	ErrConfig    = errors.New("invalid commissioned people configuration")
	ErrSource    = errors.New("current original camera source unavailable")
	ErrMedia     = errors.New("exact camera evidence unavailable")
	ErrUncertain = errors.New("track publication outcome unknown; no retry")
	ErrClosed    = errors.New("people session closed or faulted")
)

type Source struct {
	SensorID string `json:"sensor_id"`
	App      string `json:"app"`
	Service  string `json:"service"`
	Device   string `json:"device,omitempty"`
}

func (s Source) selector() serviceaccess.Source {
	return serviceaccess.Source{App: s.App, Service: s.Service, Device: s.Device, ComponentType: sensorrecord.ComponentType, Stream: s.SensorID}
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
	MaxTicks               int            `json:"max_ticks,omitempty"`
	RetainForMS            int64          `json:"retain_for_ms,omitempty"`
	ClockMaxWidthMS        int64          `json:"clock_max_width_ms,omitempty"`
	FloorY                 float64        `json:"floor_y"`
	AnkleHeightM           float64        `json:"ankle_height_m"`
	ModelID                string         `json:"model_id"`
	ModelVersion           string         `json:"model_version"`
	ModelSHA256            string         `json:"model_sha256"`
}

func (c Config) normalized() (Config, error) {
	if c.RequestTimeoutMS == 0 {
		c.RequestTimeoutMS = 1000
	}
	if c.PollIntervalMS == 0 {
		c.PollIntervalMS = 250
	}
	if c.MaxTicks == 0 {
		c.MaxTicks = 120
	}
	if c.RetainForMS == 0 {
		c.RetainForMS = 60000
	}
	if c.ClockMaxWidthMS == 0 {
		c.ClockMaxWidthMS = 5000
	}
	scope := serviceorigin.Scope{Tenant: c.Tenant, Site: c.Site, Device: c.Device, App: c.App, Service: c.Service, DeploymentGeneration: max(1, c.ExpectedGeneration), ExecutionDomain: "simulation"}
	if c.Version != 1 || scope.Validate() != nil || c.Frame.Validate() != nil || !sensing.DigestValid(c.PackageSHA256) || c.SimulationAnchorUnixNS <= 0 || c.SimulationAnchorUnixNS > math.MaxInt64-int64(24*time.Hour) || c.Seed >= 1<<32 || len(c.RootsDER) < 1 || len(c.RootsDER) > 8 || len(c.Sources) < 1 || len(c.Sources) > serviceaccess.MaxQuerySources || c.DurationMS < 1 || c.DurationMS > 120000 || c.RequestTimeoutMS < 10 || c.RequestTimeoutMS > 5000 || c.PollIntervalMS < 50 || c.PollIntervalMS > 10000 || c.MaxTicks < 1 || c.MaxTicks > 240 || c.RetainForMS < 1000 || c.RetainForMS > int64(7*24*time.Hour/time.Millisecond) || c.ClockMaxWidthMS < 1 || c.ClockMaxWidthMS > 10000 || math.IsNaN(c.FloorY) || math.IsInf(c.FloorY, 0) || math.Abs(c.FloorY) > 10000 || math.IsNaN(c.AnkleHeightM) || math.IsInf(c.AnkleHeightM, 0) || c.AnkleHeightM < 0 || c.AnkleHeightM > .3 {
		return c, ErrConfig
	}
	for _, v := range []string{c.RunID, c.SceneBootID, c.ScenarioVersion} {
		if v == "" || len(v) > 128 || strings.TrimSpace(v) != v || strings.ContainsAny(v, "\r\n\x00") {
			return c, ErrConfig
		}
	}
	if !sensing.DigestValid(c.ModelSHA256) {
		return c, ErrConfig
	}
	for _, v := range []string{c.ModelID, c.ModelVersion} {
		if v == "" || len(v) > 256 || strings.TrimSpace(v) != v || strings.ContainsAny(v, "\r\n\x00") {
			return c, ErrConfig
		}
	}
	if (serviceaccess.Source{App: c.App, Service: c.Service, ComponentType: resultrecord.ComponentType, Stream: c.ResultStream}).Validate() != nil {
		return c, ErrConfig
	}
	var total int
	roots := make([][]byte, len(c.RootsDER))
	for i, r := range c.RootsDER {
		total += len(r)
		if len(r) == 0 || total > 64<<10 {
			return c, ErrConfig
		}
		roots[i] = bytes.Clone(r)
	}
	c.RootsDER = roots
	c.Sources = append([]Source(nil), c.Sources...)
	seen := map[string]bool{}
	selectors := map[serviceaccess.Source]bool{}
	for i, s := range c.Sources {
		resolved, err := s.selector().Resolve(c.Device)
		if err != nil || seen[s.SensorID] || selectors[resolved] || resolved.Device == c.Device && s.App == c.App && s.Service == c.Service {
			return c, ErrConfig
		}
		seen[s.SensorID] = true
		selectors[resolved] = true
		c.Sources[i].Device = resolved.Device
	}
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
	f := os.NewFile(uintptr(fd), "people-config")
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0022 != 0 || st.Size() < 1 || st.Size() > 128<<10 {
		return c, ErrConfig
	}
	raw, err := io.ReadAll(io.LimitReader(f, 128<<10+1))
	if err != nil || len(raw) > 128<<10 || jws.DecodeStrict(raw, &c) != nil {
		return c, ErrConfig
	}
	return c.normalized()
}
