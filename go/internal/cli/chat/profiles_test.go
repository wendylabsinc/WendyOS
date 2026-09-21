package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProfilesAndToolRestrictions(t *testing.T) {
	if len(Profiles()) != 8 {
		t.Fatal("missing profiles")
	}
	p, err := ResolveProfile("")
	if err != nil || p.Name != "general" {
		t.Fatal(p, err)
	}
	if _, err := ResolveProfile("invented"); err == nil {
		t.Fatal("accepted unknown profile")
	}
	parent, _ := ResolveProfile("fleet")
	child, _ := ResolveProfile("developer")
	base := &engineTestExecutor{tools: []Tool{{Name: "workspace_write"}, {Name: "container_list"}, {Name: "invented"}}}
	tools := &ProfileTools{Base: base, Profile: child, Parent: &parent}
	list, err := tools.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[1].Name != "container_list" {
		t.Fatalf("unexpected tools: %+v", list)
	}
	if _, err := tools.Execute(context.Background(), ToolCall{Name: "workspace_write", Arguments: json.RawMessage(`{}`)}); err == nil || len(base.calls) != 0 {
		t.Fatal("parent policy bypassed")
	}
	if _, err := tools.Execute(context.Background(), ToolCall{Name: "invented", Arguments: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("unknown tool allowed")
	}
	for _, name := range []string{"../wendy/SKILL.md", "/wendy/SKILL.md", "swift/SKILL.md"} {
		fleet := &ProfileTools{Profile: parent}
		if _, err := fleet.skill(name); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
	if text, err := tools.skill("wendy/SKILL.md"); err != nil || !strings.Contains(text, "Wendy") {
		t.Fatal(text, err)
	}
}
func TestProfileMemoryIsolation(t *testing.T) {
	dir, workspace := t.TempDir(), t.TempDir()
	general, _ := ResolveProfile("general")
	dev, _ := ResolveProfile("developer")
	a, err := NewProfileMemoryStore(dir, workspace, "robot", general)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewProfileMemoryStore(dir, workspace, "robot", dev)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Save(context.Background(), MemoryInput{Scope: "global", Kind: "fact", Title: "test fact", Content: "red", Evidence: "user stated"})
	if err != nil {
		t.Fatal(err)
	}
	notes, err := b.Search(context.Background(), "", "", 10)
	if err != nil || len(notes) != 0 {
		t.Fatal("memory leaked", notes, err)
	}
	if a.Directory() != dir || b.Directory() == dir {
		t.Fatal("memory path mismatch")
	}
}
func TestAgentModelConnectionsDoNotLeakParentCredentials(t *testing.T) {
	t.Setenv("WENDY_CHAT_API_KEY", "parent-secret")
	t.Setenv("WENDY_CHAT_BASE_URL", "https://parent.example/v1")
	t.Setenv("WENDY_CHAT_MODEL", "parent-model")
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(`{"debugger":{"provider":"local","model":"small","base_url":"http://localhost:8080/v1"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	models, err := LoadAgentModels(path)
	if err != nil {
		t.Fatal(err)
	}
	c := models["debugger"]
	if c.APIKey != "" || c.Model != "small" || c.BaseURL != "http://localhost:8080/v1" {
		t.Fatalf("leaked settings: %+v", c)
	}
}
func TestServicePolicyChecksDirectExecution(t *testing.T) {
	base := &engineTestExecutor{tools: []Tool{{Name: "read"}, {Name: "write", RequiresApproval: true}}}
	tools := &serviceTools{base: base, allowed: []string{}}
	if _, err := tools.Execute(context.Background(), ToolCall{Name: "write"}); err == nil || len(base.calls) != 0 {
		t.Fatal("unattended write escaped policy")
	}
	if _, err := tools.Execute(context.Background(), ToolCall{Name: "read"}); err != nil {
		t.Fatal(err)
	}
}

func TestProfileStartupSkillsAreLoadedAndDiscoverable(t *testing.T) {
	for _, profile := range Profiles() {
		t.Run(profile.Name, func(t *testing.T) {
			tools := &ProfileTools{Profile: profile}
			listed, err := tools.skill("")
			if err != nil || !strings.Contains(listed, profile.StartupSkill) {
				t.Fatalf("startup skill not discoverable: %s, %v", listed, err)
			}
			body, err := tools.skill(profile.StartupSkill)
			if err != nil || body == "" {
				t.Fatalf("startup skill not readable: %v", err)
			}
			engine, err := sessionEngine(nil, &uiExecutor{}, SessionOptions{
				Profile: profile, Workspace: t.TempDir(), MemoryDirectory: t.TempDir(), NoMemory: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			messages := engine.Messages()
			if len(messages) == 0 || !strings.Contains(messages[0].Content, body) {
				t.Fatal("session did not receive its startup skill automatically")
			}
			for _, other := range Profiles() {
				if other.Name == profile.Name {
					continue
				}
				if strings.Contains(messages[0].Content, other.Instructions) {
					t.Fatalf("session eagerly loaded unrelated %s instructions", other.Name)
				}
				_, err := tools.skill(other.StartupSkill)
				if (err == nil) != (profile.Name == "general") {
					t.Fatalf("unexpected access to %s: %v", other.StartupSkill, err)
				}
			}
		})
	}
}

func TestSkillsDoNotExpandSensorToolPermissions(t *testing.T) {
	general, _ := ResolveProfile("general")
	fleet, _ := ResolveProfile("fleet")
	sensors, _ := ResolveProfile("device-sensors")
	base := &engineTestExecutor{tools: []Tool{{Name: "camera_list"}, {Name: "cloud_connect"}}}
	for _, tc := range []struct {
		profile, parent Profile
		camera          bool
	}{
		{fleet, general, false},
		{sensors, general, true},
		{sensors, fleet, false},
	} {
		tools := &ProfileTools{Base: base, Profile: tc.profile, Parent: &tc.parent}
		if _, err := tools.skill(tc.profile.StartupSkill); err != nil {
			t.Fatal(err)
		}
		list, err := tools.ListTools(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		camera := false
		for _, tool := range list {
			camera = camera || tool.Name == "camera_list"
		}
		if camera != tc.camera {
			t.Fatalf("unexpected camera access for %s under %s", tc.profile.Name, tc.parent.Name)
		}
	}
}

func TestFleetProfileIncludesCloudTunnel(t *testing.T) {
	profile, _ := ResolveProfile("fleet")
	tools := &ProfileTools{Profile: profile, Base: &engineTestExecutor{tools: []Tool{{Name: "cloud_tunnel"}}}}
	list, err := tools.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range list {
		found = found || tool.Name == "cloud_tunnel"
	}
	if !found {
		t.Fatal("fleet profile hides cloud_tunnel")
	}
}

func TestAgentModelFileRejectsNoncanonicalProfileKeys(t *testing.T) {
	for _, name := range []string{"", " developer "} {
		path := filepath.Join(t.TempDir(), "models.json")
		body := fmt.Sprintf(`{%q:{"provider":"local","model":"test","base_url":"http://127.0.0.1:1234"}}`, name)
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadAgentModels(path); err == nil || !strings.Contains(err.Error(), "canonical") {
			t.Fatalf("accepted profile %q: %v", name, err)
		}
	}
}
