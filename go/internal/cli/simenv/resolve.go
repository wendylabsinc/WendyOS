package simenv

import (
	"fmt"
	"sort"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// ProfileCustom is the default profile: no known robot model, a clearly marked
// placeholder body in the scene scaffold.
const ProfileCustom = "custom"

// knownProfiles are the robot kinds that ship a simulator under
// go/simulator/<kind>/. A resolve request naming anything else is an error.
var knownProfiles = map[string]bool{
	"go2":          true,
	"g1":           true,
	"so101":        true,
	"rosmaster-r2": true,
}

// ros2VectorDims maps a fixed-width ROS 2 message type to its flattened vector
// length. The key is the message type with the interface segment removed, so
// both "geometry_msgs/Twist" and the wire form "geometry_msgs/msg/Twist"
// resolve through normalizeMessageType.
var ros2VectorDims = map[string]int{
	"geometry_msgs/Twist":       6,
	"sensor_msgs/Imu":           10,
	"nav_msgs/Odometry":         13,
	"geometry_msgs/PoseStamped": 7,
}

// ros2VariableLength names message types whose length depends on the robot and
// so cannot be sized from the campaign alone. They become a vector observation
// with nil dims and a warning telling the operator to set the shape by hand.
var ros2VariableLength = map[string]bool{
	"sensor_msgs/JointState": true,
}

// Input carries everything Resolve needs as plain values. The surrounding
// command line interface fetches the campaign and the device sources and hands
// them in; this package reaches for nothing on its own.
//
// Sources, ControlRateHz and ActionSpec are the campaign pieces. They are
// passed explicitly rather than as a data.Campaign because the Training block
// (rate and action) is being added to the campaign type by a sibling task and
// is not present on this branch; accepting the extracted values keeps this
// package independent of that branch.
type Input struct {
	// Campaign is the campaign name, Revision its resolved revision.
	Campaign string
	Revision string
	// Sources are the authored campaign sources, in plan order.
	Sources []data.CampaignSource
	// ControlRateHz is the training control rate, used as the fallback
	// observation rate when a camera source declares no capture rate.
	ControlRateHz float64
	// ActionSpec is the training action stream, nil when the campaign drives
	// no action.
	ActionSpec *Action
	// DataSources are the device's resolved sources (v2 DataService shape).
	// They supply ROS 2 message types through Detail and the topic list used
	// to expand a domain-level selector.
	DataSources []*agentpbv2.DataSource
	// VideoDevices are the discovered cameras, used to note a pixel format on
	// a matched camera observation.
	VideoDevices []*agentpb.VideoDevice
	// Profile is "custom" (the default) or a known robot kind.
	Profile string
}

// Resolve translates the campaign and resolved sources into an Env. It never
// fails on a source it cannot map: such a source is skipped and recorded in
// Env.Warnings. The only hard error is an unknown profile, because the scene
// scaffold cannot be rendered against a robot model that does not exist.
func Resolve(in Input) (Env, error) {
	profile := in.Profile
	if profile == "" {
		profile = ProfileCustom
	}
	if profile != ProfileCustom && !knownProfiles[profile] {
		return Env{}, fmt.Errorf("unknown profile %q: want custom, go2, g1, so101 or rosmaster-r2", profile)
	}

	env := Env{
		SchemaVersion: SchemaVersion,
		Campaign:      in.Campaign,
		Revision:      in.Revision,
		Profile:       profile,
		ControlRateHz: in.ControlRateHz,
		Observations:  []Observation{},
	}

	for _, src := range in.Sources {
		switch {
		case src.Camera != "":
			env.resolveCamera(in, src)
		case src.ROS2 != "":
			env.resolveROS2(in, src)
		case src.Audio != "":
			env.Warnings = append(env.Warnings,
				fmt.Sprintf("audio %s excluded: the simulator has no audio channel", src.Audio))
		case src.Telemetry:
			env.Warnings = append(env.Warnings,
				"telemetry excluded: device telemetry has no simulated counterpart")
		}
	}

	env.resolveAction(in)

	return env, nil
}

// resolveCamera turns a camera source into an image observation. Dims come from
// the capture resolution cap; when it is absent the shape is left unset with a
// warning. A matched VideoDevice contributes the pixel format.
func (env *Env) resolveCamera(in Input, src data.CampaignSource) {
	id, matched := resolveCameraSource(in.DataSources, src.Camera)
	source := id
	if !matched {
		source = "camera:" + strings.TrimSpace(src.Camera)
	}

	obs := Observation{
		Name:   observationName("camera", strings.TrimSpace(src.Camera)),
		Kind:   "image",
		Source: source,
		Dtype:  "uint8",
		RateHz: in.ControlRateHz,
	}
	if src.Capture != nil && src.Capture.Rate > 0 {
		obs.RateHz = src.Capture.Rate
	}

	if w, h, ok := src.Capture.MaxResolutionPixels(); ok {
		// Image tensors are row-major: height, then width, then channels.
		obs.Dims = []int{int(h), int(w), 3}
	} else {
		env.Warnings = append(env.Warnings,
			fmt.Sprintf("camera %s: no capture.max_resolution; simulated resolution unset", displaySource(source)))
	}

	if matched {
		if format, ok := pixelFormatFor(in.VideoDevices, id); ok {
			obs.PixelFormat = format
		}
	}

	env.Observations = append(env.Observations, obs)
}

// resolveROS2 turns a ROS 2 source into one or more vector observations. A
// topic selector yields the matching topics; a domain selector expands to every
// topic the resolved sources report on that domain. A selector that matches no
// resolved source is recorded as a warning.
func (env *Env) resolveROS2(in Input, src data.CampaignSource) {
	matches := matchROS2Sources(in.DataSources, src.ROS2)
	if len(matches) == 0 {
		env.Warnings = append(env.Warnings,
			fmt.Sprintf("ros2 %s: no resolved source matches; expand it on the device or set dims manually", src.ROS2))
		return
	}
	for _, ds := range matches {
		env.Observations = append(env.Observations, env.ros2Observation(ds))
	}
}

// ros2Observation builds a vector observation for one resolved ROS 2 source and
// appends a warning when the message type carries a variable length or is
// unrecognized.
func (env *Env) ros2Observation(ds *agentpbv2.DataSource) Observation {
	_, topic, _ := data.ParseROS2SourceID(ds.Id)
	name := topic
	if name == "" {
		name = ds.Id
	}
	msgType := strings.TrimSpace(ds.Detail)
	obs := Observation{
		Name:   observationName("ros2", strings.TrimPrefix(name, "/")),
		Kind:   "vector",
		Source: ds.Id,
		Dtype:  "float32",
	}
	normalized := normalizeMessageType(msgType)
	switch {
	case normalized == "":
		env.Warnings = append(env.Warnings,
			fmt.Sprintf("ros2 %s: message type unknown; set dims manually", topic))
	case ros2VariableLength[normalized]:
		env.Warnings = append(env.Warnings,
			fmt.Sprintf("ros2 %s (%s): joint count unknown without the robot; set dims manually", topic, msgType))
	default:
		if n, ok := ros2VectorDims[normalized]; ok {
			obs.Dims = []int{n}
		} else {
			env.Warnings = append(env.Warnings,
				fmt.Sprintf("ros2 %s (%s): unknown message type; set dims manually", topic, msgType))
		}
	}
	return obs
}

// resolveAction records the training action and, when the action stream also
// appears among the resolved sources, an action-echo vector observation.
func (env *Env) resolveAction(in Input) {
	if in.ActionSpec == nil {
		return
	}
	action := *in.ActionSpec
	env.Action = &action

	if !actionEchoed(in, action.Stream) {
		return
	}
	env.Observations = append(env.Observations, Observation{
		Name:   observationName("action", strings.TrimPrefix(action.Stream, "/")),
		Kind:   "vector",
		Source: action.Stream,
		Dims:   []int{len(action.Channels)},
		Dtype:  "float32",
		Unit:   action.Unit,
	})
}

// actionEchoed reports whether the action stream is also observed, either as a
// ROS 2 source selector in the plan or as a resolved source identifier or
// topic.
func actionEchoed(in Input, stream string) bool {
	if stream == "" {
		return false
	}
	for _, src := range in.Sources {
		if src.ROS2 != "" && (src.ROS2 == stream || "/"+strings.TrimPrefix(src.ROS2, "/") == stream) {
			return true
		}
	}
	for _, ds := range in.DataSources {
		if ds.Id == stream {
			return true
		}
		if _, topic, ok := data.ParseROS2SourceID(ds.Id); ok && topic == stream {
			return true
		}
	}
	return false
}

// resolveCameraSource matches a camera selector against the resolved camera
// sources: exact identifier, the v4l2 alias of a /dev path, a case-insensitive
// substring of the source detail, or the lone healthy camera for the "front"
// and "default" aliases. It mirrors the agent's resolveCameraSelector closely
// enough to stay consistent without importing it.
func resolveCameraSource(sources []*agentpbv2.DataSource, selector string) (string, bool) {
	selector = strings.TrimSpace(selector)
	aliases := []string{selector}
	if strings.HasPrefix(selector, "/dev/video") {
		aliases = append(aliases, "v4l2:"+selector)
	}
	var cameras []*agentpbv2.DataSource
	for _, ds := range sources {
		if ds.Kind != "camera" {
			continue
		}
		cameras = append(cameras, ds)
		for _, alias := range aliases {
			if ds.Id == alias || strings.EqualFold(ds.Id, alias) {
				return ds.Id, true
			}
		}
	}
	var matches []string
	for _, ds := range cameras {
		if selector != "" && strings.Contains(strings.ToLower(ds.Detail), strings.ToLower(selector)) {
			matches = append(matches, ds.Id)
		}
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	if len(cameras) == 1 && (selector == "front" || selector == "default") {
		return cameras[0].Id, true
	}
	return "", false
}

// pixelFormatFor returns the first advertised pixel format of the VideoDevice
// backing a v4l2 camera identifier, matched by its /dev path.
func pixelFormatFor(devices []*agentpb.VideoDevice, sourceID string) (string, bool) {
	path := strings.TrimPrefix(sourceID, "v4l2:")
	for _, dev := range devices {
		if dev.Path != "" && dev.Path == path && len(dev.PixelFormats) > 0 {
			return dev.PixelFormats[0], true
		}
	}
	return "", false
}

// matchROS2Sources resolves a campaign ROS 2 selector against resolved sources.
// It accepts a full per-topic identifier, a bare topic name, or a domain-level
// selector that expands to every resolved topic on that domain. The returned
// sources are sorted by identifier for deterministic output.
func matchROS2Sources(sources []*agentpbv2.DataSource, selector string) []*agentpbv2.DataSource {
	ros2 := func(ds *agentpbv2.DataSource) bool { return ds != nil && ds.Kind == "ros2" }

	// A selector that parses as a per-topic identifier names exactly one
	// topic. Accept it with or without the "ros2:" prefix.
	for _, candidate := range []string{selector, data.ROS2SourcePrefix + selector} {
		wantDomain, wantTopic, ok := data.ParseROS2SourceID(candidate)
		if !ok || wantTopic == "" {
			continue
		}
		var out []*agentpbv2.DataSource
		for _, ds := range sources {
			if !ros2(ds) {
				continue
			}
			domain, topic, parsed := data.ParseROS2SourceID(ds.Id)
			if parsed && domain == wantDomain && topic == wantTopic {
				out = append(out, ds)
			}
		}
		return sortSources(out)
	}

	// A bare topic name selects that topic on every domain that publishes it.
	if strings.HasPrefix(selector, "/") {
		var out []*agentpbv2.DataSource
		for _, ds := range sources {
			if !ros2(ds) {
				continue
			}
			if _, topic, ok := data.ParseROS2SourceID(ds.Id); ok && topic == selector {
				out = append(out, ds)
			}
		}
		return sortSources(out)
	}

	// Anything else is a domain-level selector. Expand to every resolved topic
	// on the matching domain; when the selector names one domain, filter to it,
	// otherwise expand across all domains.
	wantDomain, _, domainOK := data.ParseROS2SourceID(selector)
	if !domainOK {
		wantDomain, _, domainOK = data.ParseROS2SourceID(data.ROS2SourcePrefix + selector)
	}
	var out []*agentpbv2.DataSource
	for _, ds := range sources {
		if !ros2(ds) {
			continue
		}
		domain, topic, ok := data.ParseROS2SourceID(ds.Id)
		if !ok || topic == "" {
			continue
		}
		if domainOK && domain != wantDomain {
			continue
		}
		out = append(out, ds)
	}
	return sortSources(out)
}

func sortSources(sources []*agentpbv2.DataSource) []*agentpbv2.DataSource {
	sort.SliceStable(sources, func(i, j int) bool { return sources[i].Id < sources[j].Id })
	return sources
}

// normalizeMessageType strips the ROS 2 interface segment so that the wire form
// "pkg/msg/Type" and the short form "pkg/Type" resolve to the same key.
func normalizeMessageType(msgType string) string {
	msgType = strings.TrimSpace(msgType)
	if msgType == "" {
		return ""
	}
	return strings.Replace(msgType, "/msg/", "/", 1)
}

// observationName builds a stable, filesystem and identifier friendly name from
// a prefix and a selector.
func observationName(prefix, selector string) string {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return prefix
	}
	replacer := strings.NewReplacer("/", "_", ":", "_", " ", "_", ".", "_")
	return prefix + "_" + strings.Trim(replacer.Replace(selector), "_")
}

// displaySource returns the source identifier with the "camera:" scaffold
// prefix removed, for use in warning text.
func displaySource(source string) string {
	return strings.TrimPrefix(source, "camera:")
}
