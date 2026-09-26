package commands

import (
	"strconv"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/analytics"
)

// deployMetrics records one deploy for the deploy_completed analytics event
// (WDY-3215). Every event carries the transport, duration_ms, success and
// deploy_started, plus the target platform and device type once known; a
// failed deploy also carries a bounded error_class.
//
// The deploy ends at the agent's Started acknowledgement (markStarted): from
// there on duration_ms stops counting and the deploy counts as a success,
// however the attached log session that follows ends (Ctrl-C, an app crash,
// a tunnel drop). Compose, multi-service, a fleet build (--build-host with
// several devices) and a fast-path run that follows an already-running
// container do not report that acknowledgement, so their duration and success
// still cover the whole command.
//
// Phase detail depends on the transport, and a phase is reported only once it
// happened:
//   - chunk: deploy_build_ms once the build ran; the push's upload and device
//     preparation times, bytes, chunks and layer reuse once the push completed;
//     deploy_start_ms once the agent confirmed the start; and the image size.
//   - registry: deploy_build_push_ms, the registry build+push (or the push of
//     an image the chunk-diff attempt already built). After a chunk-diff
//     failure it also carries deploy_fallback (why), deploy_image_bytes and
//     deploy_chunk_attempt_ms (the time the abandoned attempt cost), so field
//     data shows how much a fallback cost; a deploy that never tried
//     chunk-diff says why in deploy_fallback.
//   - buildhost, native, xcode, swiftpm, swift, compose, multiservice and
//     fastpath: no phase detail, only the fields every event carries.
//
// Every property is a bounded number or enum: never a path, host, image name
// or error text.
type deployMetrics struct {
	now       func() time.Time // clock seam; nil means time.Now
	began     time.Time
	started   bool // the agent acknowledged that the container started
	startedAt time.Time
	internal  bool // managed-robot provisioning: an internal build, never reported

	command        string
	transport      string // fastpath, chunk, registry, buildhost, multiservice, native, xcode, swiftpm, swift or compose; "" until chosen
	fallback       string // why the deploy used the registry push instead of chunk-diff
	targetPlatform string
	deviceType     string

	chunk *chunkDeployStats // set once the chunk-diff path runs; cleared by a fallback

	buildPushTime    time.Duration // the registry build+push, once it succeeded
	imageBytes       int64         // the chunk-diff attempt's image size, kept across a fallback
	chunkAttemptTime time.Duration // time spent in the chunk-diff attempt before a fallback
}

func newDeployMetrics(opts runOptions) *deployMetrics {
	command := "wendy run"
	if opts.isWatch() {
		command = "wendy watch"
	}
	return &deployMetrics{began: time.Now(), command: command, internal: opts.managedRobot}
}

func (m *deployMetrics) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// markStarted records the agent's Started acknowledgement: the deploy is done,
// and whatever follows belongs to the log session. The first call wins, so
// every start site may report it.
func (m *deployMetrics) markStarted() {
	if m == nil || m.started {
		return
	}
	m.started, m.startedAt = true, m.clock()
}

// fellBack records that the chunk-diff attempt failed with err after taking
// attempt, and that the deploy continues through the registry push. The
// attempt's phase timings do not describe that push, so only its cost and the
// image size it built survive.
func (m *deployMetrics) fellBack(err error, attempt time.Duration) {
	m.transport, m.fallback = "registry", chunkFallbackReason(err)
	if m.chunk != nil {
		m.imageBytes = m.chunk.imageBytes
	}
	m.chunkAttemptTime = attempt
	m.chunk = nil
}

// properties renders the event for a deploy that ended with err.
func (m *deployMetrics) properties(err error) map[string]string {
	end := m.clock()
	if m.started {
		end = m.startedAt
	}
	success := err == nil || m.started
	p := map[string]string{
		"command_name":     m.command,
		"command_root":     strings.TrimPrefix(m.command, "wendy "),
		"duration_ms":      durationMS(end.Sub(m.began)),
		"success":          strconv.FormatBool(success),
		"deploy_transport": m.transport,
		"deploy_started":   strconv.FormatBool(m.started),
	}
	if !success {
		p["error_class"] = ExecutionErrorClass(err)
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
	if m.buildPushTime > 0 {
		p["deploy_build_push_ms"] = durationMS(m.buildPushTime)
	}
	if m.chunkAttemptTime > 0 {
		p["deploy_chunk_attempt_ms"] = durationMS(m.chunkAttemptTime)
	}
	imageBytes := m.imageBytes
	if s := m.chunk; s != nil {
		imageBytes = s.imageBytes
		if s.buildTime > 0 {
			p["deploy_build_ms"] = durationMS(s.buildTime)
		}
		if s.pushCompleted {
			p["deploy_push_ms"] = durationMS(s.pushTime)
			p["deploy_upload_ms"] = durationMS(s.push.UploadTime)
			if s.push.Prepared {
				p["deploy_device_prepare_ms"] = durationMS(s.push.DeviceTime)
			}
			p["deploy_bytes_sent"] = strconv.FormatInt(s.push.SentBytes, 10)
			p["deploy_chunks_sent"] = strconv.Itoa(s.push.SentChunks)
			p["deploy_chunks_total"] = strconv.Itoa(s.push.TotalChunks)
			p["deploy_layers_total"] = strconv.Itoa(s.push.LayersTotal)
			p["deploy_layers_reused"] = strconv.Itoa(s.push.LayersReused)
			p["deploy_compression"] = "gzip" // WDY-3211 part 2 reports the chosen compressor here
		}
		if m.started {
			p["deploy_start_ms"] = durationMS(s.startTime)
		}
	}
	if imageBytes > 0 {
		p["deploy_image_bytes"] = strconv.FormatInt(imageBytes, 10)
	}
	return p
}

// emit sends deploy_completed once a deploy path was chosen; failures before
// that (a bad wendy.json, an unreachable device) are already covered by the
// command's own event. Managed-robot provisioning is an internal build, not a
// user's deploy, so it is never reported. It is a no-op when analytics is
// disabled, and in CI.
func (m *deployMetrics) emit(err error) {
	if m == nil || m.internal || m.transport == "" {
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

// chunkSkipReason is a bounded label for why a deploy went straight to the
// registry push without attempting chunk-diff.
func chunkSkipReason(isDarwinAgent bool, opts runOptions) string {
	switch {
	case isDarwinAgent:
		return "darwin_agent"
	case opts.deploy:
		return "deploy_only"
	case opts.chunking == chunkingOff:
		return "chunking_off"
	default:
		return "not_attempted"
	}
}

func durationMS(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) }
