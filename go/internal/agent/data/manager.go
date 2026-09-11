package data

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/timesync"
	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
	"golang.org/x/sys/unix"
)

const DefaultRoot = "/var/lib/wendy-agent/data/episodes"

const (
	preRollWindow = 5 * time.Minute
	preRollLimit  = 50 << 20
	// DefaultMaxQuotaBytes caps the episode store, and DefaultReserveBytes is
	// the free space eviction keeps available on the store's filesystem. The
	// quota itself is a fifth of the filesystem bounded by this cap, so both
	// are defaults rather than device-specific numbers; SetQuota overrides
	// them where a device wants a different bound.
	DefaultMaxQuotaBytes = int64(50 << 30)
	DefaultReserveBytes  = int64(5 << 30)
	// DefaultSealDrain is how long an episode that captures applications stays
	// open for late application records after its capture adapters have shut
	// down. An application that scores asynchronously writes its prediction
	// after the samples it read, so without a drain that write lands past the
	// seal and is filed against a later episode. The manager itself applies no
	// default: the value arrives through StartOptions.DrainDuration, and the
	// policy of using this default lives at the service call sites.
	DefaultSealDrain = 2 * time.Second
	// maxSealDrain bounds the configurable drain. Beyond this the wait stops
	// being a seal detail and becomes an unannounced extension of the episode.
	maxSealDrain = 30 * time.Second
)

var ErrNoActiveEpisode = errors.New("no active episode")

// The following sentinels classify request-shaped failures so RPC handlers can
// map them to precise gRPC codes (InvalidArgument for a malformed request,
// FailedPrecondition for a request that is well formed but not currently
// serviceable) instead of collapsing every failure to NotFound. Genuine
// absence still surfaces as os.ErrNotExist and read/seal failures surface as
// their underlying I/O error.
var (
	// ErrInvalidEpisodeID marks a syntactically invalid episode identifier.
	ErrInvalidEpisodeID = errors.New("invalid episode id")
	// ErrInvalidDownloadOffset marks an out-of-range download offset.
	ErrInvalidDownloadOffset = errors.New("invalid download offset")
	// ErrInvalidEpisodePath marks a malformed episode-relative file path.
	ErrInvalidEpisodePath = errors.New("invalid episode file path")
	// ErrEpisodePathEscapes marks a path that resolves outside the episode root.
	ErrEpisodePathEscapes = errors.New("episode path escapes root")
	// ErrEpisodeEntryNotRegular marks a manifest entry that is not a regular
	// file on disk; the episode exists but the entry cannot be served.
	ErrEpisodeEntryNotRegular = errors.New("episode entry is not a regular file")
)

// AdHocEpisodeKey is the reserved concurrency key for episodes started
// without a campaign (for example `wendy data record`). Each campaign may run
// one active episode at a time, and one ad-hoc episode may run beside them.
const AdHocEpisodeKey = ""

type Manager struct {
	mu sync.Mutex
	// campaignMu serializes campaign plan writes. It is separate from mu
	// because deploying a plan touches no episode state and must not queue
	// behind (or block) recording, and because DeployCampaign resolves audio
	// sources through Sources, which takes mu itself.
	campaignMu sync.Mutex
	root       string
	active     map[string]*activeEpisode
	// sealing holds episodes that have stopped capturing and are inside their
	// post-seal drain. They still receive application records, but they no
	// longer hold their campaign key: an episode whose cameras are already off
	// must not make its campaign look busy to the trigger path, or a detection
	// arriving during the drain is filed into a recording that captured
	// nothing of it and no new episode is ever started for it. Ordering is
	// irrelevant, so a slice is enough; a campaign can have several sealing
	// episodes at once only if its drain outlasts its next episode.
	sealing      []*activeEpisode
	consensus    func(context.Context) (timesync.Consensus, error)
	preRoll      []bufferedRecord
	preRollBytes int
	// preRollEvicted holds the agent receipt of every record the ring dropped
	// while it was still inside its window, which happens only when the ring
	// hits its byte budget. Records that simply aged out are not recorded here:
	// they left because they were no longer pre-roll for anything, which is the
	// ring working, not losing.
	//
	// Timestamps rather than a counter, because an episode's pre_roll_lost is
	// supposed to name what THAT episode's window lost. A cumulative counter
	// reported every episode's number as the agent's total since boot, which
	// grows forever and is never that episode's loss.
	preRollEvicted []int64
	downloads      map[string]int
	sourceProvider func(context.Context) []Source
	appObserver    func(string, ApplicationRecord)
	warn           func(string)
	// deferredWarnings holds warnings raised before a logger was attached.
	// NewManager recovers crash-interrupted episodes, and quarantining or
	// deleting one is precisely the kind of event an operator must see, but
	// the agent can only call SetWarnLogger once NewManager has returned. They
	// are replayed there rather than discarded, and capped so a store full of
	// damaged partials cannot grow this without bound.
	deferredWarnings []string
	// maxQuota and reserve bound the episode store. They default to
	// DefaultMaxQuotaBytes and DefaultReserveBytes and are overridden through
	// SetQuota, which is how the agent applies its configuration and how the
	// eviction tests drive a store small enough to evict deterministically.
	maxQuota int64
	reserve  int64
}

// SetQuota overrides the episode store bounds. maxQuota caps the store (the
// enforced quota is still the smaller of it and a fifth of the filesystem) and
// reserve is the free space eviction preserves on that filesystem. A
// non-positive maxQuota keeps the default; reserve is applied as given, so
// zero really does mean "keep no headroom".
func (m *Manager) SetQuota(maxQuota, reserve int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if maxQuota > 0 {
		m.maxQuota = maxQuota
	}
	if reserve >= 0 {
		m.reserve = reserve
	}
}

// SetWarnLogger routes operational warnings (for example evicting an episode
// that has not been uploaded yet) to the agent's logger.
func (m *Manager) SetWarnLogger(warn func(string)) {
	m.mu.Lock()
	m.warn = warn
	deferred := m.deferredWarnings
	m.deferredWarnings = nil
	m.mu.Unlock()
	if warn == nil {
		return
	}
	for _, message := range deferred {
		warn(message)
	}
}

// maxDeferredWarnings caps the pre-logger warning buffer.
const maxDeferredWarnings = 64

// Warnf routes an operational warning through the manager's configured logger.
// Unlike the internal warnf it takes the lock, so it is safe to call from a
// goroutine that holds none.
func (m *Manager) Warnf(format string, args ...any) {
	m.mu.Lock()
	warn := m.warn
	m.mu.Unlock()
	if warn != nil {
		warn(fmt.Sprintf(format, args...))
	}
}

func (m *Manager) warnf(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	if m.warn == nil {
		if len(m.deferredWarnings) < maxDeferredWarnings {
			m.deferredWarnings = append(m.deferredWarnings, message)
		}
		return
	}
	m.warn(message)
}

// SetSourceProvider adds device-backed sources discovered by capture adapters.
// The built-in application and telemetry sources are always retained.
func (m *Manager) SetSourceProvider(provider func(context.Context) []Source) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sourceProvider = provider
}

// Sources returns a fresh, stable snapshot of all built-in and adapter sources.
func (m *Manager) Sources(ctx context.Context) []Source {
	m.mu.Lock()
	provider := m.sourceProvider
	m.mu.Unlock()
	out := DiscoverSources()
	if provider != nil {
		out = append(out, provider(ctx)...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// sourceDiscoveryTimeout bounds the source provider on the episode start path.
// Discovery is out-of-process for some adapters (the ROS 2 one shells out to
// `ros2 topic list -t` inside a container), and an adapter that hangs must
// cost a starting episode a bounded delay rather than blocking it forever.
// Sources the provider misses inside the budget simply cannot be selected,
// which surfaces as the ordinary "unknown or unhealthy source" refusal.
const sourceDiscoveryTimeout = 3 * time.Second

// discoverSources snapshots the built-in and adapter sources for an episode
// start. It MUST be called without m.mu held: the provider is foreign code
// that may call back into the manager.
func (m *Manager) discoverSources() []Source {
	m.mu.Lock()
	provider := m.sourceProvider
	m.mu.Unlock()
	out := DiscoverSources()
	if provider == nil {
		return out
	}
	ctx, cancel := context.WithTimeout(context.Background(), sourceDiscoveryTimeout)
	defer cancel()
	return append(out, provider(ctx)...)
}

type bufferedRecord struct {
	// bootNanos is the record's presentation stamp: the client's own
	// CLOCK_BOOTTIME value when it was accepted, otherwise the agent receipt.
	// It is what the pre-roll offset is derived from, and a client can move it.
	bootNanos int64
	// receiptNanos is the agent-authoritative CLOCK_BOOTTIME reading taken when
	// the record arrived. No client can influence it, so it is the timestamp
	// that decides ring eviction and pre-roll window membership.
	receiptNanos int64
	encoded      []byte
}

type ApplicationRecord struct {
	Version    int            `json:"version"`
	Type       string         `json:"type"`
	Name       string         `json:"name,omitempty"`
	Model      string         `json:"model,omitempty"`
	Value      any            `json:"value,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
	// Inputs binds this record to the harness samples it was computed from, by
	// the same (source_id, sample_id) pair the app read from the agent-fed
	// node. It is optional: a record that names no inputs is
	// accepted exactly as before, and is counted as an outcome whose input is
	// unknown rather than being rejected.
	Inputs          []SampleRef `json:"inputs,omitempty"`
	ClientBootNanos int64       `json:"client_boottime_nanos"`
	ClientBootID    string      `json:"boot_id"`
}

type storedApplicationRecord struct {
	ApplicationRecord
	AppID                     string `json:"app_id"`
	EpisodeNanos              int64  `json:"episode_nanos"`
	TimestampUncertaintyNanos int64  `json:"timestamp_uncertainty_nanos"`
	AgentReceiptBootNanos     int64  `json:"agent_receipt_boottime_nanos"`
	ClientTimestampAccepted   bool   `json:"client_timestamp_accepted"`
	// PrerollFlushed marks a record this episode received from the pre-roll ring
	// rather than live. It is provenance, not a defect: a pre-trigger record is
	// legitimately replayed into an episode that started after it arrived. The
	// key is absent on live-recorded lines, so those stay byte-identical.
	PrerollFlushed bool `json:"preroll_flushed,omitempty"`
}

// SetConsensusProvider configures a fresh direct observation at episode start
// and finalization. Observation never changes CLOCK_BOOTTIME timestamps.
func (m *Manager) SetConsensusProvider(provider func(context.Context) (timesync.Consensus, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.consensus = provider
}

type activeEpisode struct {
	key      string
	dir      string
	manifest Manifest
	cancel   context.CancelFunc
	done     chan struct{}
	// capturesApplications reports whether the applications source was
	// selected; episodes that excluded it receive no application records.
	capturesApplications bool
	// drain is how long this episode stays open for records after its capture
	// adapters have stopped, so that a record an application writes about the
	// samples it just read is still filed into this episode instead of the
	// next one. Zero or less means no drain, and it is honoured only for
	// episodes that capture applications.
	drain time.Duration
	// draining reports that the episode has left m.active for m.sealing: its
	// capture adapters have stopped, it still accepts application records for
	// the rest of its drain, and it no longer holds its campaign key. Written
	// and read under m.mu.
	draining bool
	// modelInputs is the append handle for the model-input ledger, opened on
	// the first sample a model consumed (see openModelInputLedger). It is not
	// fsynced per sample: at sensor rates that would dominate the write path,
	// and a torn tail is recovered exactly like every other episode JSONL.
	modelInputs *os.File
	// awaitConsensus reports that the episode's opening Roughtime consensus was
	// moved off the start path and is still to be attached (see Start).
	awaitConsensus bool
	// telemetryLost counts consecutive telemetry rows the 1 Hz sampler could
	// not write, and telemetryWarnAt rate-limits the warning about them. Both
	// are touched only by the sampler goroutine, under m.mu.
	telemetryLost   uint64
	telemetryWarnAt time.Time
}

// telemetryWarnInterval bounds how often a failing telemetry write warns. A
// disk that is full fails every second; one line per second in the agent log
// buries everything else and is no more informative than one per minute.
const telemetryWarnInterval = time.Minute

// noteTelemetryDropLocked records one lost telemetry row on the episode's
// telemetry source. Callers hold m.mu.
func (a *activeEpisode) noteTelemetryDropLocked() {
	for i := range a.manifest.Sources {
		if a.manifest.Sources[i].Source.ID != "telemetry" {
			continue
		}
		drops := uint64(1)
		if a.manifest.Sources[i].Drops != nil {
			drops = *a.manifest.Sources[i].Drops + 1
		}
		a.manifest.Sources[i].Drops = &drops
		a.manifest.Sources[i].DropAccounting = "exact"
		return
	}
}

// consensusQueryTimeout bounds one Roughtime consensus query. It is the budget
// for the slowest server in the pool, so an unreachable server costs this long.
const consensusQueryTimeout = 5 * time.Second

func NewManager(root string) (*Manager, error) {
	implicitRoot := root == ""
	if root == "" {
		root = DefaultRoot
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		if !implicitRoot || !errors.Is(err, os.ErrPermission) {
			return nil, err
		}
		userData, fallbackErr := os.UserConfigDir()
		if fallbackErr != nil {
			return nil, errors.Join(err, fallbackErr)
		}
		root = filepath.Join(userData, "wendy-agent", "data", "episodes")
		if fallbackErr = os.MkdirAll(root, 0o750); fallbackErr != nil {
			return nil, errors.Join(err, fallbackErr)
		}
	}
	m := &Manager{root: root, downloads: make(map[string]int), active: make(map[string]*activeEpisode),
		maxQuota: DefaultMaxQuotaBytes, reserve: DefaultReserveBytes}
	if err := m.recoverPartials(); err != nil {
		return nil, err
	}
	return m, nil
}

// BootID is the kernel boot identity the episode timeline belongs to. Callers
// outside the package stamp it onto samples so a consumer can tell that a
// CLOCK_BOOTTIME value belongs to this boot and not a previous one.
func BootID() string { return bootID() }

func bootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "unavailable"
	}
	return strings.TrimSpace(string(b))
}

// readBootIDForAcceptance is the boot identity RecordApplication checks a
// client's claimed one against. It is a variable so a test can present both a
// healthy agent identity and an unavailable one without a real /proc.
var readBootIDForAcceptance = bootID

func deviceIdentity(currentBootID string) DeviceIdentity {
	hostname, _ := os.Hostname()
	id := "unavailable"
	for _, path := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) != "" {
			id = strings.TrimSpace(string(b))
			break
		}
	}
	return DeviceIdentity{ID: id, Hostname: hostname, BootID: currentBootID}
}

func newID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:]), nil
}

func observeUTC(origin int64, _ string, source string) (UTCObservation, error) {
	s, err := sandwichUTC()
	if err != nil {
		return UTCObservation{}, err
	}
	lo := s.TargetNanos - s.BootAfterNanos
	hi := s.TargetNanos - s.BootBeforeNanos
	reported, reportedConfidence := systemClockUncertainty()
	if reportedConfidence == "unbounded" {
		return UTCObservation{EpisodeNanos: s.BootBeforeNanos + (s.BootAfterNanos-s.BootBeforeNanos)/2 - origin, Confidence: "unbounded", EvidenceSource: source, Algorithm: ClockAlgorithm, ObservedUnixNano: time.Now().UnixNano(), UncertaintyNanos: reported, Sample: s}, nil
	}
	lo -= reported
	hi += reported
	return UTCObservation{
		EpisodeNanos:     s.BootBeforeNanos + (s.BootAfterNanos-s.BootBeforeNanos)/2 - origin,
		OffsetLowerNanos: lo, OffsetUpperNanos: hi,
		OffsetMidNanos: lo + (hi-lo)/2, UncertaintyNanos: (hi - lo + 1) / 2,
		Confidence: reportedConfidence, EvidenceSource: source, Algorithm: ClockAlgorithm,
		ObservedUnixNano: time.Now().UnixNano(), Sample: s,
	}, nil
}

// Start begins one episode. Concurrency is keyed per campaign: each campaign
// may record one active episode at a time, and one ad-hoc (campaign-less)
// episode may record beside them.
func (m *Manager) Start(opts StartOptions) (Manifest, error) {
	key := opts.Trigger.CampaignName
	// Two things happen before the lock is taken, both of which used to happen
	// under it. Scanning the store walks every file of every sealed episode,
	// and the source provider is an out-of-process question: the ROS 2 adapter
	// answers it by running `ros2 topic list -t` in a container. Holding m.mu
	// across either stalled every record, status query and adapter callback on
	// the device; holding it across the provider could also deadlock outright,
	// because a provider is entitled to call Warnf, Status or ActiveSession,
	// all of which take the same mutex. Sources() has always called the
	// provider without the lock for exactly this reason.
	scan, err := scanEpisodeStore(m.root)
	if err != nil {
		return Manifest{}, err
	}
	discovered := m.discoverSources()

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active[key] != nil {
		if key == AdHocEpisodeKey {
			return Manifest{}, errors.New("an episode is already active")
		}
		return Manifest{}, fmt.Errorf("an episode is already active for campaign %s", key)
	}
	if err := m.enforceQuotaLocked(scan); err != nil {
		return Manifest{}, err
	}
	origin, err := readBootTime()
	if err != nil {
		return Manifest{}, err
	}
	obs, err := observeUTC(origin, "system_reported", "linux_realtime_sandwich")
	if err != nil {
		return Manifest{}, err
	}
	// The Roughtime consensus is a network round trip to several public servers,
	// and an unreachable server is only known once its 5s timeout expires. On
	// the critical path it postpones every capture adapter by that timeout, and
	// the camera cannot record what happened while it waited. It is therefore
	// queried in the background and attached to the episode when it lands
	// (deferConsensus), exactly as the in-episode clock sampler already does.
	//
	// The one case that still has to block is an episode whose caller demanded a
	// UTC uncertainty bound: that gate can reject the episode, so its evidence
	// must exist before recording begins.
	deferConsensus := m.consensus != nil && opts.RequireUTCUncertainty <= 0
	var consensus *timesync.Consensus
	if m.consensus != nil && !deferConsensus {
		ctx, cancel := context.WithTimeout(context.Background(), consensusQueryTimeout)
		c, queryErr := m.consensus(ctx)
		cancel()
		if queryErr == nil {
			consensus = &c
		}
	}
	bestUncertainty := obs.UncertaintyNanos
	if consensus != nil && consensus.Confidence != "unbounded" {
		bestUncertainty = (consensus.UpperOffsetNanos - consensus.LowerOffsetNanos + 1) / 2
	}
	if opts.RequireUTCUncertainty > 0 && time.Duration(bestUncertainty) > opts.RequireUTCUncertainty {
		return Manifest{}, fmt.Errorf("UTC uncertainty %s does not satisfy required bound %s", time.Duration(bestUncertainty), opts.RequireUTCUncertainty)
	}
	id, err := newID()
	if err != nil {
		return Manifest{}, err
	}
	dir := filepath.Join(m.root, id+".partial")
	if err := os.Mkdir(dir, 0o750); err != nil {
		return Manifest{}, err
	}
	selected, err := selectSources(discovered, opts.Sources, opts.ExcludeSources)
	if err != nil {
		_ = os.Remove(dir)
		return Manifest{}, err
	}
	for i := range selected {
		if capture := opts.SourceCaptures[selected[i].ID]; capture != nil {
			selected[i].Capture = capture
		}
	}
	currentBootID := bootID()
	trigger := opts.Trigger
	if trigger.Reason == "" {
		trigger.Reason = "manual"
	}
	collectorVersion := opts.CollectorVersion
	if collectorVersion == "" {
		collectorVersion = "unknown"
	}
	upload := opts.Upload
	if upload.State == "" {
		upload.State = "local"
	}
	labeling := opts.Labeling
	if labeling.State == "" {
		labeling.State = "unlabeled"
	}
	privacy := append([]PrivacyTransformation(nil), opts.Privacy...)
	if privacy == nil {
		privacy = []PrivacyTransformation{}
	}
	modelVersions := make(map[string]string, len(opts.ModelVersions))
	for model, modelVersion := range opts.ModelVersions {
		modelVersions[model] = modelVersion
	}
	requestedTopics := append([]string(nil), opts.RequestedTopics...)
	if requestedTopics == nil {
		requestedTopics = []string{}
	}
	manifest := Manifest{Version: ManifestVersion, ID: id, Name: opts.Name, State: "recording", Device: deviceIdentity(currentBootID), CanonicalClock: "CLOCK_BOOTTIME", BootID: currentBootID, RequestBootNanos: origin, StartedUnixNanos: time.Now().UnixNano(), Trigger: trigger, CollectorVersion: collectorVersion, ModelVersions: modelVersions, RequestedTopics: requestedTopics, UTCObservations: []UTCObservation{obs}, PreRollAccounting: "exact", SystemClockStatus: "system_reported", Calibrations: []Calibration{}, Privacy: privacy, Upload: upload, Labeling: labeling, Files: []File{}, ModelIO: newModelIO()}
	if consensus != nil {
		recordConsensus(dir, &manifest, *consensus)
		manifest.SystemClockStatus = clockAgreement(obs, *consensus)
	}
	for _, s := range selected {
		requestedOffset := int64(0)
		if opts.PreRollDuration > 0 {
			requestedOffset = -opts.PreRollDuration.Nanoseconds()
		} else if s.ID == "applications" {
			requestedOffset = -preRollWindow.Nanoseconds()
		}
		manifest.Sources = append(manifest.Sources, SourceStats{Source: s, RequestedOffset: requestedOffset, DropAccounting: "unavailable"})
	}
	for source, contents := range opts.Calibrations {
		name := safeName(source) + ".calibration"
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, contents, 0o640); err != nil {
			_ = os.RemoveAll(dir)
			return Manifest{}, err
		}
		h := sha256.Sum256(contents)
		manifest.Calibrations = append(manifest.Calibrations, Calibration{Source: source, Revision: opts.CalibrationRevisions[source], Path: name, SHA256: hex.EncodeToString(h[:])})
	}
	for source, revision := range opts.CalibrationRevisions {
		if _, attached := opts.Calibrations[source]; !attached {
			manifest.Calibrations = append(manifest.Calibrations, Calibration{Source: source, Revision: revision})
		}
	}
	capturesApplications := false
	for _, source := range selected {
		capturesApplications = capturesApplications || source.ID == "applications"
	}
	if capturesApplications {
		if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), nil, 0o640); err != nil {
			_ = os.RemoveAll(dir)
			return Manifest{}, err
		}
	}
	for _, source := range selected {
		if source.ID == "telemetry" {
			if err := os.WriteFile(filepath.Join(dir, "telemetry.jsonl"), nil, 0o640); err != nil {
				_ = os.RemoveAll(dir)
				return Manifest{}, err
			}
			break
		}
	}
	if capturesApplications {
		manifest.PreRollLost = m.preRollLostInWindowLocked(origin, opts.PreRollDuration)
		preRollCount, earliestPreRoll, err := m.flushPreRoll(dir, origin, opts.PreRollDuration)
		if err != nil {
			_ = os.RemoveAll(dir)
			return Manifest{}, err
		}
		for i := range manifest.Sources {
			if manifest.Sources[i].Source.ID != "applications" {
				continue
			}
			manifest.Sources[i].Count += preRollCount
			manifest.Sources[i].DropAccounting = "exact"
			if earliestPreRoll != nil {
				manifest.Sources[i].ActualOffset = *earliestPreRoll
			}
		}
	}
	if err := writeManifest(dir, manifest); err != nil {
		_ = os.RemoveAll(dir)
		return Manifest{}, err
	}
	a := &activeEpisode{key: key, dir: dir, manifest: manifest, capturesApplications: capturesApplications, drain: opts.DrainDuration, awaitConsensus: deferConsensus}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.done = make(chan struct{})
	go m.sampleEpisode(ctx, a)
	m.active[key] = a
	return snapshotManifest(manifest), nil
}

// snapshotManifest returns a copy whose Sources slice does not share a
// backing array with the live episode manifest, which the manager keeps
// mutating (source counters, adapter results) under its lock while callers
// read the returned value without it.
func snapshotManifest(v Manifest) Manifest {
	v.Sources = append([]SourceStats(nil), v.Sources...)
	// Model-input accounting is reached through a pointer and through a slice
	// the manager keeps appending to, so copying the Sources slice alone would
	// still leave the caller reading live state.
	for i := range v.Sources {
		if v.Sources[i].ModelInputs != nil {
			stats := *v.Sources[i].ModelInputs
			v.Sources[i].ModelInputs = &stats
		}
	}
	v.ModelIO.Uncaptured = append([]SourceModelInputs(nil), v.ModelIO.Uncaptured...)
	return v
}

// SetApplicationObserver receives validated entitled application records after
// they have been durably buffered or recorded. It is used to arm campaign
// triggers without granting applications access to the administrative socket.
func (m *Manager) SetApplicationObserver(observer func(string, ApplicationRecord)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appObserver = observer
}

// ActiveSession returns the immutable filesystem and clock context for capture
// adapters for the episode keyed by the given campaign name (AdHocEpisodeKey
// for campaign-less episodes). It is valid only while that episode is recording.
func (m *Manager) ActiveSession(key string) (CaptureSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.active[key]
	if a == nil {
		return CaptureSession{}, false
	}
	return CaptureSession{ID: a.manifest.ID, Directory: a.dir, RequestBootNanos: a.manifest.RequestBootNanos, BootID: a.manifest.BootID, CampaignKey: a.key}, true
}

// ActiveEpisodeKeys returns the sorted concurrency keys of episodes that are
// still capturing. AdHocEpisodeKey marks a campaign-less episode; every other
// key is a campaign name.
//
// An episode inside its post-seal drain is deliberately absent: it holds no key
// any more, its capture adapters have stopped, and the next episode for that
// campaign may start while it is still accepting late records. Callers that
// mean "is this campaign busy" want exactly this list; callers that mean "which
// episodes can still receive a record" want the drain window as well, and that
// question is answered inside RecordApplication rather than here.
func (m *Manager) ActiveEpisodeKeys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.active))
	for key := range m.active {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// ApplyCaptureResults merges final adapter counters and mapping summaries before
// sealing. Unknown drops remain absent rather than being rendered as zero.
func (m *Manager) ApplyCaptureResults(key string, results []CaptureResult) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.active[key]
	if a == nil {
		return ErrNoActiveEpisode
	}
	for _, result := range results {
		for i := range a.manifest.Sources {
			stats := &a.manifest.Sources[i]
			if stats.Source.ID != result.SourceID {
				continue
			}
			if result.ClockDomain != "" {
				stats.Source.ClockDomain = result.ClockDomain
			}
			if result.SourceDetail != "" {
				stats.Source.Detail = result.SourceDetail
			}
			if result.ActualOffset != nil {
				stats.ActualOffset = *result.ActualOffset
			}
			stats.Count = result.Count
			stats.Drops = result.Drops
			if result.DropAccounting != "" {
				stats.DropAccounting = result.DropAccounting
			}
			stats.MappingError = result.MappingError
			stats.Discontinuities = result.Discontinuities
			stats.Mappings = append([]ClockMapping(nil), result.Mappings...)
		}
	}
	return writeManifest(a.dir, a.manifest)
}

// Interrupt finalizes an active episode that ended badly. Existing monotonic
// data is retained for auditability and is never silently deleted. An
// interrupted episode drains for late application records exactly as a stopped
// one does: the records an application still owes are no less its own for the
// episode having ended badly.
func (m *Manager) Interrupt(key, reason string) (Manifest, error) {
	return m.interrupt(key, reason, true)
}

// InterruptWithoutDrain finalizes an episode that never began capturing, and
// returns immediately instead of serving its post-seal drain.
//
// The drain buys exactly one thing: time for an application to file a record
// about the samples it read from this episode. An episode whose capture
// adapters failed to start delivered no samples to anyone, so there is no such
// record outstanding and nothing to wait for. Paying the drain there is pure
// latency on a failing path, and the caller holds its own start/stop lock
// across it, so a flapping camera would stall every other start and stop on the
// device for one drain per attempt.
//
// Use this only where the absence of delivered samples is certain. Any episode
// that captured, however briefly, must go through Interrupt.
func (m *Manager) InterruptWithoutDrain(key, reason string) (Manifest, error) {
	return m.interrupt(key, reason, false)
}

func (m *Manager) interrupt(key, reason string, drain bool) (Manifest, error) {
	a, err := m.beginSeal(key, drain)
	if err != nil {
		return Manifest{}, err
	}
	return m.finalize(a, "interrupted", reason)
}

func (m *Manager) sampleEpisode(ctx context.Context, a *activeEpisode) {
	defer close(a.done)
	clockTicker := time.NewTicker(5 * time.Minute)
	defer clockTicker.Stop()
	if a.awaitConsensus {
		// Deliberately not waited on: the episode records while this runs, and
		// the manifest is rewritten under the manager lock when it lands.
		go m.queryAndAttachConsensus(ctx, a, true)
	}
	telemetryTicker := time.NewTicker(time.Second)
	defer telemetryTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-telemetryTicker.C:
			now, err := readBootTime()
			if err != nil {
				continue
			}
			sample := map[string]any{"episode_nanos": now - a.manifest.RequestBootNanos, "agent_receipt_boottime_nanos": now, "values": telemetryValues()}
			// A row lost to a full disk or an I/O error used to vanish
			// silently: the sampler took the next tick and the manifest went on
			// saying drop_accounting "unavailable", so an episode with an hour
			// of missing telemetry was indistinguishable from a complete one.
			// The loss is now counted on the telemetry source (which is what
			// drops and drop_accounting are for), carried into the next row
			// that does land, and warned about at a bounded rate.
			if lost := a.telemetryLost; lost > 0 {
				sample["rows_lost_before"] = lost
			}
			b, _ := json.Marshal(sample)
			err = appendJSONL(filepath.Join(a.dir, "telemetry.jsonl"), b)
			m.mu.Lock()
			if err != nil {
				a.telemetryLost++
				a.noteTelemetryDropLocked()
				warn := a.telemetryWarnAt.IsZero() || time.Since(a.telemetryWarnAt) >= telemetryWarnInterval
				if warn {
					a.telemetryWarnAt = time.Now()
					m.warnf("episode %s: writing a telemetry row failed (%v); %d row(s) lost so far and counted as telemetry drops", a.manifest.ID, err, a.telemetryLost)
				}
			} else {
				a.telemetryLost = 0
				if m.active[a.key] == a {
					for i := range a.manifest.Sources {
						if a.manifest.Sources[i].Source.ID == "telemetry" {
							a.manifest.Sources[i].Count++
						}
					}
				}
			}
			m.mu.Unlock()
		case <-clockTicker.C:
			m.queryAndAttachConsensus(ctx, a, false)
		}
	}
}

// queryAndAttachConsensus runs one Roughtime consensus query and records it on
// the episode, rewriting the manifest. opening marks the episode's first
// consensus, which also settles system_clock_status the way a blocking
// start-time query used to; later samples only append evidence.
func (m *Manager) queryAndAttachConsensus(ctx context.Context, a *activeEpisode, opening bool) {
	if m.consensus == nil {
		return
	}
	queryCtx, cancel := context.WithTimeout(ctx, consensusQueryTimeout)
	c, err := m.consensus(queryCtx)
	cancel()
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active[a.key] != a {
		return
	}
	recordConsensus(a.dir, &a.manifest, c)
	if opening {
		a.awaitConsensus = false
		if len(a.manifest.UTCObservations) > 0 {
			a.manifest.SystemClockStatus = clockAgreement(a.manifest.UTCObservations[0], c)
		}
	}
	_ = writeManifest(a.dir, a.manifest)
}

// evictionCandidate is one sealed or quarantined episode directory the quota
// may remove, with everything the decision needs already read off disk.
type evictionCandidate struct {
	path          string
	started, size int64
	id, campaign  string
	tier          int
	// quarantined marks a directory recovery could not read, kept under
	// <id>.unrecoverable. It carries no manifest, so it is described by its
	// directory name and evicted first.
	quarantined bool
}

// storeScan is one pass over the episode store: every directory's byte total
// and, for the sealed ones, the manifest fields eviction sorts on.
//
// It is deliberately a free function taking a root rather than a method taking
// the manager lock. Scanning means reading every sealed episode's manifest and
// walking every file in the store, which on a full device is thousands of stat
// calls, and Start used to do all of it with m.mu held: every application
// record, every capture-adapter callback and every status query on the device
// blocked behind one episode's start. The scan needs no manager state, so it
// runs before the lock is taken and only the decision is made under it.
type storeScan struct {
	used       int64
	candidates []evictionCandidate
	// partialBytes is what the in-flight .partial directories occupy. They are
	// counted toward the quota (they are real bytes on the same filesystem)
	// but are never eviction candidates: an episode that is still recording
	// must not have its directory pulled out from under it.
	partialBytes int64
	// readFailures names the directories whose manifest could not be read.
	readFailures []string
}

func dirBytes(dir string) int64 {
	var size int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, e error) error {
		if e == nil && !d.IsDir() {
			if info, x := d.Info(); x == nil {
				size += info.Size()
			}
		}
		return nil
	})
	return size
}

// unrecoverableSuffix marks a crash-interrupted episode directory that
// recovery could neither read nor repair. See quarantinePartial.
const unrecoverableSuffix = ".unrecoverable"

func scanEpisodeStore(root string) (storeScan, error) {
	var scan storeScan
	entries, err := os.ReadDir(root)
	if err != nil {
		return scan, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		size := dirBytes(dir)
		scan.used += size
		switch {
		case strings.HasSuffix(e.Name(), ".partial"):
			scan.partialBytes += size
			continue
		case strings.HasSuffix(e.Name(), unrecoverableSuffix):
			// A quarantined directory holds bytes that can never be read,
			// uploaded or labeled. Counting them without ever evicting them
			// would let one damaged episode wedge the quota permanently, so it
			// is the first thing to go and the warning says so.
			started := int64(0)
			if info, statErr := e.Info(); statErr == nil {
				started = info.ModTime().UnixNano()
			}
			scan.candidates = append(scan.candidates, evictionCandidate{
				path: dir, started: started, size: size,
				id: strings.TrimSuffix(e.Name(), unrecoverableSuffix), tier: 0, quarantined: true,
			})
			continue
		}
		mf, err := readManifest(dir)
		if err != nil {
			scan.readFailures = append(scan.readFailures, e.Name())
			continue
		}
		scan.candidates = append(scan.candidates, evictionCandidate{
			path: dir, started: mf.StartedUnixNanos, size: size,
			id: mf.ID, campaign: mf.Trigger.CampaignName, tier: evictionTier(mf.Upload.State),
		})
	}
	return scan, nil
}

// recordConsensus files one Roughtime consensus round: the full round, with
// its nonce and raw signed responses, is appended to the episode's evidence
// sidecar, and the manifest keeps only the bounds.
//
// The manifest is rewritten in full on every round. Keeping the raw evidence
// in it meant a day-long episode, which queries Roughtime every five minutes,
// rewrote a manifest that had grown by several kilobytes of base64 per round,
// and by the end the clock evidence was larger than everything else in the
// file put together. The sidecar is sealed and checksummed exactly like every
// other episode file, so nothing is lost: the bytes needed to independently
// reverify a round are still in the episode, they are just not in the file a
// consumer reads to find out when the episode started.
//
// The manifest keeps the first round and the most recent one, which are the
// two a consumer actually reads: the bound the episode opened with, and the
// bound it currently holds. RoughtimeRounds says how many the sidecar has.
func recordConsensus(dir string, mf *Manifest, c timesync.Consensus) {
	if err := appendRoughtimeEvidence(dir, c); err != nil {
		// The bounds still reach the manifest: losing the ability to reverify
		// a round is not a reason to also lose the round.
		mf.RecoveryActions = append(mf.RecoveryActions, "roughtime evidence sidecar write failed: "+err.Error())
	} else {
		mf.RoughtimeEvidenceLog = RoughtimeEvidenceFile
	}
	mf.RoughtimeRounds++
	bounds := c
	bounds.Evidence = nil
	switch len(mf.Roughtime) {
	case 0:
		mf.Roughtime = []timesync.Consensus{bounds}
	case 1:
		mf.Roughtime = append(mf.Roughtime, bounds)
	default:
		mf.Roughtime[len(mf.Roughtime)-1] = bounds
	}
}

func appendRoughtimeEvidence(dir string, c timesync.Consensus) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, RoughtimeEvidenceFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return syncFile(f)
}

// enforceQuotaLocked evicts against an already-taken store scan. Callers hold
// m.mu; the scan itself was taken without it.
func (m *Manager) enforceQuotaLocked(scan storeScan) error {
	var stat unix.Statfs_t
	if err := unix.Statfs(m.root, &stat); err != nil {
		return fmt.Errorf("data filesystem quota: %w", err)
	}
	total := int64(stat.Blocks) * int64(stat.Bsize)
	free := int64(stat.Bavail) * int64(stat.Bsize)
	quota := total / 5
	if quota > m.maxQuota {
		quota = m.maxQuota
	}
	reserve := m.reserve
	used := scan.used
	for _, name := range scan.readFailures {
		m.warnf("episode %s has an unreadable manifest; its bytes count against the data quota but it can never be uploaded or evicted by state", name)
	}
	candidates := make([]evictionCandidate, 0, len(scan.candidates))
	for _, c := range scan.candidates {
		if !c.quarantined && m.downloads[c.id] > 0 {
			continue
		}
		candidates = append(candidates, c)
	}
	// Lower tier is evicted first; within a tier the oldest goes first.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].tier != candidates[j].tier {
			return candidates[i].tier < candidates[j].tier
		}
		return candidates[i].started < candidates[j].started
	})
	for _, c := range candidates {
		if used <= quota && free >= reserve {
			break
		}
		switch {
		case c.quarantined:
			m.warnf("evicting quarantined episode %s, which recovery could not read, to preserve the data quota", c.id)
		case c.tier == 2:
			m.warnf("evicting episode %s (campaign %s) before its upload completed to preserve the data quota", c.id, c.campaign)
		case c.tier == 1:
			m.warnf("evicting episode %s (campaign %s), which failed to upload and was never retried, to preserve the data quota", c.id, c.campaign)
		}
		if err := os.RemoveAll(c.path); err != nil {
			return fmt.Errorf("evicting %s: %w", filepath.Base(c.path), err)
		}
		used -= c.size
		free += c.size
	}
	if used > quota || free < reserve {
		if scan.partialBytes > 0 {
			return fmt.Errorf("data store holds %d bytes (%d of them in episodes still recording) against a %d byte quota and cannot preserve %d bytes free", used, scan.partialBytes, quota, reserve)
		}
		return fmt.Errorf("data store holds %d bytes against a %d byte quota and cannot preserve %d bytes free", used, quota, reserve)
	}
	return nil
}

// awaitingUpload reports whether an episode's payload has not reached its
// upload destination yet. Local-only episodes ("local" or empty) never upload
// and are evictable; "uploaded" episodes already have a durable remote copy.
// evictionTier orders episodes for quota eviction, lowest evicted first.
//
// Three tiers rather than two. "uploaded" and "local" go first as before: one
// has a copy in the cloud and the other was never meant to leave. "failed" sits
// in the middle, because the worker gave up on it and it is not going anywhere
// on its own; leaving it level with live candidates let a device whose route
// was broken fill its quota with episodes that could never ship while newer
// captures were pushed out. It still outranks data that is already safe, since
// its bytes are the only copy and RequeueFailedUploads can bring it back.
// "pending" and "uploading" are evicted last: they are still on their way out.
func evictionTier(state string) int {
	switch state {
	case "", "local", "uploaded":
		return 0
	case "failed":
		return 1
	}
	return 2
}

// BeginDownload pins an episode against quota eviction for as long as its
// payload is being read, and EndDownload releases that pin. They nest: the
// count is what enforceQuota consults, so an episode a device download and an
// upload are both reading survives until the last reader is done.
//
// Named for the first caller (DataService.DownloadEpisode), but the contract is
// "somebody is reading these bytes right now", which is equally true of the
// transfer worker streaming an episode to the cloud. Every reader must take the
// pin: an evicted episode's files vanish under an open stream, and the reader
// then fails with an error naming the file rather than the eviction.
func (m *Manager) BeginDownload(id string) { m.mu.Lock(); defer m.mu.Unlock(); m.downloads[id]++ }
func (m *Manager) EndDownload(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.downloads[id] <= 1 {
		delete(m.downloads, id)
	} else {
		m.downloads[id]--
	}
}

// RecordApplication validates and stamps an entitled application's record.
// It returns buffered or recorded; protocol-level validation happens before it.
func (m *Manager) RecordApplication(appID string, record ApplicationRecord) (string, error) {
	// The receipt is sampled BEFORE m.mu is taken. It is the agent's statement
	// of when this record arrived, so any wait for the lock belongs outside it:
	// stamped after the wait, a record that queued behind a seal or another
	// record was dated to when the manager got round to it, and the pre-roll
	// window and the episode offsets derived from it inherited that error.
	before, err := readBootTime()
	if err != nil {
		return "rejected", err
	}
	after, err := readBootTime()
	if err != nil {
		return "rejected", err
	}
	receipt := before + (after-before)/2
	// bootID reports the literal "unavailable" when the kernel boot identity
	// cannot be read, so comparing against it unguarded would let a client that
	// sends "unavailable" match on exactly the hosts where the agent has no
	// boot identity to check against, and have its arbitrary timestamp trusted.
	// An agent that cannot name its own boot cannot vouch for a client's, so it
	// accepts no client timestamp at all. Reading it once per record also drops
	// one /proc read from the per-record path.
	agentBootID := readBootIDForAcceptance()
	accepted := agentBootID != "unavailable" && record.ClientBootID == agentBootID &&
		record.ClientBootNanos >= 0 && abs64(record.ClientBootNanos-receipt) <= preRollWindow.Nanoseconds()
	stamp := receipt
	if accepted {
		stamp = record.ClientBootNanos
	}
	stored := storedApplicationRecord{ApplicationRecord: record, AppID: appID, AgentReceiptBootNanos: receipt, ClientTimestampAccepted: accepted, TimestampUncertaintyNanos: (after - before + 1) / 2}
	m.mu.Lock()
	// Every open episode that selected the applications source receives the
	// record on its own timeline; episodes that excluded it are skipped. Open
	// means capturing OR inside its post-seal drain: the drain exists precisely
	// so a record written about the samples an episode just produced still
	// reaches it, and an episode in that window has left m.active for m.sealing.
	recorded := false
	for _, a := range m.openEpisodesLocked() {
		if !a.capturesApplications {
			continue
		}
		stored.EpisodeNanos = stamp - a.manifest.RequestBootNanos
		b, _ := json.Marshal(stored)
		if err := appendJSONL(filepath.Join(a.dir, "events.jsonl"), b); err != nil {
			m.mu.Unlock()
			return "rejected", err
		}
		for i := range a.manifest.Sources {
			if a.manifest.Sources[i].Source.ID == "applications" {
				a.manifest.Sources[i].Count++
			}
		}
		a.noteApplicationRecord(record)
		recorded = true
	}
	// The pre-roll ring buffer is maintained continuously so an episode that
	// starts later still receives its full pre-trigger window, even when
	// another campaign's episode was recording at the time.
	stored.EpisodeNanos = 0
	b, err := json.Marshal(stored)
	if err != nil {
		m.mu.Unlock()
		return "rejected", err
	}
	m.preRoll = append(m.preRoll, bufferedRecord{bootNanos: stamp, receiptNanos: receipt, encoded: b})
	m.preRollBytes += len(b)
	m.evictPreRoll(receipt)
	observer := m.appObserver
	m.mu.Unlock()
	if observer != nil {
		observer(appID, record)
	}
	if recorded {
		return "recorded", nil
	}
	return "buffered", nil
}

// evictPreRoll drops ring entries that have aged out of the pre-roll window or
// that no longer fit the byte budget. Age is measured on the agent receipt, not
// on the presentation stamp: the ring is appended in receipt order, so the head
// is always the oldest arrival and dropping from the front is provably correct.
// Measuring the client-supplied stamp instead let one forward-dated entry sit at
// the head and hold back the eviction of genuinely older entries behind it.
// Only a record dropped while still inside its window is a loss, and the two
// reasons are therefore counted apart: an entry that aged out is no longer
// pre-roll for any episode that could start now, so its departure costs nobody
// anything, while an entry evicted by the byte budget WOULD have reached the
// next episode and did not.
func (m *Manager) evictPreRoll(now int64) {
	cutoff := now - preRollWindow.Nanoseconds()
	for len(m.preRoll) > 0 && (m.preRoll[0].receiptNanos < cutoff || m.preRollBytes > preRollLimit) {
		if m.preRoll[0].receiptNanos >= cutoff {
			m.preRollEvicted = append(m.preRollEvicted, m.preRoll[0].receiptNanos)
		}
		m.preRollBytes -= len(m.preRoll[0].encoded)
		m.preRoll = m.preRoll[1:]
	}
	// The eviction log is itself a pre-roll window's worth of history: an
	// eviction older than the widest window an episode can ask for can no
	// longer be any episode's loss, so it is forgotten rather than accumulated.
	kept := m.preRollEvicted[:0]
	for _, receipt := range m.preRollEvicted {
		if receipt >= cutoff {
			kept = append(kept, receipt)
		}
	}
	m.preRollEvicted = kept
}

// preRollLostInWindowLocked counts the records the ring dropped, while they
// were still inside their window, that would have reached an episode starting
// at origin with the requested pre-roll depth. This is what an episode's
// pre_roll_lost has always claimed to mean.
func (m *Manager) preRollLostInWindowLocked(origin int64, requested time.Duration) uint64 {
	window := preRollWindow
	if requested > 0 && requested < window {
		window = requested
	}
	cutoff := origin - window.Nanoseconds()
	var lost uint64
	for _, receipt := range m.preRollEvicted {
		if receipt >= cutoff && receipt <= origin {
			lost++
		}
	}
	return lost
}

// flushPreRoll copies the buffered records inside the requested window into a
// starting episode. The ring buffer is not consumed: with per-campaign
// concurrency another campaign's later episode is entitled to the same
// pre-trigger records on its own timeline.
//
// Admission is the intersection of two timelines. A record enters the window
// only when both its agent receipt and its presentation stamp fall inside it.
// Testing the stamp alone let an accepted client choose which later episode
// windows it appeared in, by back- or forward-dating its own CLOCK_BOOTTIME
// within the five minute acceptance band; requiring the receipt as well means
// a record can only reach a window it was physically present for. The offset
// arithmetic still uses the stamp, so ActualOffset keeps its meaning and
// offsets stay within [-window, 0].
// The flush opens events.jsonl once and fsyncs it once, rather than reopening
// and fsyncing per record. Every record in the ring is written in the same
// call, so the intermediate syncs bought no durability that the final one does
// not: they only forced up to preRollLimit worth of separate disk barriers
// onto the start path, with m.mu held, before the first frame was captured.
func (m *Manager) flushPreRoll(dir string, origin int64, requested time.Duration) (uint64, *int64, error) {
	window := preRollWindow
	if requested > 0 && requested < window {
		window = requested
	}
	cutoff := origin - window.Nanoseconds()
	var count uint64
	var earliest *int64
	var events *os.File
	defer func() {
		if events != nil {
			_ = events.Close()
		}
	}()
	for _, r := range m.preRoll {
		if r.receiptNanos < cutoff || r.receiptNanos > origin || r.bootNanos < cutoff || r.bootNanos > origin {
			continue
		}
		var stored storedApplicationRecord
		if err := json.Unmarshal(r.encoded, &stored); err != nil {
			continue
		}
		stored.EpisodeNanos = r.bootNanos - origin
		stored.PrerollFlushed = true
		if earliest == nil || stored.EpisodeNanos < *earliest {
			value := stored.EpisodeNanos
			earliest = &value
		}
		b, _ := json.Marshal(stored)
		if events == nil {
			f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				return count, earliest, err
			}
			events = f
		}
		if _, err := events.Write(append(b, '\n')); err != nil {
			return count, earliest, err
		}
		count++
	}
	if events == nil {
		return count, earliest, nil
	}
	if err := syncFile(events); err != nil {
		return count, earliest, err
	}
	return count, earliest, nil
}

// syncFile is the fsync every JSONL append goes through. It is a variable so a
// test can count the disk barriers one operation costs, which is the only way
// to hold the pre-roll flush to a single fsync rather than one per record.
var syncFile = func(f *os.File) error { return f.Sync() }

func appendJSONL(path string, b []byte) error {
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if e != nil {
		return e
	}
	defer f.Close()
	if _, e = f.Write(append(b, '\n')); e != nil {
		return e
	}
	return syncFile(f)
}
func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// openEpisodesLocked returns every episode that can still receive a record:
// the capturing ones in m.active and the ones serving a post-seal drain in
// m.sealing. Callers hold m.mu.
func (m *Manager) openEpisodesLocked() []*activeEpisode {
	open := make([]*activeEpisode, 0, len(m.active)+len(m.sealing))
	for _, a := range m.active {
		open = append(open, a)
	}
	return append(open, m.sealing...)
}

// wantsDrain reports whether this episode has a post-seal drain to serve.
// Episodes that do not capture applications have nothing to wait for.
func (a *activeEpisode) wantsDrain() bool {
	return a.capturesApplications && a.drain > 0
}

// beginSeal ends capture for the episode keyed by key and serves its post-seal
// drain, returning the episode ready to be finalized. It is the shared front
// half of Stop and Interrupt.
//
// The drain exists so that a record an application writes about the samples it
// has just read still lands in the episode those samples came from, so
// RecordApplication has to proceed normally throughout and the sleep is
// therefore taken with m.mu released.
//
// The campaign key is released BEFORE the sleep, not after. A draining episode
// is no longer capturing: its cameras are off and its adapters have stopped.
// Leaving it in m.active for the drain made its campaign look busy to the
// trigger path, which skips any campaign named in ActiveEpisodeKeys, so a
// matching record arriving inside the window was dropped outright -- not
// queued, not delayed. With DefaultSealDrain at two seconds and every campaign
// paying it (campaign plans always select the applications source), a second
// person walking in within two seconds of the previous episode produced no
// recording at all, while the detection itself was filed into the episode whose
// cameras had already stopped. An event with no video is the exact failure this
// platform exists to prevent.
//
// Moving the episode to m.sealing rather than merely flagging it keeps
// Manager.Start's "one episode per key" rule doing the work it already does:
// the key is genuinely free, so the next episode is admitted, and there is
// still no way to have two capturing episodes for one campaign.
// drain false skips the wait entirely; see InterruptWithoutDrain for the one
// caller that may.
func (m *Manager) beginSeal(key string, drain bool) (*activeEpisode, error) {
	m.mu.Lock()
	a := m.active[key]
	if a == nil {
		m.mu.Unlock()
		return nil, ErrNoActiveEpisode
	}
	if a.cancel != nil {
		a.cancel()
	}
	m.mu.Unlock()
	if a.done != nil {
		<-a.done
	}
	// Stamp when capture actually ended, here, rather than letting the seal's
	// stopped_episode_nanos stand for it. The drain below is a window for late
	// application records, not recording time: nothing is captured during it,
	// so a consumer computing episode length from stopped_episode_nanos
	// over-reported every drained episode by the whole drain. Both numbers are
	// now in the manifest and each means what it says.
	//
	// The stillOpenLocked check is the same fence finalize relies on, and it is
	// load-bearing rather than defensive. A concurrent Stop and Interrupt can
	// both leave the block above holding this episode; the one that reaches
	// finalize first detaches it under this mutex and then seals it with the
	// mutex released, so writing to the manifest here after that detachment
	// would race the seal's own reads. Once the episode is detached it is not
	// ours to stamp, and the seal has already read the clock itself.
	if now, timeErr := readBootTime(); timeErr == nil {
		m.mu.Lock()
		if m.stillOpenLocked(a) {
			a.manifest.CaptureStoppedEpisodeNS = now - a.manifest.RequestBootNanos
		}
		m.mu.Unlock()
	}
	if !drain || !a.wantsDrain() {
		return a, nil
	}
	m.mu.Lock()
	if m.active[key] != a {
		m.mu.Unlock()
		return nil, ErrNoActiveEpisode
	}
	delete(m.active, key)
	a.draining = true
	m.sealing = append(m.sealing, a)
	m.mu.Unlock()
	time.Sleep(a.drain)
	return a, nil
}

// stillOpenLocked reports whether an episode returned by beginSeal is still the
// manager's to finalize, which is the post-drain half of the identity check
// Stop and Interrupt have always made.
func (m *Manager) stillOpenLocked(a *activeEpisode) bool {
	if !a.draining {
		return m.active[a.key] == a
	}
	for _, sealing := range m.sealing {
		if sealing == a {
			return true
		}
	}
	return false
}

// Stop finalizes the episode keyed by the given campaign name
// (AdHocEpisodeKey for campaign-less episodes).
//
// For an episode that captures applications the window for application records
// does not close when the capture adapters do: the episode stays open for its
// configured drain (see beginSeal) so an application that scores asynchronously
// can still file its verdict here. It gives up its campaign key as it enters
// that window, so the campaign can start its next episode immediately.
// StoppedEpisodeNS is stamped in the seal, after the drain, so records
// that arrive during the drain carry an EpisodeNanos below it exactly as live
// ones do.
func (m *Manager) Stop(key string) (Manifest, error) {
	a, err := m.beginSeal(key, true)
	if err != nil {
		return Manifest{}, err
	}
	return m.finalize(a, "complete", "")
}

// sealMux is the seal's playable remux, indirected so a test can substitute a
// slow one and observe that the rest of the manager keeps serving during it.
// Production always runs muxPlayableClips.
var sealMux = muxPlayableClips

// finalize seals an episode whose capture has stopped and whose post-seal
// drain, if it had one, has already been served.
//
// It runs in three phases so that the cost of a seal is not charged to every
// other caller of the manager. A seal rewrites every camera byte (the playable
// remux) and then reads every byte again (the per-file SHA-256), which on a
// multi-gigabyte episode is seconds of input and output. Holding m.mu across
// that blocked Start, Status, ActiveEpisodeKeys, UpdateUploadState and, worst
// of all, RecordApplication, whose agent receipt was then taken after the wait:
// the whole seal duration was added to a timestamp whose entire purpose is to
// say when the agent received the record.
//
//  1. Under the lock the episode is detached from m.active and m.sealing. That
//     detachment is the fence the unlocked phase relies on: openEpisodesLocked
//     no longer returns the episode, so no application record and no model
//     input can be appended to it once its bytes are being hashed, and no
//     second Stop or Interrupt can claim it.
//  2. With the lock released the episode's clock is read, its ledger is
//     flushed, and its bytes are muxed and hashed. Nothing else can reach the
//     episode by then, and the quota counts a ".partial" directory's bytes but
//     never offers one as an eviction candidate, so the store cannot evict it
//     mid-seal either.
//  3. The lock is retaken to write the manifest and rename the directory out
//     of ".partial", which is what publishes the episode to every path that
//     walks the store.
//
// A failure in any phase abandons the episode rather than parking it. It is
// already detached, so it can neither be sealed twice nor absorb further
// records, and its directory keeps its ".partial" suffix, which is exactly
// what recoverPartials seals on the next start. Leaving it in m.sealing
// instead, as an earlier version did, produced an episode no caller could
// reach (Stop and Interrupt look only in m.active) that nevertheless kept
// answering "recorded" for records nothing would ever seal.
func (m *Manager) finalize(a *activeEpisode, state, reason string) (Manifest, error) {
	m.mu.Lock()
	if !m.stillOpenLocked(a) {
		m.mu.Unlock()
		return Manifest{}, ErrNoActiveEpisode
	}
	m.forgetLocked(a)
	a.manifest.State, a.manifest.Interruption = state, reason
	consensus := m.consensus
	m.mu.Unlock()

	if err := m.sealDetached(a, consensus); err != nil {
		m.Warnf("episode %s: seal failed, leaving %s for recovery on the next start: %v",
			a.manifest.ID, filepath.Base(a.dir), err)
		return Manifest{}, err
	}
	return a.manifest, nil
}

// sealDetached is phases two and three of finalize: everything the seal does
// once the episode belongs to this call alone. Only the caller may run it, and
// only after the episode has left m.active and m.sealing.
func (m *Manager) sealDetached(a *activeEpisode, consensus func(context.Context) (timesync.Consensus, error)) error {
	now, err := readBootTime()
	if err != nil {
		return err
	}
	a.manifest.StoppedEpisodeNS = now - a.manifest.RequestBootNanos
	if obs, obsErr := observeUTC(a.manifest.RequestBootNanos, "system_reported", "linux_realtime_sandwich"); obsErr == nil {
		a.manifest.UTCObservations = append(a.manifest.UTCObservations, obs)
	}
	if consensus != nil {
		ctx, cancel := context.WithTimeout(context.Background(), consensusQueryTimeout)
		c, queryErr := consensus(ctx)
		cancel()
		if queryErr == nil {
			recordConsensus(a.dir, &a.manifest, c)
			if len(a.manifest.UTCObservations) > 0 && clockAgreement(a.manifest.UTCObservations[len(a.manifest.UTCObservations)-1], c) == ClockStatusConflict {
				a.manifest.SystemClockStatus = ClockStatusConflict
			}
		}
	}
	// The model-input ledger is appended without per-sample fsync, so it must
	// reach the disk before its checksum is taken.
	if a.modelInputs != nil {
		syncErr := a.modelInputs.Sync()
		closeErr := a.modelInputs.Close()
		a.modelInputs = nil
		if err := errors.Join(syncErr, closeErr); err != nil {
			return err
		}
	}
	// The playable remux runs before sealFiles so each derived
	// cameras/<source>/playable.mp4 is checksummed, listed, uploaded and
	// verified exactly like the capture it derives from. Sealing was already
	// a synchronous pass over every episode byte (the checksums below), and
	// the remux is a copy, not a transcode, so this keeps the seal's shape:
	// one more read of the camera bytes, never a failure. A source that
	// cannot be muxed honestly seals without its clip and the notes say why.
	a.manifest.PlayableNotes = sealMux(a.dir)
	for _, note := range a.manifest.PlayableNotes {
		m.Warnf("episode %s: %s", a.manifest.ID, note)
	}
	files, err := sealFiles(a.dir)
	if err != nil {
		return err
	}
	associateFileSources(files, a.manifest.Sources)
	a.manifest.Files = files

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := writeManifest(a.dir, a.manifest); err != nil {
		return err
	}
	return os.Rename(a.dir, strings.TrimSuffix(a.dir, ".partial"))
}

// forgetLocked drops a finalized episode from whichever collection holds it:
// m.active while it was capturing, m.sealing once it entered its drain and gave
// up its campaign key.
func (m *Manager) forgetLocked(a *activeEpisode) {
	if !a.draining {
		delete(m.active, a.key)
		return
	}
	for i, sealing := range m.sealing {
		if sealing != a {
			continue
		}
		m.sealing = append(m.sealing[:i], m.sealing[i+1:]...)
		return
	}
}

// EpisodeStateDraining is the state Status reports for an episode that has
// stopped capturing and is serving its post-seal drain. It never reaches a
// manifest on disk: a drained episode seals as "complete" or "interrupted"
// like any other. It exists so that a status query during the drain says the
// episode is finishing rather than that nothing is happening, which is what
// "no active episode" claimed while Stop was still sleeping.
const EpisodeStateDraining = "draining"

// Status reports one active episode for status displays: the ad-hoc episode
// when present, otherwise the earliest-started campaign episode. An episode
// serving its post-seal drain is reported with State EpisodeStateDraining once
// no capturing episode is left, so the seal is visible rather than looking
// like an idle device. Use ActiveEpisodeKeys and ActiveSession to enumerate
// concurrent episodes; those deliberately exclude draining ones, because a
// draining episode's campaign is free to start its next episode.
func (m *Manager) Status() *Manifest {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.active[AdHocEpisodeKey]; a != nil {
		v := snapshotManifest(a.manifest)
		return &v
	}
	var earliest *activeEpisode
	for _, a := range m.active {
		if earliest == nil || a.manifest.StartedUnixNanos < earliest.manifest.StartedUnixNanos {
			earliest = a
		}
	}
	if earliest != nil {
		v := snapshotManifest(earliest.manifest)
		return &v
	}
	for _, a := range m.sealing {
		if earliest == nil || a.manifest.StartedUnixNanos < earliest.manifest.StartedUnixNanos {
			earliest = a
		}
	}
	if earliest == nil {
		return nil
	}
	v := snapshotManifest(earliest.manifest)
	v.State = EpisodeStateDraining
	return &v
}

// EpisodesAwaitingUpload returns the full manifests of finalized episodes whose
// upload workflow still needs work: "pending" (queued) or "uploading" (left
// mid-transfer by a crashed worker and safe to resume). The manifest is the
// single source of truth for upload progress; the transfer worker consumes this
// queue and calls UpdateUploadState to persist each transition. Episodes are
// returned oldest-first so the backlog drains in capture order.
// RequeueFailedUploads moves every "failed" episode back to "pending" and
// returns how many it moved.
//
// "failed" means the worker gave up, not that the episode is unshippable: a
// wrong endpoint, an expired certificate or a cloud outage long enough to
// exhaust the retry budget all land here, and every one of them is fixed by a
// change OUTSIDE the episode. Without this the fix arrives and the backlog
// stays dead, because EpisodesAwaitingUpload only ever returns pending and
// uploading, so nothing would look at those episodes again.
//
// The attempt counter resets with the state; keeping it would exhaust the
// budget again on the first retry and undo the requeue.
func (m *Manager) RequeueFailedUploads() (int, error) {
	// Collect under the lock, then update outside it: UpdateUploadState takes
	// the same mutex, so mutating in place here would deadlock.
	m.mu.Lock()
	entries, err := os.ReadDir(m.root)
	if err != nil {
		m.mu.Unlock()
		return 0, err
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasSuffix(e.Name(), ".partial") {
			continue
		}
		mf, err := readManifest(filepath.Join(m.root, e.Name()))
		if err != nil || mf.Upload.State != "failed" {
			continue
		}
		ids = append(ids, mf.ID)
	}
	m.mu.Unlock()

	moved := 0
	for _, id := range ids {
		if _, err := m.UpdateUploadState(id, func(ws *WorkflowState) {
			ws.State = "pending"
			ws.Attempts = 0
			ws.NextAttemptUnixNanos = 0
		}); err != nil {
			continue
		}
		moved++
	}
	return moved, nil
}

func (m *Manager) EpisodesAwaitingUpload() ([]Manifest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return nil, err
	}
	var out []Manifest
	for _, e := range entries {
		if !e.IsDir() || strings.HasSuffix(e.Name(), ".partial") {
			continue
		}
		mf, err := readManifest(filepath.Join(m.root, e.Name()))
		if err != nil {
			continue
		}
		switch mf.Upload.State {
		case "pending", "uploading":
			out = append(out, mf)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedUnixNanos < out[j].StartedUnixNanos })
	return out, nil
}

// UpdateUploadState atomically rewrites the upload workflow of a finalized
// episode's manifest. The mutate callback receives the current upload state and
// may change any field; UpdatedAt is stamped automatically. The rewrite reuses
// the manifest's atomic tmp-file-plus-rename pattern so a crash mid-write never
// leaves a torn manifest. Returns the updated manifest.
func (m *Manager) UpdateUploadState(id string, mutate func(ws *WorkflowState)) (Manifest, error) {
	if mutate == nil {
		return Manifest{}, errors.New("mutate callback is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	dir, err := m.episodeDir(id)
	if err != nil {
		return Manifest{}, err
	}
	mf, err := readManifest(dir)
	if err != nil {
		return Manifest{}, err
	}
	mutate(&mf.Upload)
	mf.Upload.UpdatedAt = time.Now().UnixNano()
	if err := writeManifest(dir, mf); err != nil {
		return Manifest{}, err
	}
	return mf, nil
}

func (m *Manager) List() ([]EpisodeInfo, error) {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return nil, err
	}
	var out []EpisodeInfo
	for _, e := range entries {
		if !e.IsDir() || strings.HasSuffix(e.Name(), ".partial") {
			continue
		}
		mf, err := readManifest(filepath.Join(m.root, e.Name()))
		if err != nil {
			continue
		}
		var size int64
		for _, f := range mf.Files {
			size += f.Size
		}
		out = append(out, EpisodeInfo{ID: mf.ID, Name: mf.Name, State: mf.State, StartedUnixNanos: mf.StartedUnixNanos, SizeBytes: size, BootID: mf.BootID})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedUnixNanos > out[j].StartedUnixNanos })
	return out, nil
}

func (m *Manager) Inspect(id string, verify bool) (Manifest, []string, error) {
	dir, err := m.episodeDir(id)
	if err != nil {
		return Manifest{}, nil, err
	}
	mf, err := readManifest(dir)
	if err != nil {
		return Manifest{}, nil, err
	}
	if !verify {
		return mf, nil, nil
	}
	var failures []string
	for _, f := range mf.Files {
		p, err := safeJoin(dir, f.Path)
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		if err := requireRegularFile(p); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", f.Path, err))
			continue
		}
		got, size, err := checksum(p)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", f.Path, err))
			continue
		}
		if size != f.Size || got != f.SHA256 {
			failures = append(failures, fmt.Sprintf("%s: checksum or size mismatch", f.Path))
		}
	}
	return mf, failures, nil
}

func (m *Manager) OpenFile(id, rel string, offset int64) (*os.File, File, error) {
	dir, err := m.episodeDir(id)
	if err != nil {
		return nil, File{}, err
	}
	mf, err := readManifest(dir)
	if err != nil {
		return nil, File{}, err
	}
	var want *File
	for i := range mf.Files {
		if mf.Files[i].Path == rel {
			want = &mf.Files[i]
			break
		}
	}
	if want == nil {
		return nil, File{}, os.ErrNotExist
	}
	if offset < 0 || offset > want.Size {
		return nil, File{}, ErrInvalidDownloadOffset
	}
	p, err := safeJoin(dir, rel)
	if err != nil {
		return nil, File{}, err
	}
	if err := requireRegularFile(p); err != nil {
		return nil, File{}, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, File{}, err
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		f.Close()
		return nil, File{}, err
	}
	return f, *want, nil
}

func (m *Manager) episodeDir(id string) (string, error) {
	// "." and ".." survive safeName untouched, because it preserves dots so
	// that identifiers and calibration filenames keep theirs. Joined onto the
	// root they name the store itself and its parent, so an RPC asking to
	// inspect or download episode ".." was reading outside the episode store
	// entirely. Neither is a generated identifier, so both are simply refused.
	if id == "" || id == "." || id == ".." || safeName(id) != id {
		return "", ErrInvalidEpisodeID
	}
	p := filepath.Join(m.root, id)
	if st, err := os.Stat(p); err != nil || !st.IsDir() {
		if err == nil {
			err = os.ErrNotExist
		}
		return "", err
	}
	return p, nil
}

// recoverPartials repairs every crash-interrupted episode directory left in
// the store, and is the only thing standing between a power cut and a
// permanently unreadable episode.
//
// No single damaged directory may stop the agent from starting. Failing
// NewManager over one unreadable partial bricked data capture on the device
// until somebody logged in and deleted a directory by hand, and the episode
// that caused it was unreadable either way. A directory that cannot be
// repaired is therefore quarantined under <id>.unrecoverable and named in a
// warning, and recovery moves on to the next one.
func (m *Manager) recoverPartials() error {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasSuffix(e.Name(), ".partial") {
			continue
		}
		dir := filepath.Join(m.root, e.Name())
		if err := m.recoverPartial(dir); err != nil {
			m.warnf("episode %s could not be recovered (%v); quarantining it", strings.TrimSuffix(e.Name(), ".partial"), err)
			m.quarantinePartial(dir)
		}
	}
	return nil
}

// quarantinePartial takes an unrepairable directory out of the recovery path.
// A directory with no manifest at all is deleted rather than kept: it was
// created between Mkdir and the first manifest write, so it holds no episode
// and there is nothing in it to salvage. Anything else is renamed, so the
// bytes stay on disk for an operator to look at, stop being retried on every
// agent start, and become visible to (and evictable by) the quota.
func (m *Manager) quarantinePartial(dir string) {
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); errors.Is(err, os.ErrNotExist) {
		if err := os.RemoveAll(dir); err != nil {
			m.warnf("removing empty episode directory %s failed: %v", filepath.Base(dir), err)
		}
		return
	}
	target := strings.TrimSuffix(dir, ".partial") + unrecoverableSuffix
	if err := os.Rename(dir, target); err != nil {
		m.warnf("quarantining episode directory %s failed: %v", filepath.Base(dir), err)
	}
}

func (m *Manager) recoverPartial(dir string) error {
	mf, err := readManifest(dir)
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}
	// A manifest that already says "complete" was fully sealed: its files were
	// checksummed and listed, and the crash landed in the one instruction
	// between writing that manifest and renaming the directory. Rerunning
	// recovery over it would relabel a complete episode as interrupted and
	// blame a reboot for a rename, so the only thing left to do is the rename.
	if mf.State == "complete" {
		return os.Rename(dir, strings.TrimSuffix(dir, ".partial"))
	}
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr == nil && !d.IsDir() && strings.HasSuffix(d.Name(), ".jsonl") {
			truncateJSONL(path)
		}
		return nil
	})
	reason := "agent_restart"
	if current := bootID(); mf.BootID != "" && mf.BootID != current {
		reason = "reboot"
	}
	mf.State, mf.Interruption = "interrupted", reason
	mf.RecoveryActions = append(mf.RecoveryActions, "truncated incomplete JSONL tail", "recomputed sealed-file checksums")
	// The summary counters are folded in memory and written only at seal, so
	// an interrupted episode arrives here with all of them at zero while its
	// ledger and outcome log are intact on disk. Publishing those zeros would
	// be a manifest that lies about what the model consumed, so recompute
	// them from the files that survived rather than annotating the lie.
	reconciled, reconcileErr := reconcileModelIO(dir, &mf)
	if reconcileErr != nil {
		return fmt.Errorf("reconciling model input/outcome accounting: %w", reconcileErr)
	}
	if reconciled {
		mf.RecoveryActions = append(mf.RecoveryActions, "recomputed model input/outcome counters from "+ModelInputLedgerFile+" and "+mf.ModelIO.OutcomeLog)
	}
	for _, note := range mf.ModelIO.RecoveryNotes {
		m.warnf("episode %s: %s", mf.ID, note)
	}
	// Recovery is the other path that seals an episode, so it derives the
	// same playable clips; the truncated index tails above were already
	// cut, and the muxer counts a partial trailing line as unusable
	// rather than guessing at it.
	mf.PlayableNotes = muxPlayableClips(dir)
	mf.Files, err = sealFiles(dir)
	if err != nil {
		return fmt.Errorf("sealing files: %w", err)
	}
	associateFileSources(mf.Files, mf.Sources)
	if err := writeManifest(dir, mf); err != nil {
		return fmt.Errorf("writing manifest: %w", err)
	}
	return os.Rename(dir, strings.TrimSuffix(dir, ".partial"))
}

func selectSources(all []Source, include, exclude []string) ([]Source, error) {
	ex := map[string]bool{}
	for _, id := range exclude {
		ex[id] = true
	}
	inc := map[string]bool{}
	for _, id := range include {
		inc[id] = true
	}
	var out []Source
	for _, s := range all {
		if ex[s.ID] || (!s.Healthy && len(include) == 0) {
			continue
		}
		if len(include) == 0 || inc[s.ID] {
			out = append(out, s)
			delete(inc, s.ID)
		}
	}
	if len(inc) > 0 {
		var ids []string
		for id := range inc {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return nil, fmt.Errorf("unknown or unhealthy source(s): %s", strings.Join(ids, ", "))
	}
	return out, nil
}

func DiscoverSources() []Source {
	return []Source{{ID: "applications", Kind: "application", ClockDomain: "CLOCK_BOOTTIME", Healthy: true}, {ID: "telemetry", Kind: "telemetry", ClockDomain: "CLOCK_BOOTTIME", Healthy: true}}
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '_'
	}, s)
}

func safeJoin(root, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || filepath.Clean(rel) != rel || strings.HasPrefix(rel, "..") {
		return "", ErrInvalidEpisodePath
	}
	p := filepath.Join(root, rel)
	if !strings.HasPrefix(p, root+string(os.PathSeparator)) {
		return "", ErrEpisodePathEscapes
	}
	return p, nil
}

func requireRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return ErrEpisodeEntryNotRegular
	}
	return nil
}

func checksum(p string) (string, int64, error) {
	f, e := os.Open(p)
	if e != nil {
		return "", 0, e
	}
	defer f.Close()
	h := sha256.New()
	n, e := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, e
}

func sealFiles(dir string) ([]File, error) {
	var out []File
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("episode contains symlink %s", p)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("episode contains non-regular file %s", p)
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "manifest.json" || strings.HasSuffix(rel, ".tmp") {
			return nil
		}
		h, n, e := checksum(p)
		if e != nil {
			return e
		}
		rel = filepath.ToSlash(rel)
		format, mediaType := payloadFormat(rel)
		entry := File{Path: rel, Size: n, SHA256: h, SourceID: sourceForPath(rel), Format: format, MediaType: mediaType}
		if isDerivedPlayable(rel) {
			entry.Role = FileRoleDerived
		}
		out = append(out, entry)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, err
}

func payloadFormat(path string) (string, string) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mcap":
		return "mcap", "application/vnd.mcap"
	case ".db3":
		return "rosbag2", "application/vnd.sqlite3"
	case ".h264":
		return "h264", "video/h264"
	case ".h265", ".hevc":
		return "h265", "video/h265"
	case ".mp4":
		return "mp4", "video/mp4"
	case ".wav":
		return "wav", "audio/wav"
	case ".jpg", ".jpeg":
		return "jpeg", "image/jpeg"
	case ".png":
		return "png", "image/png"
	case ".parquet":
		return "parquet", "application/vnd.apache.parquet"
	case ".jsonl":
		return "jsonl", "application/x-ndjson"
	case ".yaml", ".yml":
		return "yaml", "application/yaml"
	default:
		return "binary", "application/octet-stream"
	}
}

// sourceForPath names the source behind the two files whose episode-relative
// path is fixed rather than derived from a source identifier. Every other file
// is attributed by associateFileSources, which matches it against the sources
// the manifest actually declares. A path component is deliberately NOT used as
// a fallback identifier: it is safeName of an identifier, and safeName is not
// invertible, so a file no source claims is left unattributed rather than
// labelled with a fragment that matches nothing on the device or in the cloud
// catalog.
func sourceForPath(path string) string {
	switch path {
	case "events.jsonl":
		return "applications"
	case "telemetry.jsonl":
		return "telemetry"
	}
	return ""
}

// fileSourceKey is the path component that a source's files are written under.
// It is safeName of the source identifier, except for a ROS 2 topic source: one
// recorder per DDS domain records every selected topic on that domain into a
// single bag, and both the bag directory and its clock-sample sidecar are named
// for the domain, so a topic source's files live under the domain's key.
func fileSourceKey(id string) string {
	if domainID, _, ok := ParseROS2SourceID(id); ok {
		return safeName(domainID)
	}
	return safeName(id)
}

// fileHasSourceKey reports whether an episode-relative path, with its
// kind directory already stripped, belongs to the source encoded as key: the
// payload directory itself, a file inside it, the source's calibration
// blob, or a sidecar such as "<key>-clock_samples.jsonl".
func fileHasSourceKey(path, key string) bool {
	return path == key || path == key+".calibration" ||
		strings.HasPrefix(path, key+"/") || strings.HasPrefix(path, key+"-")
}

// associateFileSources attaches every sealed file to the episode sources that
// produced it.
//
// Most files have exactly one source. A ROS 2 bag has as many as the campaign
// selected topics on its domain, because the domain is recorded once: the bag
// and its clock samples are the payload of each of those sources, not of a
// domain source the campaign never named. SourceID stays single valued for the
// consumers that key on it, so it carries the domain-level source when the
// campaign selected one and the first selected topic source otherwise, and the
// remaining sources are listed in AdditionalSourceIDs.
func associateFileSources(files []File, sources []SourceStats) {
	for i := range files {
		if files[i].SourceID != "" {
			continue
		}
		path := strings.TrimPrefix(files[i].Path, "cameras/")
		path = strings.TrimPrefix(path, "ros2/")
		path = strings.TrimPrefix(path, "audio/")
		var matched []string
		var additional []string
		primary := -1
		for _, stats := range sources {
			id := stats.Source.ID
			if !fileHasSourceKey(path, fileSourceKey(id)) {
				continue
			}
			if _, topic, isROS2 := ParseROS2SourceID(id); isROS2 && topic == "" && primary < 0 {
				primary = len(matched)
			}
			matched = append(matched, id)
		}
		if len(matched) == 0 {
			continue
		}
		if primary < 0 {
			primary = 0
		}
		for j, id := range matched {
			if j != primary {
				additional = append(additional, id)
			}
		}
		files[i].SourceID, files[i].AdditionalSourceIDs = matched[primary], additional
	}
}

// writeManifest replaces the episode manifest durably.
//
// It goes through atomicfile rather than a bare WriteFile plus Rename: the
// previous version fsynced neither the temporary file nor the directory, so a
// device that lost power just after the rename could come back with the
// manifest entry pointing at a file whose contents had never left the page
// cache. On embedded hardware that loses power without warning, which is what
// this package records on, a zero-length manifest makes the whole episode
// unreadable even though every payload byte survived.
func writeManifest(dir string, m Manifest) error {
	b, e := json.MarshalIndent(m, "", "  ")
	if e != nil {
		return e
	}
	b = append(b, '\n')
	return atomicfile.Write(filepath.Join(dir, "manifest.json"), b, 0o640)
}
func readManifest(dir string) (Manifest, error) {
	var m Manifest
	b, e := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if e != nil {
		return m, e
	}
	e = json.Unmarshal(b, &m)
	return m, e
}

// truncateJSONLChunk is how much of a JSONL tail is read at a time when
// hunting for the last complete line. One read covers any line this package
// writes; a file whose tail holds no newline within a chunk is scanned
// backwards a chunk at a time rather than in one allocation.
const truncateJSONLChunk = 64 << 10

// truncateJSONL cuts a torn trailing line off a crash-interrupted JSONL file.
//
// It scans backwards in bounded chunks instead of reading the file into
// memory. The previous implementation did os.ReadFile followed by string(b),
// which holds two copies of the whole file at once: an episode that recorded a
// multi-gigabyte telemetry or event log made recovery allocate twice its size
// on a device that has a few hundred megabytes of RAM, and the agent was
// killed by the out-of-memory killer at exactly the moment it was trying to
// repair itself.
func truncateJSONL(p string) {
	f, err := os.Open(p)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return
	}
	size := info.Size()
	if size == 0 {
		return
	}
	buf := make([]byte, truncateJSONLChunk)
	for end := size; end > 0; {
		start := end - int64(len(buf))
		if start < 0 {
			start = 0
		}
		chunk := buf[:end-start]
		if _, err := f.ReadAt(chunk, start); err != nil {
			return
		}
		if i := bytes.LastIndexByte(chunk, '\n'); i >= 0 {
			if keep := start + int64(i) + 1; keep != size {
				_ = os.Truncate(p, keep)
			}
			return
		}
		end = start
	}
	// No newline anywhere: the file holds nothing but a torn first line.
	_ = os.Truncate(p, 0)
}

// clockAgreement judges the device's system clock against a Roughtime
// consensus. It returns one of the ClockStatus constants in model.go.
//
// An unbounded system observation has no interval to compare: observeUTC
// leaves both offset bounds at zero when adjtimex reports the clock
// unsynchronized. Comparing that empty interval against a real consensus
// declared a conflict on every episode recorded by every device without a
// synced clock, which is most of them at first boot, and a conflict that no
// evidence supports is worse than no status at all. Such an episode reports
// roughtime_only: the consensus is its sole UTC evidence, and the system clock
// is not disagreeing with it, it is simply saying nothing.
func clockAgreement(system UTCObservation, consensus timesync.Consensus) string {
	if consensus.Confidence == "unbounded" {
		return ClockStatusSystemReported
	}
	if system.Confidence == "unbounded" {
		return ClockStatusRoughtimeOnly
	}
	if system.OffsetUpperNanos < consensus.LowerOffsetNanos || consensus.UpperOffsetNanos < system.OffsetLowerNanos {
		return ClockStatusConflict
	}
	return ClockStatusAgreement
}
