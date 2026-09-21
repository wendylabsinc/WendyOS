package chat

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/assets"
)

// Profile is a specialization, independent of provider and execution location.
// Tool selection is enforced by ProfileTools; it is not an OS sandbox.
type Profile struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Instructions string   `json:"-"`
	StartupSkill string   `json:"startup_skill"`
	Skills       []string `json:"skills"`
	groups       []string
}

// Startup skills live with chat, separate from assets/skills which is synced
// from the external language-skill repository. Missing files fail the build.
//
//go:embed skills/general/SKILL.md
var generalSkill string

//go:embed skills/developer/SKILL.md
var developerSkill string

//go:embed skills/simulation/SKILL.md
var simulationSkill string

//go:embed skills/debugger/SKILL.md
var debuggerSkill string

//go:embed skills/fleet/SKILL.md
var fleetSkill string

//go:embed skills/device-reasoning/SKILL.md
var deviceReasoningSkill string

//go:embed skills/device-sensors/SKILL.md
var deviceSensorsSkill string

//go:embed skills/device-control/SKILL.md
var deviceControlSkill string

var profiles = []Profile{
	{Name: "general", Description: "Build, deploy, diagnose, and delegate to specialists", Skills: []string{"*"}, groups: []string{"*"}, Instructions: generalSkill, StartupSkill: "profiles/general/SKILL.md"},
	{Name: "developer", Description: "Build, test, and deploy applications", Skills: []string{"wendy", "swift", "swift-concurrency", "swift-nio", "hummingbird", "postgres-nio", "swift-valkey"}, groups: []string{"workspace", "connection", "container", "telemetry", "hardware", "sensors", "app"}, Instructions: developerSkill, StartupSkill: "profiles/developer/SKILL.md"},
	{Name: "simulation", Description: "Build and deploy simulations to VMs and other targets", Skills: []string{"wendy", "swift", "swift-concurrency"}, groups: []string{"workspace", "connection", "container", "telemetry", "hardware", "sensors", "app"}, Instructions: simulationSkill, StartupSkill: "profiles/simulation/SKILL.md"},
	{Name: "debugger", Description: "Diagnose software, ROS 2, LiDAR, cameras, and hardware", Skills: []string{"wendy", "swift", "swift-concurrency", "swift-nio"}, groups: []string{"workspace", "connection", "container", "telemetry", "hardware", "sensors", "network", "app"}, Instructions: debuggerSkill, StartupSkill: "profiles/debugger/SKILL.md"},
	{Name: "fleet", Description: "Coordinate work across devices over cloud or LAN", Skills: []string{"wendy"}, groups: []string{"connection", "container", "telemetry", "hardware", "network", "os"}, Instructions: fleetSkill, StartupSkill: "profiles/fleet/SKILL.md"},
	{Name: "device-reasoning", Description: "Reason about device goals, observations, and events", Skills: []string{"wendy"}, groups: []string{"connection", "container", "telemetry", "hardware", "sensors", "app"}, Instructions: deviceReasoningSkill, StartupSkill: "profiles/device-reasoning/SKILL.md"},
	{Name: "device-sensors", Description: "Configure perception models and interpret sensor observations", Skills: []string{"wendy"}, groups: []string{"connection", "container", "telemetry", "hardware", "sensors", "app"}, Instructions: deviceSensorsSkill, StartupSkill: "profiles/device-sensors/SKILL.md"},
	{Name: "device-control", Description: "Supervise motion models and execute bounded device goals", Skills: []string{"wendy"}, groups: []string{"connection", "container", "telemetry", "hardware", "sensors", "app"}, Instructions: deviceControlSkill, StartupSkill: "profiles/device-control/SKILL.md"},
}

func Profiles() []Profile {
	out := append([]Profile(nil), profiles...)
	for i := range out {
		out[i].Skills = append([]string(nil), out[i].Skills...)
		out[i].groups = append([]string(nil), out[i].groups...)
	}
	return out
}

func ResolveProfile(name string) (Profile, error) {
	name = firstValue(name, "general")
	for _, p := range Profiles() {
		if p.Name == name {
			return p, nil
		}
	}
	return Profile{}, fmt.Errorf("unknown chat profile %q; use --list-profiles", name)
}

func (p Profile) Prompt(workspace, device string) string {
	return SystemPrompt(workspace, device) + "\n\nActive profile: " + p.Name + "\nStartup skill (" + p.StartupSkill + "):\n" + p.Instructions + `
The role skill above is already loaded. Use profile_skill to list and read additional bundled skills and references when relevant.
Before connecting or taking other actions, check that the tools required for the assigned task are in your offered tool list. If not, report the missing capability immediately.
Return concise findings, evidence, and blockers. Do not repeat the assignment or provide a transcript of routine calls.
Profiles select tools and instructions; approved shell and container execution still have their normal access.
When agent_delegate is available, submit independent tasks together to run them in parallel. Children receive only the task text, their profile, scoped memory, and tools. Include all required context. Child tools use the same approval policy. Children cannot delegate again. Do not delegate the same file edits or motion ownership concurrently. Report child failures and incomplete work explicitly.`
}

func (p Profile) allows(name string) bool {
	group := toolGroup(name)
	for _, allowed := range p.groups {
		if allowed == "*" || allowed == group {
			return true
		}
	}
	return name == "wendy_docs" || name == "profile_skill"
}

func toolGroup(name string) string {
	switch name {
	case "cloud_tunnel":
		return "network"
	case "wendy_status", "device_list", "device_connect", "device_disconnect", "device_info", "cloud_discover", "cloud_connect", "cloud_ping":
		return "connection"
	case "run":
		return "container"
	case "background_process_list", "background_process_stop", "audio_listen":
		return "sensors"
	}
	for _, prefix := range []string{"workspace_", "container_", "telemetry_", "hardware_", "os_"} {
		if strings.HasPrefix(name, prefix) {
			return strings.TrimSuffix(prefix, "_")
		}
	}
	for _, prefix := range []string{"camera_", "ros2_", "audio_"} {
		if strings.HasPrefix(name, prefix) {
			return "sensors"
		}
	}
	for _, prefix := range []string{"wifi_", "bluetooth_"} {
		if strings.HasPrefix(name, prefix) {
			return "network"
		}
	}
	if strings.Contains(name, "__") {
		return "app"
	}
	return ""
}

var profileSkillTool = Tool{Name: "profile_skill", Description: "List or read bundled skills available to this profile, including its already-loaded startup skill. Omit path to list files; supply a listed relative path to read a skill or reference.", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","maxLength":500}},"additionalProperties":false}`)}

// ProfileTools applies both the child's and parent's tool policies. Checks also
// happen on execution, so a hidden tool cannot be called by name.
type ProfileTools struct {
	Base    Executor
	Profile Profile
	Parent  *Profile
}

func (p *ProfileTools) allowed(name string) bool {
	return p.Profile.allows(name) && (p.Parent == nil || p.Parent.allows(name))
}
func (p *ProfileTools) ListTools(ctx context.Context) ([]Tool, error) {
	all, err := p.Base.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	out := []Tool{profileSkillTool}
	for _, t := range all {
		if p.allowed(t.Name) {
			out = append(out, t)
		}
	}
	return out, nil
}
func (p *ProfileTools) Execute(ctx context.Context, call ToolCall) (string, error) {
	result, err := p.ExecuteResult(ctx, call)
	return result.Text, err
}
func (p *ProfileTools) ExecuteResult(ctx context.Context, call ToolCall) (ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	if !p.allowed(call.Name) {
		return ToolResult{}, fmt.Errorf("tool %q is unavailable to profile %s", call.Name, p.Profile.Name)
	}
	if call.Name == "profile_skill" {
		if err := validateArguments(profileSkillTool, call.Arguments); err != nil {
			return ToolResult{}, err
		}
		var args struct{ Path string }
		_ = json.Unmarshal(call.Arguments, &args)
		text, err := p.skill(args.Path)
		return ToolResult{Text: text}, err
	}
	if media, ok := p.Base.(MediaExecutor); ok {
		return media.ExecuteResult(ctx, call)
	}
	text, err := p.Base.Execute(ctx, call)
	return ToolResult{Text: text}, err
}

func (p *ProfileTools) skill(name string) (string, error) {
	if strings.HasPrefix(name, "profiles/") {
		for _, profile := range Profiles() {
			if name == profile.StartupSkill && (p.Profile.Name == "general" || p.Profile.Name == profile.Name) {
				return profile.Instructions, nil
			}
		}
		return "", fmt.Errorf("unavailable skill path %q; list profile_skill first", name)
	}
	allowed := func(name string) bool {
		root := strings.Split(name, "/")[0]
		for _, skill := range p.Profile.Skills {
			if skill == "*" || skill == root {
				return true
			}
		}
		return false
	}
	if name == "" {
		var files []string
		for _, profile := range Profiles() {
			if p.Profile.Name == "general" || p.Profile.Name == profile.Name {
				files = append(files, profile.StartupSkill)
			}
		}
		err := fs.WalkDir(assets.FS, "skills", func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel := strings.TrimPrefix(name, "skills/")
			if !entry.IsDir() && strings.HasSuffix(rel, ".md") && allowed(rel) {
				files = append(files, rel)
			}
			return nil
		})
		return strings.Join(files, "\n"), err
	}
	if !fs.ValidPath(name) || !allowed(name) || !strings.HasSuffix(name, ".md") {
		return "", fmt.Errorf("unavailable skill path %q; list profile_skill first", name)
	}
	data, err := assets.FS.ReadFile(path.Join("skills", name))
	if err != nil {
		return "", err
	}
	return truncateOutput(string(data)), nil
}

// General retains existing memory. Specialists have their own durable notes,
// still scoped by workspace/device/global within each profile directory.
func NewProfileMemoryStore(directory, workspace, device string, profile Profile) (*MemoryStore, error) {
	store, err := NewMemoryStore(directory, workspace, device)
	if err != nil {
		return nil, err
	}
	profile, err = ResolveProfile(profile.Name)
	if err != nil {
		return nil, err
	}
	if profile.Name != "general" {
		store.directory = filepath.Join(store.directory, "profiles", profile.Name)
	}
	return store, nil
}
