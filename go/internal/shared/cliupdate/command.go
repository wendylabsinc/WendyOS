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
