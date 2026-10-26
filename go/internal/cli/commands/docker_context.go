package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// dockerSelection mirrors Docker's selection precedence without running a
// subprocess. The identity namespaces automatic builders; explicit Wendy
// builder names remain an operator override and are never renamed or deleted.
func dockerSelection() (context, identity string) {
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".docker")
	}
	if name := strings.TrimSpace(os.Getenv("DOCKER_CONTEXT")); name != "" {
		return name, "context:" + dir + ":" + name
	}
	if host := strings.TrimSpace(os.Getenv("DOCKER_HOST")); host != "" {
		return "", "host:" + host
	}
	var config struct {
		CurrentContext string `json:"currentContext"`
	}
	if data, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
		_ = json.Unmarshal(data, &config)
	}
	if config.CurrentContext != "" && config.CurrentContext != "default" {
		return config.CurrentContext, "context:" + dir + ":" + config.CurrentContext
	}
	return "", ""
}
