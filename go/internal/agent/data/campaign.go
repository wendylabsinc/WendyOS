package data

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const CampaignVersion = 1

// ErrInvalidCampaignName marks a syntactically invalid campaign name so RPC
// handlers can return InvalidArgument rather than NotFound.
var ErrInvalidCampaignName = errors.New("invalid campaign name")

// SourceCapture is an optional per-source capture policy. The camera adapter
// implements the "continuous" (default) and "snapshot" modes; other
// combinations are validated so authored plans are portable, and deployment
// reports them as not yet implemented rather than silently recording
// continuously.
//
// Snapshot stills and the continuous-mode rate cap additionally require a
// camera transport that delivers whole encoded access units, which today
// means a local V4L2 camera with native H.264 output. IP cameras and
// GStreamer-encoded pipelines (CSI sensors, USB cameras without native H.264)
// deliver byte-stream chunks that cannot be cut into standalone files without
// corruption, so those sources record continuously at the stream rate and the
// episode manifest's source detail records that the policy was not applied.
// Which case applies is only knowable once the stream is running, so this is
// reported per episode rather than warned at deployment.
type SourceCapture struct {
	// Mode is one of continuous (default), snapshot, fragment, or threshold.
	Mode string `json:"mode,omitempty" yaml:"mode,omitempty"`
	// Interval is the snapshot period (snapshot mode only).
	Interval string `json:"interval,omitempty" yaml:"interval,omitempty"`
	// Rate caps the capture rate in hertz (continuous mode only).
	Rate float64 `json:"rate,omitempty" yaml:"rate,omitempty"`
	// Pre and Post bound a fragment around an occurrence (fragment mode only).
	Pre  string `json:"pre,omitempty" yaml:"pre,omitempty"`
	Post string `json:"post,omitempty" yaml:"post,omitempty"`
	// Trigger is a field threshold expression such as "model.uncertainty > 0.9"
	// or "level_db > -20" (threshold mode only). The 0..1 range applies only to
	// model.uncertainty; other fields carry their own units.
	Trigger string `json:"trigger,omitempty" yaml:"trigger,omitempty"`
	// Fragment is the captured duration per threshold crossing (threshold mode only).
	Fragment string `json:"fragment,omitempty" yaml:"fragment,omitempty"`
	// MaxResolution caps camera capture as WxH, for example 1280x720 (camera sources only).
	MaxResolution string `json:"max_resolution,omitempty" yaml:"max_resolution,omitempty"`
}

// EffectiveMode returns the declared capture mode, defaulting to continuous.
func (sc *SourceCapture) EffectiveMode() string {
	if sc == nil || sc.Mode == "" {
		return "continuous"
	}
	return sc.Mode
}

// IntervalDuration returns the validated snapshot interval, or zero when the
// policy declares none.
func (sc *SourceCapture) IntervalDuration() time.Duration {
	if sc == nil || sc.Interval == "" {
		return 0
	}
	d, err := time.ParseDuration(sc.Interval)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// MaxResolutionPixels returns the validated WxH resolution cap.
func (sc *SourceCapture) MaxResolutionPixels() (uint32, uint32, bool) {
	if sc == nil || sc.MaxResolution == "" {
		return 0, 0, false
	}
	width, height, found := strings.Cut(sc.MaxResolution, "x")
	w, errW := strconv.Atoi(width)
	h, errH := strconv.Atoi(height)
	if !found || errW != nil || errH != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return uint32(w), uint32(h), true
}

type CampaignSource struct {
	Camera      string         `json:"camera,omitempty" yaml:"camera,omitempty"`
	Audio       string         `json:"audio,omitempty" yaml:"audio,omitempty"`
	ROS2        string         `json:"ros2,omitempty" yaml:"ros2,omitempty"`
	Telemetry   bool           `json:"telemetry,omitempty" yaml:"telemetry,omitempty"`
	Calibration string         `json:"calibration_revision,omitempty" yaml:"calibration_revision,omitempty"`
	Capture     *SourceCapture `json:"capture,omitempty" yaml:"capture,omitempty"`
}

func (s CampaignSource) describe() string {
	switch {
	case s.Camera != "":
		return "camera:" + s.Camera
	case s.Audio != "":
		return "audio:" + s.Audio
	case s.ROS2 != "":
		return "ros2:" + s.ROS2
	case s.Telemetry:
		return "telemetry"
	}
	return "unknown"
}

func (s CampaignSource) kind() string {
	switch {
	case s.Camera != "":
		return "camera"
	case s.Audio != "":
		return "audio"
	case s.ROS2 != "":
		return "ros2"
	case s.Telemetry:
		return "telemetry"
	}
	return "unknown"
}

type CampaignTrigger struct {
	Event            string `json:"event,omitempty" yaml:"event,omitempty"`
	ModelUncertainty string `json:"model_uncertainty,omitempty" yaml:"model.uncertainty,omitempty"`
}

type CampaignCapture struct {
	Buffer string `json:"buffer" yaml:"buffer"`
	// Drain holds the episode open for late application records after its
	// capture adapters stop. An empty value takes DefaultSealDrain; "0s" opts
	// out. The omitempty tag is load-bearing: planOnly is marshalled to JSON to
	// compute Revision, so a field rendered on every campaign would change the
	// revision digest of every already-deployed campaign.
	Drain        string            `json:"drain,omitempty" yaml:"drain,omitempty"`
	AfterTrigger string            `json:"after_trigger" yaml:"after_trigger"`
	Triggers     []CampaignTrigger `json:"triggers" yaml:"triggers"`
}

type CampaignUpload struct {
	// When is one of always, wifi, or manual.
	When string `json:"when" yaml:"when"`
	// Destination is an optional logical dataset name the fleet backend maps
	// to storage. It is not a URL; devices never receive storage credentials
	// or bucket layouts through campaign plans.
	Destination string `json:"destination,omitempty" yaml:"destination,omitempty"`
	// MaxRate caps upload bandwidth in bytes per second. Plain integers and
	// human-readable rates such as "5MB/s" are accepted.
	MaxRate string `json:"max_rate,omitempty" yaml:"max_rate,omitempty"`
}

type CampaignRetention struct {
	// LocalQuota bounds on-device episode storage in bytes. Plain integers and
	// human-readable sizes such as "10GiB" are accepted.
	LocalQuota string `json:"local_quota,omitempty" yaml:"local_quota,omitempty"`
}

type CampaignExport struct {
	Annotation string `json:"annotation" yaml:"annotation"`
}

type CampaignPrivacy struct {
	Name     string `json:"name" yaml:"name"`
	Revision string `json:"revision,omitempty" yaml:"revision,omitempty"`
}

// NotifyOnEpisodeCommitted is the only notification event campaign version 1
// supports: fire when an episode this campaign captured is committed.
const NotifyOnEpisodeCommitted = "episode_committed"

// ErrUnsupportedNotifyOn marks a notify.on value this schema version does not
// support, so callers can identify the rejection with errors.Is.
var ErrUnsupportedNotifyOn = errors.New("notify.on must be " + NotifyOnEpisodeCommitted)

// CampaignNotify is an optional, device-inert notification policy. The device
// attaches no behavior to it: the block rides verbatim in the committed
// episode manifest (and therefore in the manifest JSON the cloud ingest
// service receives), and the cloud decides whether and whom to notify. No
// device-side network activity ever results from this block.
type CampaignNotify struct {
	// On names the event to notify on. NotifyOnEpisodeCommitted is the only
	// supported value.
	On string `json:"on" yaml:"on"`
	// UnknownKeys lists keys inside the notify block this schema version does
	// not know. Unlike the rest of the campaign document, which rejects
	// unknown fields, the notify block is read by the cloud rather than the
	// device, so a newer cloud-side key must not brick deployment to an older
	// device; DeployCampaign warns instead. The list is deploy-time state, not
	// plan content: it is excluded from the stored plan and the revision hash.
	UnknownKeys []string `json:"-" yaml:"-"`
}

// UnmarshalYAML decodes the notify block by hand so unknown keys inside it are
// collected for a deploy-time warning instead of tripping the document-wide
// KnownFields rejection. Comparing the raw key scalar also sidesteps YAML 1.1
// resolving an unquoted "on" key as a boolean.
func (n *CampaignNotify) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return errors.New("notify must be a mapping such as {on: " + NotifyOnEpisodeCommitted + "}")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		switch key {
		case "on":
			if err := node.Content[i+1].Decode(&n.On); err != nil {
				return fmt.Errorf("notify.on: %w", err)
			}
		default:
			n.UnknownKeys = append(n.UnknownKeys, key)
		}
	}
	return nil
}

// Campaign is the durable, device-local collection plan. Deployment arms its
// triggers; it does not require the configured sensors or network destination
// to be online at deployment time.
type Campaign struct {
	Version           int               `json:"version" yaml:"version"`
	Name              string            `json:"name" yaml:"name"`
	Fleet             string            `json:"fleet,omitempty" yaml:"fleet,omitempty"`
	Sources           []CampaignSource  `json:"sources" yaml:"sources"`
	Capture           CampaignCapture   `json:"capture" yaml:"capture"`
	Upload            CampaignUpload    `json:"upload" yaml:"upload"`
	Retention         CampaignRetention `json:"retention,omitempty" yaml:"retention,omitempty"`
	Export            CampaignExport    `json:"export" yaml:"export"`
	Models            map[string]string `json:"models" yaml:"models,omitempty"`
	Privacy           []CampaignPrivacy `json:"privacy" yaml:"privacy,omitempty"`
	Notify            *CampaignNotify   `json:"notify,omitempty" yaml:"notify,omitempty"`
	State             string            `json:"state" yaml:"-"`
	Revision          string            `json:"revision" yaml:"-"`
	DeployedUnixNanos int64             `json:"deployed_unix_nanos" yaml:"-"`
	Warnings          []string          `json:"warnings" yaml:"-"`
}

func ParseCampaign(contents []byte) (Campaign, error) {
	var campaign Campaign
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)
	if err := decoder.Decode(&campaign); err != nil {
		return Campaign{}, fmt.Errorf("parsing campaign YAML: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return Campaign{}, errors.New("campaign YAML must contain exactly one document")
	}
	if err := campaign.validate(); err != nil {
		return Campaign{}, err
	}
	campaign.State = "armed"
	if campaign.Models == nil {
		campaign.Models = map[string]string{}
	}
	if campaign.Privacy == nil {
		campaign.Privacy = []CampaignPrivacy{}
	}
	canonical, err := json.Marshal(campaign.planOnly())
	if err != nil {
		return Campaign{}, err
	}
	digest := sha256.Sum256(canonical)
	campaign.Revision = hex.EncodeToString(digest[:])
	return campaign, nil
}

// planOnly strips deployment state before hashing. The author-declared
// schema version and every plan field, including per-source capture policy,
// upload policy, and retention, feed the revision digest.
func (c Campaign) planOnly() Campaign {
	c.State, c.Revision, c.DeployedUnixNanos, c.Warnings = "", "", 0, nil
	return c
}

func (c Campaign) validate() error {
	if c.Version == 0 {
		return fmt.Errorf("version is required; this agent supports campaign version %d", CampaignVersion)
	}
	if c.Version != CampaignVersion {
		return fmt.Errorf("campaign version %d is not supported; this agent supports up to version %d", c.Version, CampaignVersion)
	}
	if c.Name == "" || safeName(c.Name) != c.Name || len(c.Name) > 128 {
		return errors.New("campaign name must use only letters, numbers, '.', '-' or '_'")
	}
	if len(c.Sources) == 0 {
		return errors.New("campaign must define at least one source")
	}
	for i, source := range c.Sources {
		kinds := 0
		if strings.TrimSpace(source.Camera) != "" {
			kinds++
		}
		if strings.TrimSpace(source.Audio) != "" {
			kinds++
		}
		if strings.TrimSpace(source.ROS2) != "" {
			kinds++
		}
		if source.Telemetry {
			kinds++
		}
		if kinds != 1 {
			return fmt.Errorf("sources[%d] must select exactly one of camera, audio, ros2, or telemetry", i)
		}
		if err := validateSourceCapture(source); err != nil {
			return fmt.Errorf("sources[%d].capture: %w", i, err)
		}
	}
	buffer, err := time.ParseDuration(c.Capture.Buffer)
	if err != nil || buffer < 0 || buffer > preRollWindow {
		return fmt.Errorf("capture.buffer must be a duration from 0s through %s", preRollWindow)
	}
	if c.Capture.Drain != "" {
		drain, drainErr := time.ParseDuration(c.Capture.Drain)
		if drainErr != nil || drain < 0 || drain > maxSealDrain {
			return fmt.Errorf("capture.drain must be a duration from 0s through %s", maxSealDrain)
		}
	}
	after, err := time.ParseDuration(c.Capture.AfterTrigger)
	if err != nil || after <= 0 || after > 24*time.Hour {
		return errors.New("capture.after_trigger must be a duration greater than 0s and no more than 24h")
	}
	if len(c.Capture.Triggers) == 0 {
		return errors.New("capture.triggers must contain at least one trigger")
	}
	for i, trigger := range c.Capture.Triggers {
		if (trigger.Event == "") == (trigger.ModelUncertainty == "") {
			return fmt.Errorf("capture.triggers[%d] must define exactly one event or model.uncertainty", i)
		}
		if trigger.ModelUncertainty != "" {
			if _, _, err := parseThreshold("model.uncertainty", trigger.ModelUncertainty); err != nil {
				return fmt.Errorf("capture.triggers[%d]: %w", i, err)
			}
		}
	}
	switch c.Upload.When {
	case "always", "wifi", "manual":
	default:
		return errors.New("upload.when must be one of always, wifi, or manual")
	}
	if c.Upload.MaxRate != "" {
		if _, err := parseByteRate(c.Upload.MaxRate); err != nil {
			return fmt.Errorf("upload.max_rate: %w", err)
		}
	}
	if c.Retention.LocalQuota != "" {
		if _, err := parseByteSize(c.Retention.LocalQuota); err != nil {
			return fmt.Errorf("retention.local_quota: %w", err)
		}
	}
	if c.Export.Annotation == "" {
		return errors.New("export.annotation is required")
	}
	for i, transform := range c.Privacy {
		if strings.TrimSpace(transform.Name) == "" {
			return fmt.Errorf("privacy[%d].name is required", i)
		}
	}
	if c.Notify != nil && c.Notify.On != NotifyOnEpisodeCommitted {
		return fmt.Errorf("notify.on %q is not supported: %w", c.Notify.On, ErrUnsupportedNotifyOn)
	}
	return nil
}

func (c Campaign) BufferDuration() time.Duration {
	d, _ := time.ParseDuration(c.Capture.Buffer)
	return d
}

// DrainDuration is how long an episode this campaign triggers stays open for
// late application records after its capture adapters stop. A campaign that
// declares nothing takes DefaultSealDrain, so asynchronous scoring is filed
// correctly by default; "drain: 0s" opts out and seals immediately.
func (c Campaign) DrainDuration() time.Duration {
	if c.Capture.Drain == "" {
		return DefaultSealDrain
	}
	d, _ := time.ParseDuration(c.Capture.Drain)
	return d
}

func (c Campaign) AfterTriggerDuration() time.Duration {
	d, _ := time.ParseDuration(c.Capture.AfterTrigger)
	return d
}

// preRollParameterConflicts names the camera sources whose explicit stream
// parameters this campaign's pre-roll will not honor, so deployment can warn
// while the operator can still change the plan.
//
// A camera source that is armed for pre-roll subscribes to the producer WITHOUT
// asserting stream parameters, because arming must never take a running camera
// away from a viewer, and because asserting them at the trigger would change the
// sequence parameter set mid-clip, which costs the episode its browser-playable
// rendering. The consequence is that an armed source records at whatever the
// producer is already running, so an explicit max_resolution or rate on that
// same source is requested but not achieved.
//
// Only sources that are actually armed conflict. Snapshot-mode camera sources
// are never armed (snapshot capture writes sparse independent stills, not a
// continuous ring), so their explicit parameters are still honored through the
// normal capture join and they are deliberately not named here.
func preRollParameterConflicts(c Campaign) []string {
	if c.BufferDuration() <= 0 {
		return nil
	}
	var conflicts []string
	for i, source := range c.Sources {
		if source.Camera == "" || source.Capture == nil {
			continue
		}
		if source.Capture.EffectiveMode() != "continuous" {
			continue
		}
		var explicit []string
		if source.Capture.MaxResolution != "" {
			explicit = append(explicit, "max_resolution "+source.Capture.MaxResolution)
		}
		if source.Capture.Rate > 0 {
			explicit = append(explicit, fmt.Sprintf("rate %g", source.Capture.Rate))
		}
		if len(explicit) > 0 {
			conflicts = append(conflicts, fmt.Sprintf("sources[%d] (%s, %s)", i, source.describe(), strings.Join(explicit, " and ")))
		}
	}
	return conflicts
}

func (m *Manager) campaignDir() string { return filepath.Join(filepath.Dir(m.root), "campaigns") }

func (m *Manager) DeployCampaign(contents []byte) (Campaign, error) {
	campaign, err := ParseCampaign(contents)
	if err != nil {
		return Campaign{}, err
	}
	if err := m.checkDeployableAudioSources(campaign); err != nil {
		return Campaign{}, err
	}
	campaign.DeployedUnixNanos = time.Now().UnixNano()
	if campaign.BufferDuration() > 0 {
		for _, source := range campaign.Sources {
			if source.Audio != "" || source.ROS2 != "" {
				campaign.Warnings = append(campaign.Warnings, "pre-trigger buffering is exact for application records and camera streams; audio and ROS 2 streams start at the trigger and report their achieved offset")
				break
			}
		}
	}
	var pendingModes []string
	for i, source := range campaign.Sources {
		if mode := source.Capture.EffectiveMode(); !implementedCaptureModes[source.kind()][mode] {
			pendingModes = append(pendingModes, fmt.Sprintf("sources[%d] (%s, mode %s)", i, source.describe(), mode))
		}
	}
	if len(pendingModes) > 0 {
		campaign.Warnings = append(campaign.Warnings, "these capture modes are not implemented yet for their source kind; the sources record continuously for now: "+strings.Join(pendingModes, ", "))
	}
	if conflicts := preRollParameterConflicts(campaign); len(conflicts) > 0 {
		campaign.Warnings = append(campaign.Warnings, "pre-roll and explicit camera stream parameters are mutually exclusive in this release; a buffered camera arms a standby subscription that asserts no stream parameters, so both the pre-roll and the live tail record at the producer's running parameters and the explicit max_resolution or rate is reported as requested but not achieved in the episode manifest: "+strings.Join(conflicts, ", "))
	}
	if campaign.Retention.LocalQuota != "" {
		campaign.Warnings = append(campaign.Warnings, "retention.local_quota is recorded with the plan, but this release enforces only the device-wide storage quota")
	}
	if campaign.Notify != nil && len(campaign.Notify.UnknownKeys) > 0 {
		campaign.Warnings = append(campaign.Warnings, "notify has unknown keys this agent ignores: "+strings.Join(campaign.Notify.UnknownKeys, ", "))
	}
	dir := m.campaignDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return Campaign{}, err
	}
	b, err := json.MarshalIndent(campaign, "", "  ")
	if err != nil {
		return Campaign{}, err
	}
	b = append(b, '\n')
	tmp := filepath.Join(dir, campaign.Name+".json.tmp")
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return Campaign{}, err
	}
	if err := os.Rename(tmp, filepath.Join(dir, campaign.Name+".json")); err != nil {
		return Campaign{}, err
	}
	return campaign, nil
}

func (m *Manager) Campaign(name string) (Campaign, error) {
	if name == "" || safeName(name) != name {
		return Campaign{}, ErrInvalidCampaignName
	}
	var campaign Campaign
	b, err := os.ReadFile(filepath.Join(m.campaignDir(), name+".json"))
	if err != nil {
		return Campaign{}, err
	}
	if err := json.Unmarshal(b, &campaign); err != nil {
		return Campaign{}, err
	}
	return campaign, nil
}

func (m *Manager) Campaigns() ([]Campaign, error) {
	entries, err := os.ReadDir(m.campaignDir())
	if errors.Is(err, os.ErrNotExist) {
		return []Campaign{}, nil
	}
	if err != nil {
		return nil, err
	}
	var campaigns []Campaign
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		campaign, err := m.Campaign(strings.TrimSuffix(entry.Name(), ".json"))
		if err == nil {
			campaigns = append(campaigns, campaign)
		}
	}
	sort.Slice(campaigns, func(i, j int) bool { return campaigns[i].Name < campaigns[j].Name })
	return campaigns, nil
}

// ResolveCampaignSources maps semantic campaign selectors onto the current
// device source inventory. A ROS 2 topic selector resolves to that topic's own
// source, so the episode records that topic and nothing else; the requested
// topics are still preserved in the manifest. Camera capture policies are
// returned keyed by resolved source ID so the capture adapter can honor them;
// ROS 2 and telemetry policies are not plumbed because those adapters
// implement only continuous capture (deployment warns).
func (m *Manager) ResolveCampaignSources(campaign Campaign) ([]string, []string, map[string]*SourceCapture, error) {
	all := m.Sources(context.Background())
	selected := map[string]bool{"applications": true}
	captures := map[string]*SourceCapture{}
	var topics []string
	for _, requested := range campaign.Sources {
		switch {
		case requested.Telemetry:
			selected["telemetry"] = true
		case requested.ROS2 != "":
			topics = append(topics, requested.ROS2)
			ids, err := resolveROS2Selector(all, requested.ROS2)
			if err != nil {
				return nil, nil, nil, err
			}
			for _, id := range ids {
				selected[id] = true
			}
		case requested.Camera != "":
			id, err := resolveCameraSelector(all, requested.Camera)
			if err != nil {
				return nil, nil, nil, err
			}
			selected[id] = true
			if requested.Capture != nil {
				captures[id] = requested.Capture
			}
		case requested.Audio != "":
			id, err := resolveKindSelector(all, "audio", requested.Audio)
			if err != nil {
				return nil, nil, nil, err
			}
			selected[id] = true
			if requested.Capture != nil {
				captures[id] = requested.Capture
			}
		}
	}
	ids := make([]string, 0, len(selected))
	for id := range selected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	sort.Strings(topics)
	return ids, topics, captures, nil
}

// resolveROS2Selector maps a campaign `ros2:` selector onto discovered source
// identifiers. Three spellings resolve, and every one of them has to keep
// working because campaign YAML is deployed to devices in the field.
//
//   - A topic name, "/lidar/points": the topic's own source on every healthy
//     graph that publishes it. This is the spelling the documentation has
//     always shown, and it is the one that used to silently select the whole
//     DDS domain, so a plan asking for the lidar recorded every camera and
//     every inertial measurement unit topic on the robot as well.
//   - A full per-topic identifier, "ros2:rmw_cyclonedds_cpp:domain-42:/scan":
//     that exact source, no graph search.
//   - Anything else, which includes the domain-level identifier a plan written
//     before per-topic sources existed names ("ros2:rmw_cyclonedds_cpp:domain-42",
//     with or without the "ros2:" prefix): the whole domain, still recorded
//     with `ros2 bag record -a`. An unrecognized selector selects every healthy
//     domain, which is exactly what this function did for every selector before
//     per-topic sources existed, so no deployed plan changes meaning except the
//     topic selectors, which now mean what they say.
//
// A topic selector that names a topic no healthy graph publishes is an error
// rather than a fallback to the whole domain. Falling back would resurrect the
// bug this addresses, and the alternative failure, a sealed and uploaded
// episode holding none of the data the plan asked for, is the one this package
// already refuses for audio sources.
func resolveROS2Selector(all []Source, selector string) ([]string, error) {
	healthyROS2 := func(source Source) bool { return source.Kind == "ros2" && source.Healthy }

	// A selector that parses as a source identifier names exactly one thing.
	// Accept it with or without the "ros2:" prefix, because a person copying a
	// domain out of `wendy data sources` may keep or drop it.
	candidates := []string{selector, ROS2SourcePrefix + selector}
	for _, candidate := range candidates {
		wantDomain, wantTopic, ok := ParseROS2SourceID(candidate)
		if !ok {
			continue
		}
		var ids []string
		for _, source := range all {
			if !healthyROS2(source) {
				continue
			}
			domainID, topic, parsed := ParseROS2SourceID(source.ID)
			if parsed && domainID == wantDomain && topic == wantTopic {
				ids = append(ids, source.ID)
			}
		}
		if len(ids) == 0 {
			return nil, fmt.Errorf("no healthy ROS 2 source %s is available", candidate)
		}
		return ids, nil
	}

	if strings.HasPrefix(selector, "/") {
		var ids []string
		for _, source := range all {
			if !healthyROS2(source) {
				continue
			}
			if _, topic, ok := ParseROS2SourceID(source.ID); ok && topic == selector {
				ids = append(ids, source.ID)
			}
		}
		if len(ids) == 0 {
			return nil, fmt.Errorf("no healthy ROS 2 graph publishes topic %s", selector)
		}
		return ids, nil
	}

	var ids []string
	for _, source := range all {
		if !healthyROS2(source) {
			continue
		}
		if _, topic, ok := ParseROS2SourceID(source.ID); ok && topic == "" {
			ids = append(ids, source.ID)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no healthy ROS 2 graph is available for %s", selector)
	}
	return ids, nil
}

// checkDeployableAudioSources refuses a campaign whose audio selector does not
// name a healthy source, at deploy rather than at the first trigger.
//
// Resolution used to happen only in triggerCampaign, so a plan naming a source
// that can never yield audio deployed cleanly and failed much later, or worse
// resolved to an endpoint that streams digital silence forever: a sealed,
// checksummed, uploaded episode full of nothing, with clean drop accounting and
// no error at any layer. Deploy is the last moment an operator is present to
// read the reason, so it is where the refusal belongs.
//
// It reuses resolveKindSelector so deploy and trigger cannot disagree about
// what resolves, and is scoped to audio: camera and ROS 2 selectors keep their
// existing deploy behavior.
func (m *Manager) checkDeployableAudioSources(campaign Campaign) error {
	var all []Source
	loaded := false
	for _, requested := range campaign.Sources {
		if requested.Audio == "" {
			continue
		}
		if !loaded {
			all, loaded = m.Sources(context.Background()), true
		}
		if _, err := resolveKindSelector(all, "audio", requested.Audio); err == nil {
			continue
		}
		// Name the reason, not just the miss: when the selector does match an
		// enumerated source that is merely unhealthy, its detail already says
		// why and what to check.
		if unhealthy, ok := matchUnhealthySource(all, "audio", requested.Audio); ok {
			return fmt.Errorf("audio source %q resolves to %s, which is unhealthy and would record silence: %s",
				requested.Audio, unhealthy.ID, unhealthy.Detail)
		}
		return fmt.Errorf("audio source %q does not name a healthy capture source on this device", requested.Audio)
	}
	return nil
}

// matchUnhealthySource finds an unhealthy source of the given kind that the
// selector names, so a refusal can quote the reason the source reported.
func matchUnhealthySource(all []Source, kind, selector string) (Source, bool) {
	selector = strings.TrimSpace(selector)
	for _, source := range all {
		if source.Kind != kind || source.Healthy {
			continue
		}
		if source.ID == selector || strings.EqualFold(source.ID, selector) {
			return source, true
		}
		if selector != "" && strings.Contains(strings.ToLower(source.Detail), strings.ToLower(selector)) {
			return source, true
		}
	}
	return Source{}, false
}

func resolveCameraSelector(all []Source, selector string) (string, error) {
	selector = strings.TrimSpace(selector)
	aliases := []string{selector}
	if strings.HasPrefix(selector, "/dev/video") {
		aliases = append(aliases, "v4l2:"+selector)
	}
	var healthy []Source
	for _, source := range all {
		if source.Kind != "camera" || !source.Healthy {
			continue
		}
		healthy = append(healthy, source)
		for _, alias := range aliases {
			if source.ID == alias || strings.EqualFold(source.ID, alias) {
				return source.ID, nil
			}
		}
	}
	var matches []string
	for _, source := range healthy {
		if strings.Contains(strings.ToLower(source.Detail), strings.ToLower(selector)) {
			matches = append(matches, source.ID)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("camera selector %q is ambiguous: %s", selector, strings.Join(matches, ", "))
	}
	if len(healthy) == 1 && (selector == "front" || selector == "default") {
		return healthy[0].ID, nil
	}
	return "", fmt.Errorf("no healthy camera matches %q", selector)
}

// resolveKindSelector maps a campaign selector onto a healthy source of the
// given kind by exact ID, then by a case-insensitive substring of the source
// detail, and finally accepts "default" when exactly one healthy source of the
// kind exists. It mirrors resolveCameraSelector for non-camera kinds such as
// audio, which have no /dev alias.
func resolveKindSelector(all []Source, kind, selector string) (string, error) {
	selector = strings.TrimSpace(selector)
	var healthy []Source
	for _, source := range all {
		if source.Kind != kind || !source.Healthy {
			continue
		}
		healthy = append(healthy, source)
		if source.ID == selector || strings.EqualFold(source.ID, selector) {
			return source.ID, nil
		}
	}
	var matches []string
	for _, source := range healthy {
		if selector != "" && strings.Contains(strings.ToLower(source.Detail), strings.ToLower(selector)) {
			matches = append(matches, source.ID)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("%s selector %q is ambiguous: %s", kind, selector, strings.Join(matches, ", "))
	}
	if len(healthy) == 1 && (selector == "default" || selector == "front") {
		return healthy[0].ID, nil
	}
	return "", fmt.Errorf("no healthy %s matches %q", kind, selector)
}

func (c Campaign) Match(record ApplicationRecord) (string, string, bool) {
	for _, trigger := range c.Capture.Triggers {
		if trigger.Event != "" && record.Type == "event" && record.Name == trigger.Event {
			return "event:" + trigger.Event, "event=" + trigger.Event, true
		}
		if trigger.ModelUncertainty != "" && record.Type == "prediction" {
			value, ok := uncertaintyValue(record)
			if !ok {
				continue
			}
			op, threshold, _ := parseThreshold("model.uncertainty", trigger.ModelUncertainty)
			if compareThreshold(value, op, threshold) {
				return fmt.Sprintf("model_uncertainty:%g", value), "model.uncertainty " + trigger.ModelUncertainty, true
			}
		}
	}
	return "", "", false
}

func uncertaintyValue(record ApplicationRecord) (float64, bool) {
	if raw, ok := record.Attributes["uncertainty"]; ok {
		if value, ok := numericValue(raw); ok {
			return value, true
		}
	}
	return numericValue(record.Value)
}

func numericValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		value, err := typed.Float64()
		return value, err == nil
	default:
		return 0, false
	}
}

// parseThreshold parses an "<operator> <number>" expression for the named
// field. Value ranges are field dependent: model.uncertainty is a probability
// clamped to 0..1, while fields such as an audio level_db are legitimately
// negative and carry no fixed range.
func parseThreshold(field, expression string) (string, float64, error) {
	expression = strings.TrimSpace(expression)
	for _, operator := range []string{"<=", ">=", "==", "<", ">"} {
		if strings.HasPrefix(expression, operator) {
			value, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(expression, operator)), 64)
			if err != nil {
				return "", 0, fmt.Errorf("%s must compare with a number", field)
			}
			// strconv accepts NaN and infinities; a NaN threshold never fires
			// and an infinite one always or never fires. Reject both.
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return "", 0, fmt.Errorf("%s must compare with a finite number", field)
			}
			if field == "model.uncertainty" && (value < 0 || value > 1) {
				return "", 0, errors.New("model.uncertainty must compare with a number from 0 through 1")
			}
			return operator, value, nil
		}
	}
	return "", 0, fmt.Errorf("%s must begin with <, <=, ==, >=, or >", field)
}

// ParseFieldThreshold exposes the campaign threshold grammar to capture
// adapters in other packages, for example the audio adapter's "level_db"
// trigger. It returns the field name, comparison operator, and numeric value.
func ParseFieldThreshold(expression string) (string, string, float64, error) {
	return parseFieldThreshold(expression)
}

// CompareThreshold applies a parsed threshold operator to a measured value.
func CompareThreshold(value float64, operator string, threshold float64) bool {
	return compareThreshold(value, operator, threshold)
}

// parseFieldThreshold parses a "<field> <operator> <number>" expression such
// as "model.uncertainty > 0.9" or "level_db > -20".
func parseFieldThreshold(expression string) (string, string, float64, error) {
	expression = strings.TrimSpace(expression)
	split := strings.IndexAny(expression, "<>=")
	if split <= 0 {
		return "", "", 0, errors.New("threshold trigger must have the form \"<field> <operator> <number>\", for example \"model.uncertainty > 0.9\"")
	}
	field := strings.TrimSpace(expression[:split])
	for _, r := range field {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_') {
			return "", "", 0, fmt.Errorf("threshold trigger field %q must use only letters, numbers, '.' or '_'", field)
		}
	}
	operator, value, err := parseThreshold(field, expression[split:])
	if err != nil {
		return "", "", 0, err
	}
	return field, operator, value, nil
}

// validCaptureModes lists the campaign schema's per-source capture modes. The
// schema accepts them all; DeployCampaign reports which ones the installed
// adapters do not implement yet.
var validCaptureModes = []string{"continuous", "snapshot", "fragment", "threshold"}

// implementedCaptureModes is what capture adapters actually honor today, by
// campaign source kind. The camera adapter implements snapshot capture and the
// audio adapter implements threshold capture; the ROS 2 and telemetry paths
// still record continuously regardless of mode, so other modes deploy-warn for
// them.
var implementedCaptureModes = map[string]map[string]bool{
	"camera":    {"continuous": true, "snapshot": true},
	"audio":     {"continuous": true, "threshold": true},
	"ros2":      {"continuous": true},
	"telemetry": {"continuous": true},
}

func validateSourceCapture(source CampaignSource) error {
	capture := source.Capture
	if capture == nil {
		return nil
	}
	mode := capture.EffectiveMode()
	type field struct {
		name  string
		set   bool
		modes []string
	}
	fields := []field{
		{"interval", capture.Interval != "", []string{"snapshot"}},
		{"rate", capture.Rate != 0, []string{"continuous"}},
		{"pre", capture.Pre != "", []string{"fragment"}},
		{"post", capture.Post != "", []string{"fragment"}},
		{"trigger", capture.Trigger != "", []string{"threshold"}},
		{"fragment", capture.Fragment != "", []string{"threshold"}},
	}
	valid := false
	for _, candidate := range validCaptureModes {
		valid = valid || candidate == mode
	}
	if !valid {
		return fmt.Errorf("mode must be one of %s", strings.Join(validCaptureModes, ", "))
	}
	for _, f := range fields {
		if !f.set {
			continue
		}
		allowed := false
		for _, m := range f.modes {
			allowed = allowed || m == mode
		}
		if !allowed {
			return fmt.Errorf("%s applies only to %s mode, not %s mode", f.name, strings.Join(f.modes, "/"), mode)
		}
	}
	switch mode {
	case "continuous":
		if capture.Rate < 0 {
			return errors.New("rate must be a positive capture frequency in hertz")
		}
	case "snapshot":
		interval, err := time.ParseDuration(capture.Interval)
		if capture.Interval == "" || err != nil || interval <= 0 {
			return errors.New("snapshot mode requires a positive interval duration")
		}
	case "fragment":
		if capture.Pre == "" && capture.Post == "" {
			return errors.New("fragment mode requires pre and/or post durations")
		}
		for name, raw := range map[string]string{"pre": capture.Pre, "post": capture.Post} {
			if raw == "" {
				continue
			}
			if d, err := time.ParseDuration(raw); err != nil || d < 0 {
				return fmt.Errorf("%s must be a non-negative duration", name)
			}
		}
	case "threshold":
		if capture.Trigger == "" {
			return errors.New("threshold mode requires a trigger expression")
		}
		if _, _, _, err := parseFieldThreshold(capture.Trigger); err != nil {
			return fmt.Errorf("trigger: %w", err)
		}
		if capture.Fragment != "" {
			if d, err := time.ParseDuration(capture.Fragment); err != nil || d <= 0 {
				return errors.New("fragment must be a positive duration")
			}
		}
	}
	if capture.MaxResolution != "" {
		if source.Camera == "" {
			return errors.New("max_resolution applies only to camera sources")
		}
		if err := validateResolution(capture.MaxResolution); err != nil {
			return err
		}
	}
	return nil
}

func validateResolution(resolution string) error {
	width, height, found := strings.Cut(resolution, "x")
	w, errW := strconv.Atoi(width)
	h, errH := strconv.Atoi(height)
	if !found || errW != nil || errH != nil || w <= 0 || h <= 0 {
		return fmt.Errorf("max_resolution %q must be WxH, for example 1280x720", resolution)
	}
	return nil
}

// parseByteSize parses a byte count. Plain integers are bytes; decimal (KB,
// MB, GB, TB) and binary (KiB, MiB, GiB, TiB) suffixes follow the convention
// used elsewhere in this repository.
func parseByteSize(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	cut := len(s)
	for cut > 0 {
		r := s[cut-1]
		if r >= '0' && r <= '9' || r == '.' {
			break
		}
		cut--
	}
	number := strings.TrimSpace(s[:cut])
	unit := strings.TrimSpace(s[cut:])
	value, err := strconv.ParseFloat(number, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%q is not a byte size; use bytes or a unit such as 500MB or 10GiB", raw)
	}
	multiplier := float64(1)
	switch strings.ToLower(unit) {
	case "", "b":
	case "kb":
		multiplier = 1e3
	case "kib":
		multiplier = 1 << 10
	case "mb":
		multiplier = 1e6
	case "mib":
		multiplier = 1 << 20
	case "gb":
		multiplier = 1e9
	case "gib":
		multiplier = 1 << 30
	case "tb":
		multiplier = 1e12
	case "tib":
		multiplier = 1 << 40
	default:
		return 0, fmt.Errorf("%q is not a byte size; use bytes or a unit such as 500MB or 10GiB", raw)
	}
	total := value * multiplier
	if total <= 0 {
		return 0, fmt.Errorf("%q must be a positive byte size", raw)
	}
	// Converting a float beyond int64 range is implementation-defined in Go;
	// reject instead of storing a garbage (possibly negative) byte count.
	if total >= float64(math.MaxInt64) {
		return 0, fmt.Errorf("%q is too large for a byte size", raw)
	}
	return int64(total), nil
}

// parseByteRate parses a bytes-per-second rate such as "5MB/s", "5MB", or a
// plain integer byte count per second.
func parseByteRate(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, "/s")
	return parseByteSize(s)
}

// UploadMaxRateBytes returns the validated upload bandwidth cap in bytes per
// second, or 0 when unlimited.
func (c Campaign) UploadMaxRateBytes() int64 {
	if c.Upload.MaxRate == "" {
		return 0
	}
	rate, _ := parseByteRate(c.Upload.MaxRate)
	return rate
}

// LocalQuotaBytes returns the validated on-device retention quota in bytes,
// or 0 when the device-wide default applies.
func (c Campaign) LocalQuotaBytes() int64 {
	if c.Retention.LocalQuota == "" {
		return 0
	}
	quota, _ := parseByteSize(c.Retention.LocalQuota)
	return quota
}

func compareThreshold(value float64, operator string, threshold float64) bool {
	switch operator {
	case "<":
		return value < threshold
	case "<=":
		return value <= threshold
	case "==":
		return value == threshold
	case ">=":
		return value >= threshold
	case ">":
		return value > threshold
	default:
		return false
	}
}
