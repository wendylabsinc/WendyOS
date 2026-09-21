package main

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/hoststats"
	"github.com/wendylabsinc/wendy/go/internal/agent/hoststats/rosbattery"
	"github.com/wendylabsinc/wendy/go/internal/rtps"
)

// startROS2BatteryMonitor registers a battery source backed by the device's DDS
// bus, for hardware whose pack is not a Linux power supply.
//
// A Unitree Go2 is the motivating case: its BMS is reachable only over DDS, and
// the Jetson it runs on has no /sys/class/power_supply entry of its own — so
// without this, `wendy device top` and `wendy device info` correctly but
// unhelpfully report no battery at all.
//
// The monitor starts with the agent rather than lazily because
// `wendy device info` is a one-shot call: a monitor that only began on first
// use would return nothing the first time it was asked.
func startROS2BatteryMonitor(ctx context.Context, logger *zap.Logger, configPath string, pool *rtps.Pool) {
	cfg, err := rosbattery.LoadConfig(configPath)
	if err != nil {
		// A malformed config is worth saying out loud, but not worth refusing
		// to start over: carry on with auto-discovery.
		logger.Warn("Reading ROS 2 battery config; using defaults", zap.Error(err))
	}
	if !cfg.Enabled {
		logger.Info("ROS 2 battery monitor disabled by config")
		return
	}

	cache := rosbattery.NewCache(time.Now)
	// Info rather than Debug: which interface the monitor settled on, and
	// whether it found a topic at all, are the first things anyone debugging a
	// missing battery needs, and they are cheap — a handful of lines per scan.
	monitor := rosbattery.NewMonitor(cfg, cache, pool, func(format string, args ...any) {
		logger.Info(fmt.Sprintf(format, args...))
	})

	hoststats.SetFallbackBatterySource(monitor.Battery)
	hoststats.SetSupplementalThermalSource(monitor.ThermalZones)
	go monitor.Run(ctx)
}
