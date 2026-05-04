// Package containerd implements the ContainerdClient interface using the official
// containerd v2 SDK to manage containers, images, and content on the agent device.
package containerd

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/distribution/reference"

	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// normalizeImageName canonicalises a Docker short reference (e.g.
// "python:3.11-slim", "nginx") to a fully-qualified form
// ("docker.io/library/python:3.11-slim") that containerd's reference parser
// accepts. References that already include a registry, tag, or digest pass
// through unchanged. When the input cannot be parsed as a valid Docker
// reference, the original string is returned so existing error paths still
// surface a meaningful diagnostic.
func normalizeImageName(image string) string {
	trimmed := strings.TrimSpace(image)
	if trimmed == "" {
		return image
	}
	named, err := reference.ParseNormalizedNamed(trimmed)
	if err != nil {
		return image
	}
	return reference.TagNameOnly(named).String()
}

// labelKeyAppVersion is the containerd label key that marks Wendy-managed containers.
const labelKeyAppVersion = "sh.wendy/app.version"

// labelKeyRestartPolicy stores the restart policy (e.g. "on-failure:5").
const labelKeyRestartPolicy = "sh.wendy/restart.policy"

// labelKeyMCPPort stores the MCP server port for containers with an mcp entitlement.
const labelKeyMCPPort = "sh.wendy/mcp.port"

// labelKeyEntitlementPrefix is the prefix for per-entitlement labels; each
// entitlement is stored as sh.wendy/entitlement.<index> so it can be
// codesigned alongside the rest of the container metadata.
const labelKeyEntitlementPrefix = "sh.wendy/entitlement."

// labelKeyGCRoot prevents garbage collection of content blobs.
const labelKeyGCRoot = "containerd.io/gc.root"

// labelKeyWendyLayer marks a content blob as a Wendy-pushed layer.
const labelKeyWendyLayer = "sh.wendy.layer"

// computeChainID computes the chain ID for a layer given its parent chain ID
// and the layer's diff ID. The chain ID is defined recursively:
//
//	chainID(L0) = diffID(L0)
//	chainID(L0|...|Ln) = SHA256(chainID(L0|...|Ln-1) + " " + diffID(Ln))
func computeChainID(parent, diffID string) string {
	if parent == "" {
		return diffID
	}
	h := sha256.New()
	h.Write([]byte(parent + " " + diffID))
	return fmt.Sprintf("sha256:%x", h.Sum(nil))
}

// parseRestartPolicyLabel parses a restart policy label value such as
// "on-failure:5" or "unless-stopped" into the policy string and max retries.
func parseRestartPolicyLabel(label string) (string, int) {
	parts := strings.SplitN(label, ":", 2)
	policy := parts[0]
	maxRetries := 0
	if len(parts) == 2 {
		if n, err := strconv.Atoi(parts[1]); err == nil {
			maxRetries = n
		}
	}
	return policy, maxRetries
}

// isLocalRegistryImage reports whether the image reference points at the
// device-local HTTP registry. Such pulls must use a PlainHTTP resolver, but
// they should be a fallback only — the registry shares containerd's content
// store, so a successful GetImage avoids round-tripping bytes over loopback.
func isLocalRegistryImage(imageName string) bool {
	return strings.HasPrefix(imageName, "localhost:5000/") ||
		strings.HasPrefix(imageName, "127.0.0.1:5000/") ||
		strings.HasPrefix(imageName, "[::1]:5000/") ||
		strings.HasPrefix(imageName, "localhost:5555/") ||
		strings.HasPrefix(imageName, "127.0.0.1:5555/") ||
		strings.HasPrefix(imageName, "[::1]:5555/")
}

// gcTimestamp returns an RFC3339 timestamp string suitable for use as a GC root
// label value, anchoring content so it is not garbage collected.
func gcTimestamp() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// wendyLabels builds the standard set of containerd labels for a Wendy-managed
// container. These labels are used to identify, filter, and manage containers.
func wendyLabels(appName, version string, restartPolicy *agentpb.RestartPolicy, entitlements []appconfig.Entitlement) map[string]string {
	labels := map[string]string{
		labelKeyAppVersion: version,
	}

	if restartPolicy != nil {
		policyStr := restartPolicyToLabel(restartPolicy)
		if policyStr != "" {
			labels[labelKeyRestartPolicy] = policyStr
		}
	}

	for _, e := range entitlements {
		if e.Type == appconfig.EntitlementMCP && e.Port > 0 {
			labels[labelKeyMCPPort] = strconv.FormatUint(uint64(e.Port), 10)
			break
		}
	}

	for i, e := range entitlements {
		if data, err := json.Marshal(e); err == nil {
			labels[fmt.Sprintf("%s%d", labelKeyEntitlementPrefix, i)] = string(data)
		}
	}

	return labels
}

// restartPolicyToLabel converts a protobuf RestartPolicy to a label string.
func restartPolicyToLabel(rp *agentpb.RestartPolicy) string {
	if rp == nil {
		return ""
	}
	switch rp.GetMode() {
	case agentpb.RestartPolicyMode_NO:
		return "no"
	case agentpb.RestartPolicyMode_UNLESS_STOPPED:
		return "unless-stopped"
	case agentpb.RestartPolicyMode_ON_FAILURE:
		maxRetries := rp.GetOnFailureMaxRetries()
		if maxRetries > 0 {
			return fmt.Sprintf("on-failure:%d", maxRetries)
		}
		return "on-failure"
	case agentpb.RestartPolicyMode_DEFAULT:
		return "unless-stopped"
	default:
		return ""
	}
}
