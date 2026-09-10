package data

import (
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
	mu             sync.Mutex
	root           string
	active         map[string]*activeEpisode
	consensus      func(context.Context) (timesync.Consensus, error)
	preRoll        []bufferedRecord
	preRollBytes   int
	preRollLost    uint64
	downloads      map[string]int
	sourceProvider func(context.Context) []Source
	appObserver    func(string, ApplicationRecord)
	warn           func(string)
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
	defer m.mu.Unlock()
	m.warn = warn
}

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
	if m.warn != nil {
		m.warn(fmt.Sprintf(format, args...))
	}
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

type bufferedRecord struct {
	bootNanos int64
	encoded   []byte
}

type ApplicationRecord struct {
	Version    int            `json:"version"`
	Type       string         `json:"type"`
	Name       string         `json:"name,omitempty"`
	Model      string         `json:"model,omitempty"`
	Value      any            `json:"value,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
	// Inputs binds this record to the harness samples it was computed from, by
	// the same (source_id, sample_id) pair the app received from
	// SensorService.Subscribe. It is optional: a record that names no inputs is
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
	// modelInputs is the append handle for the model-input ledger, opened on
	// the first sample a model consumed (see openModelInputLedger). It is not
	// fsynced per sample: at sensor rates that would dominate the write path,
	// and a torn tail is recovered exactly like every other episode JSONL.
	modelInputs *os.File
	// awaitConsensus reports that the episode's opening Roughtime consensus was
	// moved off the start path and is still to be attached (see Start).
	awaitConsensus bool
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
	m.mu.Lock()
	defer m.mu.Unlock()
	key := opts.Trigger.CampaignName
	if m.active[key] != nil {
		if key == AdHocEpisodeKey {
			return Manifest{}, errors.New("an episode is already active")
		}
		return Manifest{}, fmt.Errorf("an episode is already active for campaign %s", key)
	}
	if err := m.enforceQuota(); err != nil {
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
	discovered := DiscoverSources()
	if m.sourceProvider != nil {
		discovered = append(discovered, m.sourceProvider(context.Background())...)
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
		manifest.Roughtime = append(manifest.Roughtime, *consensus)
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
		manifest.PreRollLost = m.preRollLost
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
	a := &activeEpisode{key: key, dir: dir, manifest: manifest, capturesApplications: capturesApplications, awaitConsensus: deferConsensus}
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
	return CaptureSession{ID: a.manifest.ID, Directory: a.dir, RequestBootNanos: a.manifest.RequestBootNanos, BootID: a.manifest.BootID}, true
}

// ActiveEpisodeKeys returns the sorted concurrency keys of active episodes.
// AdHocEpisodeKey marks a campaign-less episode; every other key is a
// campaign name.
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

// Interrupt finalizes an active episode after adapter startup failed. Existing
// monotonic data is retained for auditability and is never silently deleted.
func (m *Manager) Interrupt(key, reason string) (Manifest, error) {
	m.mu.Lock()
	a := m.active[key]
	if a == nil {
		m.mu.Unlock()
		return Manifest{}, ErrNoActiveEpisode
	}
	if a.cancel != nil {
		a.cancel()
	}
	m.mu.Unlock()
	if a.done != nil {
		<-a.done
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active[key] != a {
		return Manifest{}, ErrNoActiveEpisode
	}
	return m.finalizeLocked(a, "interrupted", reason)
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
			b, _ := json.Marshal(sample)
			if err = appendJSONL(filepath.Join(a.dir, "telemetry.jsonl"), b); err == nil {
				m.mu.Lock()
				if m.active[a.key] == a {
					for i := range a.manifest.Sources {
						if a.manifest.Sources[i].Source.ID == "telemetry" {
							a.manifest.Sources[i].Count++
						}
					}
				}
				m.mu.Unlock()
			}
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
	a.manifest.Roughtime = append(a.manifest.Roughtime, c)
	if opening {
		a.awaitConsensus = false
		if len(a.manifest.UTCObservations) > 0 {
			a.manifest.SystemClockStatus = clockAgreement(a.manifest.UTCObservations[0], c)
		}
	}
	_ = writeManifest(a.dir, a.manifest)
}

func (m *Manager) enforceQuota() error {
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
	type candidate struct {
		path          string
		started, size int64
		id, campaign  string
		awaitUpload   bool
	}
	var candidates []candidate
	var used int64
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasSuffix(e.Name(), ".partial") {
			continue
		}
		dir := filepath.Join(m.root, e.Name())
		mf, err := readManifest(dir)
		if err != nil {
			continue
		}
		var size int64
		_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, e error) error {
			if e == nil && !d.IsDir() {
				if info, x := d.Info(); x == nil {
					size += info.Size()
				}
			}
			return nil
		})
		used += size
		if m.downloads[mf.ID] == 0 {
			candidates = append(candidates, candidate{dir, mf.StartedUnixNanos, size, mf.ID, mf.Trigger.CampaignName, awaitingUpload(mf.Upload.State)})
		}
	}
	// Episodes that still await upload are evicted only after every uploaded or
	// local-only episode; within each group the oldest goes first.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].awaitUpload != candidates[j].awaitUpload {
			return !candidates[i].awaitUpload
		}
		return candidates[i].started < candidates[j].started
	})
	for _, c := range candidates {
		if used <= quota && free >= reserve {
			break
		}
		if c.awaitUpload {
			m.warnf("evicting episode %s (campaign %s) before its upload completed to preserve the data quota", c.id, c.campaign)
		}
		if err := os.RemoveAll(c.path); err != nil {
			return fmt.Errorf("evicting %s: %w", filepath.Base(c.path), err)
		}
		used -= c.size
		free += c.size
	}
	if used > quota || free < reserve {
		return fmt.Errorf("data store holds %d bytes against a %d byte quota and cannot preserve %d bytes free", used, quota, reserve)
	}
	return nil
}

// awaitingUpload reports whether an episode's payload has not reached its
// upload destination yet. Local-only episodes ("local" or empty) never upload
// and are evictable; "uploaded" episodes already have a durable remote copy.
func awaitingUpload(state string) bool {
	switch state {
	case "", "local", "uploaded":
		return false
	}
	return true
}

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
	m.mu.Lock()
	before, err := readBootTime()
	if err != nil {
		m.mu.Unlock()
		return "rejected", err
	}
	after, err := readBootTime()
	if err != nil {
		m.mu.Unlock()
		return "rejected", err
	}
	receipt := before + (after-before)/2
	accepted := record.ClientBootID == bootID() && record.ClientBootNanos >= 0 && abs64(record.ClientBootNanos-receipt) <= preRollWindow.Nanoseconds()
	stamp := receipt
	if accepted {
		stamp = record.ClientBootNanos
	}
	stored := storedApplicationRecord{ApplicationRecord: record, AppID: appID, AgentReceiptBootNanos: receipt, ClientTimestampAccepted: accepted, TimestampUncertaintyNanos: (after - before + 1) / 2}
	// Every active episode that selected the applications source receives the
	// record on its own timeline; episodes that excluded it are skipped.
	recorded := false
	for _, a := range m.active {
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
	m.preRoll = append(m.preRoll, bufferedRecord{bootNanos: stamp, encoded: b})
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

func (m *Manager) evictPreRoll(now int64) {
	cutoff := now - preRollWindow.Nanoseconds()
	for len(m.preRoll) > 0 && (m.preRoll[0].bootNanos < cutoff || m.preRollBytes > preRollLimit) {
		m.preRollBytes -= len(m.preRoll[0].encoded)
		m.preRoll = m.preRoll[1:]
		m.preRollLost++
	}
}

// flushPreRoll copies the buffered records inside the requested window into a
// starting episode. The ring buffer is not consumed: with per-campaign
// concurrency another campaign's later episode is entitled to the same
// pre-trigger records on its own timeline.
func (m *Manager) flushPreRoll(dir string, origin int64, requested time.Duration) (uint64, *int64, error) {
	window := preRollWindow
	if requested > 0 && requested < window {
		window = requested
	}
	cutoff := origin - window.Nanoseconds()
	var count uint64
	var earliest *int64
	for _, r := range m.preRoll {
		if r.bootNanos < cutoff || r.bootNanos > origin {
			continue
		}
		var stored storedApplicationRecord
		if err := json.Unmarshal(r.encoded, &stored); err != nil {
			continue
		}
		stored.EpisodeNanos = r.bootNanos - origin
		if earliest == nil || stored.EpisodeNanos < *earliest {
			value := stored.EpisodeNanos
			earliest = &value
		}
		b, _ := json.Marshal(stored)
		if err := appendJSONL(filepath.Join(dir, "events.jsonl"), b); err != nil {
			return count, earliest, err
		}
		count++
	}
	return count, earliest, nil
}
func appendJSONL(path string, b []byte) error {
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if e != nil {
		return e
	}
	defer f.Close()
	if _, e = f.Write(append(b, '\n')); e != nil {
		return e
	}
	return f.Sync()
}
func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// Stop finalizes the episode keyed by the given campaign name
// (AdHocEpisodeKey for campaign-less episodes).
func (m *Manager) Stop(key string) (Manifest, error) {
	m.mu.Lock()
	a := m.active[key]
	if a == nil {
		m.mu.Unlock()
		return Manifest{}, ErrNoActiveEpisode
	}
	if a.cancel != nil {
		a.cancel()
	}
	m.mu.Unlock()
	if a.done != nil {
		<-a.done
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active[key] != a {
		return Manifest{}, ErrNoActiveEpisode
	}
	return m.finalizeLocked(a, "complete", "")
}

func (m *Manager) finalizeLocked(a *activeEpisode, state, reason string) (Manifest, error) {
	now, err := readBootTime()
	if err != nil {
		return Manifest{}, err
	}
	a.manifest.StoppedEpisodeNS = now - a.manifest.RequestBootNanos
	if obs, err := observeUTC(a.manifest.RequestBootNanos, "system_reported", "linux_realtime_sandwich"); err == nil {
		a.manifest.UTCObservations = append(a.manifest.UTCObservations, obs)
	}
	if m.consensus != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, queryErr := m.consensus(ctx)
		cancel()
		if queryErr == nil {
			a.manifest.Roughtime = append(a.manifest.Roughtime, c)
			if len(a.manifest.UTCObservations) > 0 && clockAgreement(a.manifest.UTCObservations[len(a.manifest.UTCObservations)-1], c) == "conflict" {
				a.manifest.SystemClockStatus = "conflict"
			}
		}
	}
	a.manifest.State, a.manifest.Interruption = state, reason
	// The model-input ledger is appended without per-sample fsync, so it must
	// reach the disk before its checksum is taken.
	if a.modelInputs != nil {
		syncErr := a.modelInputs.Sync()
		closeErr := a.modelInputs.Close()
		a.modelInputs = nil
		if err := errors.Join(syncErr, closeErr); err != nil {
			return Manifest{}, err
		}
	}
	files, err := sealFiles(a.dir)
	if err != nil {
		return Manifest{}, err
	}
	associateFileSources(files, a.manifest.Sources)
	a.manifest.Files = files
	if err := writeManifest(a.dir, a.manifest); err != nil {
		return Manifest{}, err
	}
	final := strings.TrimSuffix(a.dir, ".partial")
	if err := os.Rename(a.dir, final); err != nil {
		return Manifest{}, err
	}
	delete(m.active, a.key)
	return a.manifest, nil
}

// Status reports one active episode for status displays: the ad-hoc episode
// when present, otherwise the earliest-started campaign episode. Use
// ActiveEpisodeKeys and ActiveSession to enumerate concurrent episodes.
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
	if earliest == nil {
		return nil
	}
	v := snapshotManifest(earliest.manifest)
	return &v
}

// EpisodesAwaitingUpload returns the full manifests of finalized episodes whose
// upload workflow still needs work: "pending" (queued) or "uploading" (left
// mid-transfer by a crashed worker and safe to resume). The manifest is the
// single source of truth for upload progress; the transfer worker consumes this
// queue and calls UpdateUploadState to persist each transition. Episodes are
// returned oldest-first so the backlog drains in capture order.
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
	if id == "" || safeName(id) != id {
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
		mf, err := readManifest(dir)
		if err != nil {
			continue
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
			return fmt.Errorf("recovering episode %s: reconciling model input/outcome accounting: %w", mf.ID, reconcileErr)
		}
		if reconciled {
			mf.RecoveryActions = append(mf.RecoveryActions, "recomputed model input/outcome counters from "+ModelInputLedgerFile+" and "+mf.ModelIO.OutcomeLog)
		}
		mf.Files, err = sealFiles(dir)
		if err != nil {
			return fmt.Errorf("recovering episode %s: %w", mf.ID, err)
		}
		associateFileSources(mf.Files, mf.Sources)
		if err := writeManifest(dir, mf); err != nil {
			return err
		}
		if err := os.Rename(dir, strings.TrimSuffix(dir, ".partial")); err != nil {
			return err
		}
	}
	return nil
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
		out = append(out, File{Path: rel, Size: n, SHA256: h, SourceID: sourceForPath(rel), Format: format, MediaType: mediaType})
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

func sourceForPath(path string) string {
	if path == "events.jsonl" {
		return "applications"
	}
	if path == "telemetry.jsonl" {
		return "telemetry"
	}
	parts := strings.Split(path, "/")
	if len(parts) >= 2 && (parts[0] == "cameras" || parts[0] == "ros2" || parts[0] == "audio") {
		return parts[1]
	}
	return ""
}

func associateFileSources(files []File, sources []SourceStats) {
	for i := range files {
		if files[i].SourceID == "applications" || files[i].SourceID == "telemetry" {
			continue
		}
		path := strings.TrimPrefix(files[i].Path, "cameras/")
		path = strings.TrimPrefix(path, "ros2/")
		path = strings.TrimPrefix(path, "audio/")
		for _, stats := range sources {
			encoded := safeName(stats.Source.ID)
			if path == encoded || path == encoded+".calibration" || strings.HasPrefix(path, encoded+"/") || strings.HasPrefix(path, encoded+"-") {
				files[i].SourceID = stats.Source.ID
				break
			}
		}
	}
}
func writeManifest(dir string, m Manifest) error {
	b, e := json.MarshalIndent(m, "", "  ")
	if e != nil {
		return e
	}
	b = append(b, '\n')
	tmp := filepath.Join(dir, "manifest.json.tmp")
	if e = os.WriteFile(tmp, b, 0o640); e != nil {
		return e
	}
	return os.Rename(tmp, filepath.Join(dir, "manifest.json"))
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
func truncateJSONL(p string) {
	b, e := os.ReadFile(p)
	if e != nil {
		return
	}
	i := strings.LastIndexByte(string(b), '\n')
	if i < 0 {
		_ = os.Truncate(p, 0)
		return
	}
	_ = os.Truncate(p, int64(i+1))
}

func clockAgreement(system UTCObservation, consensus timesync.Consensus) string {
	if consensus.Confidence == "unbounded" {
		return "system_reported"
	}
	if system.OffsetUpperNanos < consensus.LowerOffsetNanos || consensus.UpperOffsetNanos < system.OffsetLowerNanos {
		return "conflict"
	}
	return "agreement"
}
