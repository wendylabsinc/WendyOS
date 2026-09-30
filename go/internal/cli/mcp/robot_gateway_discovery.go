package mcp

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// GatewayCloudDevice comes from authenticated Cloud inventory, never tool input.
type GatewayCloudDevice struct {
	Device string
	Name   string
	Type   string
}

type GatewayCloudDiscoverFunc func(context.Context, GatewayCloudSource, bool) ([]GatewayCloudDevice, error)
type RobotGatewayOption func(*RobotGateway)

func WithRobotCloudDiscovery(discover GatewayCloudDiscoverFunc) RobotGatewayOption {
	return func(g *RobotGateway) { g.discover = discover }
}

type gatewayCatalogRobot struct {
	GatewayRobot
	source     string
	presence   string
	deviceType string
}

func discoveredRobotID(device string) string {
	digest := sha256.Sum256([]byte(device))
	return fmt.Sprintf("cloud-%x", digest[:16])
}

// Each request gets its own fresh catalog. There is no cross-account cache and
// removed Cloud devices cannot keep access through a previously listed ID.
func (g *RobotGateway) catalog(ctx context.Context, includeOffline bool) ([]gatewayCatalogRobot, []string) {
	grant, _ := g.grant(ctx)
	if grant == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	rows := []gatewayCatalogRobot{}
	warnings := []string{}
	configured := map[string]bool{}
	seen := map[string]bool{}
	byDevice := map[string]int{}
	for _, r := range g.cfg.Robots {
		configured[r.Device] = true
		if !slices.Contains(grant.Robots, r.ID) {
			continue
		}
		byDevice[r.Device], seen[r.ID] = len(rows), true
		rows = append(rows, gatewayCatalogRobot{GatewayRobot: r, source: "configured", presence: "unknown"})
	}
	if g.localSimulatorAllowed(ctx) && g.lifecycle.simulators.List != nil {
		items, err := g.lifecycle.simulators.List(ctx)
		if err != nil {
			warnings = append(warnings, "Local simulators could not be listed.")
		} else {
			for _, item := range items {
				id := "sim-" + item.Name
				if item.State != "running" || configured[item.Device] || seen[id] {
					continue
				}
				model := "generic"
				if item.Profile == "go2" {
					model = "go2"
				}
				rows = append(rows, gatewayCatalogRobot{GatewayRobot: GatewayRobot{ID: id, Name: item.Name, Device: item.Device, Model: model, discovered: true}, source: "simulator", presence: item.State, deviceType: item.Profile + " simulator"})
				seen[id] = true
			}
		}
	}
	for _, source := range g.cfg.CloudSources {
		if !slices.Contains(grant.CloudSources, source.ID) {
			continue
		}
		devices, err := g.discover(ctx, source, !includeOffline)
		if err != nil || len(devices) > 10000 {
			warnings = append(warnings, fmt.Sprintf("Cloud inventory %s could not be refreshed. The robot list is incomplete.", source.ID))
			continue
		}
		presence := "online"
		if includeOffline {
			presence = "unknown"
		}
		for _, device := range devices {
			if device.Device == "" {
				continue
			}
			if index, exists := byDevice[device.Device]; exists {
				rows[index].presence, rows[index].deviceType = presence, device.Type
				continue
			}
			// Explicit policies take precedence even when a subject has only a
			// discovery grant. Never bypass an explicit robot grant via discovery.
			if configured[device.Device] {
				continue
			}
			id := discoveredRobotID(device.Device)
			if seen[id] {
				warnings = append(warnings, "A conflicting robot identity was omitted from Cloud inventory.")
				continue
			}
			name := strings.TrimSpace(device.Name)
			if name == "" {
				name = "Unnamed device " + id[len(id)-8:]
			}
			name = string([]rune(name)[:min(len([]rune(name)), 128)])
			byDevice[device.Device], seen[id] = len(rows), true
			rows = append(rows, gatewayCatalogRobot{GatewayRobot: GatewayRobot{ID: id, Name: name, Device: device.Device, AllowCamera: source.AllowCamera, discovered: true}, source: "cloud", presence: presence, deviceType: device.Type})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows, warnings
}

func (g *RobotGateway) listRobots(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if !g.hasScope(ctx, RobotReadScope) {
		return mcpgo.NewToolResultError("robot access is not authorized"), nil
	}
	includeOffline := req.GetBool("include_offline", false)
	rows, warnings := g.catalog(ctx, includeOffline)
	query := strings.ToLower(strings.TrimSpace(req.GetString("query", "")))
	robots := []map[string]any{}
	for _, row := range rows {
		if query != "" && !strings.Contains(strings.ToLower(row.Name), query) && !strings.Contains(strings.ToLower(row.ID), query) {
			continue
		}
		robots = append(robots, map[string]any{"id": row.ID, "name": row.Name, "connection": "unknown", "cloud_presence": row.presence, "source": row.source, "device_type": row.deviceType, "model": gatewayModel(row.GatewayRobot, row.deviceType), "can_read_events": len(row.Triggers) > 0 && g.hasScope(ctx, RobotEventsScope), "can_capture": row.AllowCamera && g.hasScope(ctx, RobotCameraScope), "can_control_apps": len(row.Apps) > 0 && g.hasScope(ctx, RobotControlScope)})
	}
	limit, offset := req.GetInt("limit", 100), req.GetInt("offset", 0)
	if limit < 1 || limit > 200 || offset < 0 {
		return mcpgo.NewToolResultError("Use limit 1..200 and a nonnegative offset."), nil
	}
	start := min(offset, len(robots))
	end := start + min(limit, len(robots)-start)
	var next any
	if end < len(robots) {
		next = end
	}
	return okResult(map[string]any{"robots": robots[start:end], "total_count": len(robots), "next_offset": next, "include_offline": includeOffline, "discovery_complete": len(warnings) == 0, "warnings": warnings, "can_manage_simulators": g.localSimulatorAllowed(ctx), "note": "Cloud presence is not an agent connection check. Permission flags do not establish hardware capability."}), nil
}
