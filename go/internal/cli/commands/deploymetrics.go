package commands

import (
	"strconv"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/analytics"
)

// deployMetrics records one deploy's transport, phase timings and sizes for
// the deploy_completed analytics event, so field data can tell build-bound,
// upload-bound, device-bound and fallback-bound deploys apart (WDY-3215).
// Every property is a bounded number or enum: never a path, host, image name
// or error text.
type deployMetrics struct {
	started        time.Time
	command        string
	transport      string // fastpath, chunk, registry, buildhost, multiservice, native, xcode, swiftpm, swift or compose; "" until chosen
	fallback       string // why the deploy used the registry push instead of chunk-diff
	targetPlatform string
	deviceType     string
	chunk          *chunkDeployStats // set once the chunk-diff path runs
}

func newDeployMetrics(opts runOptions) *deployMetrics {
	command := "wendy run"
	if opts.isWatch() {
		command = "wendy watch"
	}
	return &deployMetrics{started: time.Now(), command: command}
}

// properties renders the event for a deploy that ended with err.
func (m *deployMetrics) properties(err error) map[string]string {
	p := map[string]string{
		"command_name":     m.command,
		"command_root":     strings.TrimPrefix(m.command, "wendy "),
		"duration_ms":      durationMS(time.Since(m.started)),
		"success":          strconv.FormatBool(err == nil),
		"deploy_transport": m.transport,
	}
	if m.fallback != "" {
		p["deploy_fallback"] = m.fallback
	}
	if m.targetPlatform != "" {
		p["deploy_target_platform"] = m.targetPlatform
	}
	if m.deviceType != "" {
		p["deploy_device_type"] = m.deviceType
	}
	if s := m.chunk; s != nil {
		p["deploy_build_ms"] = durationMS(s.buildTime)
		p["deploy_push_ms"] = durationMS(s.pushTime)
		p["deploy_upload_ms"] = durationMS(s.push.UploadTime)
		p["deploy_device_prepare_ms"] = durationMS(s.push.DeviceTime)
		p["deploy_start_ms"] = durationMS(s.startTime)
		p["deploy_image_bytes"] = strconv.FormatInt(s.imageBytes, 10)
		p["deploy_bytes_sent"] = strconv.FormatInt(s.push.SentBytes, 10)
		p["deploy_chunks_sent"] = strconv.Itoa(s.push.SentChunks)
		p["deploy_chunks_total"] = strconv.Itoa(s.push.TotalChunks)
		p["deploy_layers_total"] = strconv.Itoa(s.push.LayersTotal)
		p["deploy_layers_reused"] = strconv.Itoa(s.push.LayersReused)
		p["deploy_compression"] = "gzip" // WDY-3211 part 2 reports the chosen compressor here
	}
	return p
}

// emit sends deploy_completed once a deploy path was chosen; failures before
// that (a bad wendy.json, an unreachable device) are already covered by the
// command's own event. It is a no-op when analytics is disabled, and in CI.
func (m *deployMetrics) emit(err error) {
	if m == nil || m.transport == "" {
		return
	}
	analytics.Track("deploy_completed", m.properties(err))
}

// chunkFallbackReason is a bounded label for why a chunk-diff deploy fell back
// to the registry push.
func chunkFallbackReason(err error) string {
	switch {
	case isUnimplementedRPCError(err):
		return "unimplemented"
	case retryableTunnelError(err):
		return "transport"
	default:
		return "other"
	}
}

func durationMS(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) }
