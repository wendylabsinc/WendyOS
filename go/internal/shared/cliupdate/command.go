// Package cliupdate contains CLI update guidance shared by terminal and MCP notices.
package cliupdate

// Command returns the CLI installer command for the host operating system.
func Command(goos string) string {
	switch goos {
	case "windows":
		return "winget upgrade WendyLabs.Wendy"
	case "darwin":
		return "brew update && brew install wendy"
	default:
		return "curl -fsSL https://install.wendy.dev/cli.sh | bash"
	}
}

// PluginAdvice is the update advice for a CLI the Wendy plugin's launcher
// installed. Updating the plugin brings only the CLI version that plugin
// release pins, which can trail the latest release. The launcher runs an
// installed wendy ahead of its own copy once it is at least the plugin's
// minimum version, so installing the CLI with Command also updates what the
// plugin runs.
func PluginAdvice(goos string) string {
	return "This CLI came with the Wendy plugin. Update the plugin, or install the CLI yourself (the plugin then uses it): " + Command(goos)
}
