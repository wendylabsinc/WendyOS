package env

import (
	"log"
	"os"
	"strings"
	"time"
)

func DiscoverUSBInterval() time.Duration {
	return parseDuration("WENDY_DISCOVER_USB_INTERVAL", 3*time.Second)
}

func DiscoverEthernetInterval() time.Duration {
	return parseDuration("WENDY_DISCOVER_ETHERNET_INTERVAL", 3*time.Second)
}

func DiscoverExternalInterval() time.Duration {
	return parseDuration("WENDY_DISCOVER_EXTERNAL_INTERVAL", 5*time.Second)
}

func Analytics() bool {
	v := strings.TrimSpace(os.Getenv("WENDY_ANALYTICS"))
	switch strings.ToLower(v) {
	case "", "true":
		return true
	case "false":
		return false
	default:
		log.Printf("WARNING: invalid WENDY_ANALYTICS=%q, expected \"true\" or \"false\", defaulting to true", v)
		return true
	}
}

// CIEnvVars are env vars whose presence (with any non-empty, non-whitespace
// value) indicates the process is running inside a CI system. Exported so
// tests in other packages can clear them without redefining the list.
var CIEnvVars = []string{
	"CI",
	"GITHUB_ACTIONS",
	"GITLAB_CI",
	"BUILDKITE",
	"CIRCLECI",
	"JENKINS_HOME",
	"TF_BUILD",
	"TEAMCITY_VERSION",
}

// IsCI reports whether the process is running inside a CI environment.
// Wendy uses this as a hard analytics kill switch — CI runs are never useful
// product signal and have historically inflated event volume by orders of
// magnitude. There is no opt-in flag that re-enables analytics in CI.
func IsCI() bool {
	for _, key := range CIEnvVars {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			return true
		}
	}
	return false
}

func SystemdServiceName() string {
	return stringOrDefault("WENDY_SYSTEMD_SERVICE_NAME", "edge-agent")
}

func parseDuration(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("WARNING: invalid %s=%q, using default %s", key, v, fallback)
		return fallback
	}
	if d <= 0 {
		log.Printf("WARNING: non-positive %s=%q, using default %s", key, v, fallback)
		return fallback
	}
	return d
}

func stringOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
		log.Printf("WARNING: blank %s=%q, using default %s", key, v, fallback)
	}
	return fallback
}
