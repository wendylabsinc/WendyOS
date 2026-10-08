package assets

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
)

// EndUserSkillNames is the audience boundary shared by native installation and
// MCP distribution. Other embedded skills are not part of the end-user group.
func EndUserSkillNames() ([]string, error) {
	data, err := FS.ReadFile("skills/end-user-group.json")
	if err != nil {
		return nil, err
	}
	var group struct {
		Name     string   `json:"name"`
		Audience string   `json:"audience"`
		Skills   []string `json:"skills"`
	}
	if err := json.Unmarshal(data, &group); err != nil {
		return nil, err
	}
	if group.Name != "wendy-agentic-coding" || group.Audience != "end-users" || len(group.Skills) == 0 {
		return nil, fmt.Errorf("invalid embedded end-user skill group")
	}
	seen := map[string]bool{}
	for _, name := range group.Skills {
		if name == "." || !fs.ValidPath(name) || strings.ContainsAny(name, "/\\") || seen[name] {
			return nil, fmt.Errorf("invalid or duplicate end-user skill %q", name)
		}
		seen[name] = true
		if _, err := fs.Stat(FS, "skills/"+name+"/SKILL.md"); err != nil {
			return nil, err
		}
	}
	return group.Skills, nil
}
