package assets

import "testing"

// skillByteBudgets caps the entry points an agent reads before acting. Agents
// load a skill whole, and every later model call in the task carries it again,
// so a few kilobytes of growth multiply into tens of thousands of tokens per
// task (WDY-3857). The router (wendy) is read by nearly every task and has the
// tightest cap; wendy/references/mcp.md is read by every MCP-connected agent.
//
// When a skill outgrows its budget, move situational detail into a reference
// file that the skill links to. Raise a budget only deliberately, in the same
// change, with the reason in its description.
var skillByteBudgets = map[string]int{
	"wendy/SKILL.md":                4096,
	"wendy/references/mcp.md":       7168,
	"wendy-app-lifecycle/SKILL.md":  6144,
	"wendy-data/SKILL.md":           6144,
	"wendy-device-debug/SKILL.md":   5120,
	"wendy-device-install/SKILL.md": 6144,
	"wendy-device-ops/SKILL.md":     6144,
	"wendy-entitlements/SKILL.md":   9216,
	"wendy-install/SKILL.md":        6144,
	"wendy-mcp-setup/SKILL.md":      6144,
	"wendy-onboarding/SKILL.md":     13312,
	"wendy-project-setup/SKILL.md":  6144,
	"wendy-robot-deploy/SKILL.md":   6144,
	"wendy-template-app/SKILL.md":   4096,
	// The opt-in training workflow includes job, evidence and checkpoint contracts.
	"wendy-training/SKILL.md": 12288,
}

func TestEndUserSkillsStayWithinByteBudgets(t *testing.T) {
	names, err := EndUserSkillNames()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if _, ok := skillByteBudgets[name+"/SKILL.md"]; !ok {
			t.Errorf("end-user skill %s has no byte budget; add one to skillByteBudgets", name)
		}
	}
	for path, budget := range skillByteBudgets {
		data, err := FS.ReadFile("skills/" + path)
		if err != nil {
			t.Errorf("budgeted skill file %s: %v", path, err)
			continue
		}
		if len(data) > budget {
			t.Errorf("plugins/wendy-agentic-coding/skills/%s is %d bytes, over its %d-byte budget; move situational detail into a linked reference file", path, len(data), budget)
		}
	}
}
