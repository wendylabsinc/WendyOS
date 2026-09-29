package commands

import (
	"fmt"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

const initProjectBack = "_back_to_deployments"

type initProjectGroup struct {
	key         string
	name        string
	description string
	target      string
	items       []tui.PickerItem
}

func initProjectGroups(meta *repoMeta, target string, allowSkip bool) []initProjectGroup {
	groups := []initProjectGroup{
		{key: "edge-ai", name: "Edge AI", description: "Apps for Jetson, Raspberry Pi, and other Linux edge devices", target: targetWendyOS},
		{key: "unitree-g1", name: "Unitree G1", description: "Projects for the Unitree G1 humanoid", target: targetWendyOS},
		{key: "unitree-go2", name: "Unitree Go2", description: "Projects for the Unitree Go2 quadruped", target: targetWendyOS},
		{key: "rosmaster", name: "ROSMaster", description: "Projects for Yahboom ROSMaster robots", target: targetWendyOS},
		{key: targetDarwin, name: "macOS", description: "Native apps for Wendy Agent for Mac", target: targetDarwin},
		{key: targetWendyLite, name: "ESP32", description: "Microcontroller projects for ESP32", target: targetWendyLite},
	}
	var available []initProjectGroup
	for _, group := range groups {
		if target != "" && group.target != target {
			continue
		}
		for _, tmpl := range meta.Templates {
			if templateTargetMatch(tmpl, group.target) && templateDeploymentKey(tmpl, group.target) == group.key {
				group.items = append(group.items, tui.PickerItem{
					Name: tmpl.Name, Description: tmpl.Description, Value: tmpl.Name,
				})
			}
		}
		// Keep manual setup available even when a platform has no templates.
		manual := allowSkip && (group.key == "edge-ai" || group.key == targetDarwin || group.key == targetWendyLite)
		if len(group.items) > 0 || manual {
			available = append(available, group)
		}
	}
	return available
}

func templateDeploymentKey(tmpl repoMetaTemplate, target string) string {
	if target != targetWendyOS {
		return target
	}
	// Catalogs can declare the deployment explicitly. Older catalogs use the
	// established robot template names, including rc-car for ROSMaster.
	switch tmpl.Deployment {
	case "edge-ai", "unitree-g1", "unitree-go2", "rosmaster":
		return tmpl.Deployment
	}
	name := strings.ToLower(tmpl.Name)
	switch {
	case name == "g1" || strings.HasPrefix(name, "g1-") || strings.HasPrefix(name, "unitree-g1-"):
		return "unitree-g1"
	case name == "go2" || strings.HasPrefix(name, "go2-") || strings.HasPrefix(name, "unitree-go2-"):
		return "unitree-go2"
	case name == "rc-car" || name == "rosmaster" || strings.HasPrefix(name, "rosmaster-"):
		return "rosmaster"
	default:
		return "edge-ai"
	}
}

// pickInitProject first selects a deployment within the platform tabs, then
// shows only that deployment's projects. A target flag restricts the tabs.
func pickInitProject(meta *repoMeta, target string, allowSkip bool) (string, string, error) {
	groups := initProjectGroups(meta, target, allowSkip)
	if len(groups) == 0 {
		return "", "", fmt.Errorf("no templates available for %s", target)
	}
	var items []tui.PickerItem
	for i, group := range groups {
		items = append(items, tui.PickerItem{
			Name: group.name, Description: group.description, Value: group.key, SortKey: fmt.Sprintf("%02d", i),
		})
	}
	for {
		fmt.Println()
		key, err := runTabbedPicker(newInitTargetPickerModel(items))
		if err != nil {
			return "", "", err
		}
		for _, group := range groups {
			if group.key != key {
				continue
			}
			projects := append([]tui.PickerItem(nil), group.items...)
			if allowSkip {
				projects = append(projects, tui.PickerItem{
					Name: "No template", Description: "Configure language and entitlements manually", Value: "", SortKey: "~0",
				})
			}
			projects = append(projects, tui.PickerItem{
				Name: "Back to deployment groups", Value: initProjectBack, SortKey: "~1",
			})
			name, err := pickFromItems("Choose a project for "+group.name, projects)
			if err != nil {
				return "", "", err
			}
			if name == initProjectBack {
				break
			}
			return group.target, name, nil
		}
	}
}
