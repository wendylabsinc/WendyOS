package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

const (
	RobotReadScope      = "robots:read"
	RobotCameraScope    = "cameras:capture"
	RobotControlScope   = "apps:control"
	RobotToolsScope     = "apps:tools"
	RobotEventsScope    = "events:read"
	RobotTriggerScope   = "triggers:write"
	RobotSettingsScope  = "preferences:write"
	RobotProjectScope   = "projects:read"
	RobotDeployScope    = "apps:deploy"
	RobotHostScope      = "host:manage"
	RobotSimulatorScope = "simulators:manage"
)

var robotGatewayScopes = []string{RobotReadScope, RobotCameraScope, RobotControlScope, RobotToolsScope, RobotEventsScope, RobotTriggerScope, RobotSettingsScope, RobotProjectScope, RobotDeployScope, RobotHostScope, RobotSimulatorScope}
var gatewayIdentifier = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// RobotGatewayConfig is operator-owned policy, never model-provided routing.
// HTTP users can only exercise the intersection of OAuth scopes and their grant.
type RobotGatewayConfig struct {
	Workspaces          []GatewayWorkspace   `json:"workspaces,omitempty"`
	AllowHostOperations bool                 `json:"allow_host_operations,omitempty"`
	AllowSimulators     bool                 `json:"allow_simulators,omitempty"`
	StateDirectory      string               `json:"state_directory,omitempty"`
	Robots              []GatewayRobot       `json:"robots"`
	Grants              []GatewayGrant       `json:"grants"`
	LocalSubject        string               `json:"local_subject,omitempty"`
	HTTP                *GatewayHTTPConfig   `json:"http,omitempty"`
	CloudSources        []GatewayCloudSource `json:"cloud_sources,omitempty"`
}

// GatewayCloudSource pins discovery to an operator-selected Cloud identity.
// It never follows a changing CLI default or a model-supplied endpoint.
type GatewayCloudSource struct {
	ID             string `json:"id"`
	Endpoint       string `json:"endpoint"`
	OrganizationID int32  `json:"organization_id,omitempty"`
	TenantUUID     string `json:"tenant_uuid,omitempty"`
	AllowCamera    bool   `json:"allow_camera,omitempty"`
}

type GatewayRobot struct {
	Model       string           `json:"model,omitempty"`
	Triggers    []GatewayTrigger `json:"triggers,omitempty"`
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Device      string           `json:"device"`
	Apps        []string         `json:"apps"`
	AllowCamera bool             `json:"allow_camera"`
	ListAllApps bool             `json:"list_all_apps,omitempty"`
	Exports     []GatewayExport  `json:"exports,omitempty"`
	discovered  bool
}

// A trigger is an operator-reviewed campaign, not arbitrary model code or YAML
// supplied by a conversation. An external YOLO app can use an event-only entry.
type GatewayTrigger struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	AppID        string `json:"app_id"`
	Event        string `json:"event"`
	CampaignYAML string `json:"campaign_yaml,omitempty"`
}

// GatewayExport freezes the reviewed app descriptor. The published wrapper adds
// explicit robot_id and nests app arguments so schemas cannot override routing.
type GatewayExport struct {
	Name string     `json:"name"`
	App  string     `json:"app"`
	Tool mcpgo.Tool `json:"tool"`
}

type GatewayGrant struct {
	Workspaces   []string `json:"workspaces,omitempty"`
	Subject      string   `json:"subject"`
	Robots       []string `json:"robots"`
	Scopes       []string `json:"scopes"`
	CloudSources []string `json:"cloud_sources,omitempty"`
}
type GatewayWorkspace struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Path   string   `json:"path"`
	Robots []string `json:"robots"`
}

func DecodeRobotGatewayConfig(r io.Reader) (RobotGatewayConfig, error) {
	var cfg RobotGatewayConfig
	b, err := io.ReadAll(io.LimitReader(r, (1<<20)+1))
	if err != nil {
		return cfg, err
	}
	if len(b) > 1<<20 {
		return cfg, fmt.Errorf("gateway configuration exceeds 1 MiB")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		return cfg, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return cfg, fmt.Errorf("expected one gateway configuration object")
	}
	return cfg, cfg.validate()
}

func (c RobotGatewayConfig) validate() error {
	projects := map[string]bool{}
	for _, w := range c.Workspaces {
		if !gatewayIdentifier.MatchString(w.ID) || projects[w.ID] || !filepath.IsAbs(w.Path) || w.Name == "" {
			return fmt.Errorf("workspaces require unique IDs, names and absolute paths")
		}
		projects[w.ID] = true
	}
	if (len(c.Robots) == 0 && len(c.CloudSources) == 0) || len(c.Robots) > 100 || len(c.CloudSources) > 8 {
		return fmt.Errorf("configure robots or Cloud sources, with at most 100 explicit robots and 8 sources")
	}
	sources, identities := map[string]bool{}, map[string]bool{}
	for _, source := range c.CloudSources {
		host, port, err := net.SplitHostPort(source.Endpoint)
		if !gatewayIdentifier.MatchString(source.ID) || sources[source.ID] || err != nil || host == "" || port == "" || strings.ContainsAny(source.Endpoint, "/?#@") {
			return fmt.Errorf("Cloud sources need unique IDs and explicit host:port endpoints")
		}
		if (source.OrganizationID > 0) == (source.TenantUUID != "") || source.OrganizationID < 0 {
			return fmt.Errorf("Cloud source %s needs exactly one organization ID or tenant UUID", source.ID)
		}
		if source.TenantUUID != "" {
			id, err := uuid.Parse(source.TenantUUID)
			if err != nil || id == uuid.Nil || id.String() != source.TenantUUID {
				return fmt.Errorf("Cloud source %s needs a canonical nonzero tenant UUID", source.ID)
			}
		}
		key := fmt.Sprintf("%s/%d/%s", source.Endpoint, source.OrganizationID, source.TenantUUID)
		if identities[key] {
			return fmt.Errorf("Cloud sources must not repeat an organization or tenant")
		}
		sources[source.ID], identities[key] = true, true
	}
	ids, names := map[string]bool{}, map[string]bool{}
	for _, name := range append(append(desktopToolNames, gatewayLifecycleTools...), gatewayHostTools...) {
		names[name] = true
	}
	for _, name := range []string{"list_robots", "open_robot", "inspect_robot", "capture_robot_image", "start_robot_app", "stop_robot_app", "open_robot_app", "start_camera_preview", "read_camera_preview", "stop_camera_preview"} {
		names[name] = true
	}
	for _, r := range c.Robots {
		if !gatewayIdentifier.MatchString(r.ID) || ids[r.ID] || r.Name == "" || len(r.Name) > 128 || r.Device == "" {
			return fmt.Errorf("robots need unique IDs, names, and explicit device selectors")
		}
		ids[r.ID] = true
		if r.Model != "" && !gatewayModels[r.Model] {
			return fmt.Errorf("unknown model %s", r.Model)
		}
		triggers := map[string]bool{}
		for _, t := range r.Triggers {
			if !gatewayIdentifier.MatchString(t.ID) || triggers[t.ID] || t.Name == "" || t.AppID == "" || t.Event == "" || len(t.CampaignYAML) > 65536 {
				return fmt.Errorf("invalid trigger on robot %s", r.ID)
			}
			if err := validateGatewayTrigger(t); err != nil {
				return err
			}
			triggers[t.ID] = true
		}
		apps := map[string]bool{}
		for _, app := range r.Apps {
			if app == "" || len(app) > 256 || apps[app] {
				return fmt.Errorf("robot %s has an invalid or repeated app", r.ID)
			}
			apps[app] = true
		}
		for _, e := range r.Exports {
			if !gatewayIdentifier.MatchString(e.Name) || names[e.Name] || !apps[e.App] || e.Tool.Name == "" || e.Tool.Description == "" {
				return fmt.Errorf("robot %s has an invalid app tool export", r.ID)
			}
			if e.Tool.Annotations.ReadOnlyHint == nil || e.Tool.Annotations.DestructiveHint == nil || e.Tool.Annotations.OpenWorldHint == nil || e.Tool.Annotations.IdempotentHint == nil {
				return fmt.Errorf("export %s requires all four explicit behavior annotations", e.Name)
			}
			names[e.Name] = true
		}
	}
	subjects := map[string]bool{}
	for _, grant := range c.Grants {
		for _, id := range grant.Workspaces {
			if !projects[id] {
				return fmt.Errorf("grant references unknown workspace %s", id)
			}
		}
		if grant.Subject == "" || subjects[grant.Subject] {
			return fmt.Errorf("grants need unique, nonempty subjects")
		}
		subjects[grant.Subject] = true
		for _, id := range grant.Robots {
			if !ids[id] {
				return fmt.Errorf("grant references unknown robot %s", id)
			}
		}
		for _, scope := range grant.Scopes {
			if !slices.Contains(robotGatewayScopes, scope) {
				return fmt.Errorf("unknown gateway scope %s", scope)
			}
		}
		for _, source := range grant.CloudSources {
			if !sources[source] {
				return fmt.Errorf("grant references unknown Cloud source %s", source)
			}
		}
	}
	if len(subjects) == 0 {
		return fmt.Errorf("at least one subject grant is required")
	}
	if c.LocalSubject != "" && !subjects[c.LocalSubject] {
		return fmt.Errorf("local_subject must have an explicit grant")
	}
	return nil
}
