// Package cliupdate contains CLI update guidance shared by terminal and MCP notices.
package cliupdate

// Command returns the installation-aware updater entry point. Keep the host
// parameter for callers compiled against the original guidance helper.
func Command(_ string) string { return "wendy update" }

// InstallCommand returns the command that installs the Wendy CLI on the host
// operating system.
func InstallCommand(goos string) string {
	if goos == "windows" {
		return "winget install WendyLabs.Wendy --source winget"
	}
	return "curl -fsSL https://install.wendy.dev/cli.sh | bash"
}

// PluginAdvice is the update advice for a CLI the Wendy plugin's launcher
// installed. No updater that wendy update recognizes owns that copy, and
// updating the plugin brings only the CLI version that plugin release pins,
// which can trail the latest release. The launcher runs an installed wendy
// ahead of its own copy once it is at least the plugin's minimum version, so
// installing the CLI with InstallCommand also updates what the plugin runs.
func PluginAdvice(goos string) string {
	return "This CLI came with the Wendy plugin. Update the plugin, or install the CLI yourself (the plugin then uses it): " + InstallCommand(goos)
}
