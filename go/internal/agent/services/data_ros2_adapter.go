package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"golang.org/x/sys/unix"
)

// ROS 2 source identifiers are defined by data.ParseROS2SourceID and its
// siblings: "ros2:<rmw>:domain-<n>" for a whole DDS domain and
// "ros2:<rmw>:domain-<n>:<topic>" for one topic on it. The grammar and the
// reasoning behind it live in the data package because the campaign resolver
// matches against the same identifiers.

// ros2ClockDomain is unchanged from what the domain-level source has always
// reported: a per-topic source is a narrower selection of the same rosbag2
// recording, not a different clock.
const ros2ClockDomain = "ROSBAG2_STORAGE/ROS_MESSAGE_HEADER/SIM_TIME"

// ros2DiscoveryTTL bounds how long one `ros2 topic list -t` result is reused.
//
// Measured against a live graph on hardware, that enumeration costs about 0.6
// seconds beyond the bare `ros2` command-line baseline, and the cost does not
// grow with the number of topics. Discover is not called once per user action:
// rendering `wendy data sources` calls it, and a single campaign trigger calls
// it twice more, once through ResolveCampaignSources and again through
// Manager.Start. Uncached, that puts roughly 1.8 seconds of DDS discovery
// between a trigger firing and the recorder starting, and the episode does not
// capture what happened during it.
//
// Five seconds collapses each of those bursts into one enumeration while
// keeping the listing fresh enough that a node started by hand appears in the
// next `wendy data sources` a person types. The time to live is a bound on
// staleness, not a substitute for correctness: the cache is keyed by sidecar
// identity, so a sidecar restart, an RMW change or a domain override misses
// immediately, and invalidateDiscovery drops every entry outright when the
// adapter learns the graph moved under it.
const ros2DiscoveryTTL = 5 * time.Second

// ros2DiscoveryDetailLimit truncates an enumeration failure before it is put in
// a source Detail, which the CLI renders in a table column.
const ros2DiscoveryDetailLimit = 200

type ros2DataAdapter struct {
	service *ROS2Service
	// now is time.Now, replaced by tests that need to step over the cache TTL
	// without sleeping.
	now func() time.Time

	mu    sync.Mutex
	cache map[string]ros2DiscoveryEntry
}

// ros2DiscoveryEntry is one sidecar's enumerated sources and their expiry.
// Failures are cached too: a domain whose topic listing is failing tends to
// keep failing, and re-running a slow failing exec on every Discover is how a
// dead DDS domain turns `wendy data sources` into a hang.
type ros2DiscoveryEntry struct {
	sources []data.Source
	expires time.Time
}

func newROS2DataAdapter(service *ROS2Service) dataCaptureAdapter {
	return newROS2Adapter(service)
}

// newROS2Adapter returns the concrete adapter. Tests use it to step the clock
// over the discovery TTL and to reach invalidateDiscovery.
func newROS2Adapter(service *ROS2Service) *ros2DataAdapter {
	return &ros2DataAdapter{service: service, now: time.Now, cache: map[string]ros2DiscoveryEntry{}}
}

// ros2SidecarKey identifies the graph an enumeration result belongs to. The
// domain is part of the key because resolveSidecars applies a --domain
// override to the same sidecar.
func ros2SidecarKey(sc ros2SC) string {
	return sc.name + "\x00" + sc.rmw + "\x00" + strconv.Itoa(sc.domainID)
}

// ros2DomainSourceID is the identifier for the whole DDS domain behind sc.
func ros2DomainSourceID(sc ros2SC) string {
	return data.ROS2DomainSourceID(sc.rmw, sc.domainID)
}

// ros2DomainSource describes the whole domain. Healthy is derived, never
// asserted: see enumerate.
func ros2DomainSource(sc ros2SC, healthy bool, detail string) data.Source {
	return data.Source{
		ID:          ros2DomainSourceID(sc),
		Kind:        "ros2",
		ClockDomain: ros2ClockDomain,
		Healthy:     healthy,
		Detail:      detail,
	}
}

// ros2TopicSource describes one topic. Detail is the message type because that
// is what `wendy data sources` prints in its DETAIL column, and the type is
// the one fact that tells a person whether this is the topic they meant.
//
// Subscribable is not set here and must stay false: it reports whether a
// provider can multiplex the source to an in-agent consumer, and
// ROS 2 has no producer hub, so a topic can be captured into an episode but
// not streamed to an app. Making it true would advertise a stream that
// Subscribe would then refuse.
func ros2TopicSource(sc ros2SC, topic *agentpbv2.ROS2Topic) data.Source {
	detail := strings.Join(topic.GetTypes(), ", ")
	if detail == "" {
		// `ros2 topic list -t` prints the type for every topic it lists, so an
		// empty one means the line did not parse. Say so rather than printing
		// an empty column that reads like "no type".
		detail = "message type unreported by ros2 topic list"
	}
	return data.Source{
		ID:          data.ROS2TopicSourceID(sc.rmw, sc.domainID, topic.GetName()),
		Kind:        "ros2",
		ClockDomain: ros2ClockDomain,
		Healthy:     true,
		Detail:      detail,
	}
}

func (a *ros2DataAdapter) Discover(ctx context.Context) []data.Source {
	scs, err := a.service.resolveSidecars(ctx, nil)
	if err != nil {
		return nil
	}
	out := make([]data.Source, 0, len(scs)*8)
	live := make(map[string]bool, len(scs))
	for _, sc := range scs {
		live[ros2SidecarKey(sc)] = true
		out = append(out, a.discoverSidecar(ctx, sc)...)
	}
	a.evictAllBut(live)
	return out
}

// discoverSidecar enumerates one graph, through the cache.
func (a *ros2DataAdapter) discoverSidecar(ctx context.Context, sc ros2SC) []data.Source {
	key := ros2SidecarKey(sc)
	if cached, ok := a.cached(key); ok {
		return cached
	}
	sources := a.enumerate(ctx, sc)
	a.store(key, sources)
	return sources
}

// enumerate runs the one command that answers the whole question. `ros2 topic
// list -t` returns every topic name with its message type in a single exec, so
// there is no second enumerator here.
//
// Publisher and subscriber counts are deliberately not fetched. That path runs
// `ros2 topic info` once per topic, bounded at ros2TopicInfoConcurrency, and
// measures at roughly 0.8 seconds per round, so it costs ten seconds or more
// on a robot with a hundred topics and tells a person nothing they need in
// order to choose what to record.
func (a *ros2DataAdapter) enumerate(ctx context.Context, sc ros2SC) []data.Source {
	out, err := a.service.runIn(ctx, sc, "topic", "list", "-t")
	if err != nil {
		// Healthy used to be hardcoded true, so a DDS domain that answered
		// nothing still enumerated as healthy and a campaign naming it started
		// a recorder that captured nothing. Enumeration is the signal: a graph
		// that cannot list its own topics cannot be recorded from either, and
		// the reason belongs in the Detail rather than in a log nobody reads.
		return []data.Source{ros2DomainSource(sc, false, fmt.Sprintf(
			"%s DDS domain %d: listing topics failed: %s",
			a.rmwLabel(sc), sc.domainID, truncateROS2Detail(err.Error()),
		))}
	}
	topics := parseROS2TopicList(out)
	sources := make([]data.Source, 0, len(topics)+1)
	// The domain-level source stays listed. It is the handle for "record
	// everything on this graph", it is what a campaign written before per-topic
	// sources existed names, and without it the default episode (no explicit
	// source list) would start one rosbag2 process per topic instead of one.
	sources = append(sources, ros2DomainSource(sc, true, fmt.Sprintf(
		"%s DDS domain %d, %d topics", a.rmwLabel(sc), sc.domainID, len(topics),
	)))
	// `ros2 topic list -t` should not repeat a name, but a duplicate would mint
	// two sources with the same id, and selectSources would then match one
	// campaign entry against both.
	seen := make(map[string]bool, len(topics))
	for _, topic := range topics {
		name := topic.GetName()
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		sources = append(sources, ros2TopicSource(sc, topic))
	}
	return sources
}

func (a *ros2DataAdapter) rmwLabel(sc ros2SC) string {
	if sc.rmw == "" {
		return "default"
	}
	return sc.rmw
}

func truncateROS2Detail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= ros2DiscoveryDetailLimit {
		return s
	}
	// Cut on a rune boundary: the Detail is serialized as JSON and a half
	// rune would make the whole response invalid UTF-8.
	cut := ros2DiscoveryDetailLimit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

func (a *ros2DataAdapter) cached(key string) ([]data.Source, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.cache[key]
	if !ok || !a.now().Before(entry.expires) {
		return nil, false
	}
	return append([]data.Source(nil), entry.sources...), true
}

func (a *ros2DataAdapter) store(key string, sources []data.Source) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cache[key] = ros2DiscoveryEntry{sources: append([]data.Source(nil), sources...), expires: a.now().Add(ros2DiscoveryTTL)}
}

// evictAllBut drops entries for sidecars that are no longer live so a device
// that cycles through RMWs or domains does not grow the map forever.
func (a *ros2DataAdapter) evictAllBut(live map[string]bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key := range a.cache {
		if !live[key] {
			delete(a.cache, key)
		}
	}
}

// invalidateDiscovery is the explicit invalidation path for the TTL cache. It
// is called when the adapter learns the graph is not what it enumerated, so
// the next Discover re-runs `ros2 topic list -t` instead of serving a listing
// that has already been shown to be wrong.
func (a *ros2DataAdapter) invalidateDiscovery() {
	a.mu.Lock()
	defer a.mu.Unlock()
	clear(a.cache)
}

// ros2DomainSelection is the set of sources one episode selected on a single
// DDS domain, reduced to one recorder invocation.
type ros2DomainSelection struct {
	// wholeDomain is set when the domain-level identifier was selected. It
	// wins over any per-topic identifier on the same domain: `-a` already
	// records those topics, and the alternative is two rosbag2 processes
	// writing the same messages into two bags in one episode.
	wholeDomain bool
	sources     []data.Source
	topics      []string
}

func (a *ros2DataAdapter) Start(ctx context.Context, session data.CaptureSession, selected []data.Source) (runningDataCapture, error) {
	wanted := map[string]*ros2DomainSelection{}
	for _, source := range selected {
		if !strings.HasPrefix(source.ID, data.ROS2SourcePrefix) {
			continue
		}
		domainID, topic, ok := data.ParseROS2SourceID(source.ID)
		if !ok {
			return nil, fmt.Errorf("unrecognized ROS 2 source id %q", source.ID)
		}
		selection := wanted[domainID]
		if selection == nil {
			selection = &ros2DomainSelection{}
			wanted[domainID] = selection
		}
		selection.sources = append(selection.sources, source)
		if topic == "" {
			selection.wholeDomain = true
		} else {
			selection.topics = append(selection.topics, topic)
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}
	scs, err := a.service.resolveSidecars(ctx, nil)
	if err != nil {
		return nil, err
	}
	group := &ros2CaptureGroup{}
	for _, sc := range scs {
		domainID := ros2DomainSourceID(sc)
		selection, ok := wanted[domainID]
		if !ok {
			continue
		}
		capture, err := a.startOne(ctx, session, sc, selection)
		if err != nil {
			_, _ = group.Stop(context.Background())
			return nil, fmt.Errorf("%s: %w", domainID, err)
		}
		group.captures = append(group.captures, capture)
		delete(wanted, domainID)
	}
	if len(wanted) != 0 {
		_, _ = group.Stop(context.Background())
		// The cached listing described a graph that is no longer there, so it
		// must not be served to the next caller.
		a.invalidateDiscovery()
		return nil, errors.New("ROS 2 graph changed during recorder startup")
	}
	return group, nil
}

// ros2RecordArgs builds the rosbag2 command line for one domain.
//
// One invocation records every selected topic on the domain rather than one
// invocation per topic. Each rosbag2 process pays its own DDS discovery, opens
// its own storage writer and produces its own bag directory, and this adapter
// pairs each recorder with a clock sampler and one ClockMapping. Splitting a
// domain across N recorders would therefore produce N bags and N independent
// clock mappings for messages that share a single timeline, which is exactly
// the alignment the episode exists to preserve.
//
// The topic names become argv elements of the rosbag2 exec, so they are an
// argument-injection surface. They cannot be read as flags: every one of them
// arrives through parseROS2TopicList, which keeps only lines beginning with
// "/", and again through data.ParseROS2SourceID, which rejects an identifier
// whose topic is not absolute (SOC2-CC6, ISO27001-A.8, NIST-SI-10). There is
// no shell in the path either; ExecROS2 passes argv straight through.
func ros2RecordArgs(staging string, selection *ros2DomainSelection) []string {
	args := []string{"bag", "record", "-o", staging}
	if selection.wholeDomain {
		return append(args, "-a")
	}
	return append(args, sortedUniqueStrings(selection.topics)...)
}

func sortedUniqueStrings(in []string) []string {
	out := append([]string(nil), in...)
	slices.Sort(out)
	return slices.Compact(out)
}

func (a *ros2DataAdapter) startOne(ctx context.Context, session data.CaptureSession, sc ros2SC, selection *ros2DomainSelection) (*ros2Capture, error) {
	// Paths are keyed by the domain, not by any one selected source: a domain
	// yields exactly one bag per episode, whether it was selected whole or by
	// a list of topics, and a topic name is not a safe path component.
	key := ros2DomainSourceID(sc)
	name := safeCaptureName("wendy-" + session.ID + "-" + key)
	staging := filepath.Join(a.service.bagDir, name)
	destination := filepath.Join(session.Directory, "ros2", safeCaptureName(key))
	if _, err := os.Stat(staging); err == nil {
		return nil, fmt.Errorf("staging bag %s already exists", name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return nil, err
	}
	clockFile, err := os.OpenFile(filepath.Join(session.Directory, "ros2", safeCaptureName(key)+"-clock_samples.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, err
	}
	recordCtx, cancel := context.WithCancel(context.Background())
	c := &ros2Capture{service: a.service, session: session, sources: append([]data.Source(nil), selection.sources...), sc: sc, staging: staging, destination: destination, clockFile: clockFile, ctx: recordCtx, cancel: cancel, recordDone: make(chan ros2ExecResult, 1), samplerDone: make(chan struct{})}
	args := ros2RecordArgs(staging, selection)
	go func() {
		var output bytes.Buffer
		code, execErr := a.service.runtime.ExecROS2(recordCtx, ROS2ExecOptions{DomainID: sc.domainID, SidecarName: sc.name, Args: args}, &output, &output)
		c.recordDone <- ros2ExecResult{code: code, err: execErr, output: output.String()}
	}()
	go c.sampleClocks()

	select {
	case result := <-c.recordDone:
		cancel()
		<-c.samplerDone
		clockFile.Close()
		return nil, fmt.Errorf("rosbag2 exited before recording (code %d): %s", result.code, summarizeROS2Output(result.err, result.output))
	case <-ctx.Done():
		cancel()
		result := <-c.recordDone
		<-c.samplerDone
		clockFile.Close()
		return nil, errors.Join(ctx.Err(), result.err)
	case <-time.After(750 * time.Millisecond):
		return c, nil
	}
}

type ros2ExecResult struct {
	code   int
	err    error
	output string
}

type ros2CaptureGroup struct{ captures []*ros2Capture }

func (g *ros2CaptureGroup) Stop(ctx context.Context) ([]data.CaptureResult, error) {
	var out []data.CaptureResult
	var errs []error
	for i := len(g.captures) - 1; i >= 0; i-- {
		r, err := g.captures[i].Stop(ctx)
		out = append(out, r...)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return out, errors.Join(errs...)
}

type ros2Capture struct {
	service *ROS2Service
	session data.CaptureSession
	// sources are every episode source this one recorder covers: the
	// domain-level source, one or more topic sources, or both. Each gets its
	// own CaptureResult so the manifest accounts for what the campaign named.
	sources              []data.Source
	sc                   ros2SC
	staging, destination string
	clockFile            *os.File
	clockMu              sync.Mutex
	ctx                  context.Context
	cancel               context.CancelFunc
	recordDone           chan ros2ExecResult
	samplerDone          chan struct{}
	stopOnce             sync.Once
	stopResult           []data.CaptureResult
	stopErr              error
	samples              uint64
	maxError             int64
	discontinuities      uint64
	simClock             bool
}

func (c *ros2Capture) writeClockSample(value any) {
	b, _ := json.Marshal(value)
	c.clockMu.Lock()
	if _, err := c.clockFile.Write(append(b, '\n')); err == nil {
		c.samples++
	}
	c.clockMu.Unlock()
}

func (c *ros2Capture) sampleClocks() {
	defer close(c.samplerDone)
	// Presence of /clock changes the interpretation of message header stamps,
	// but never turns them into UTC. Preserve its raw sequence with receipt time.
	if topics, err := c.service.runIn(c.ctx, c.sc, "topic", "list"); err == nil {
		for _, topic := range strings.Fields(topics) {
			if topic == "/clock" {
				c.simClock = true
				break
			}
		}
	}
	var simDone chan struct{}
	if c.simClock && c.ctx.Err() == nil {
		simDone = make(chan struct{})
		go func() {
			defer close(simDone)
			writer := &rosClockWriter{capture: c}
			_, _ = c.service.runtime.ExecROS2(c.ctx, ROS2ExecOptions{DomainID: c.sc.domainID, SidecarName: c.sc.name, Args: []string{"topic", "echo", "/clock"}}, writer, io.Discard)
		}()
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		sample, err := data.CaptureUTCClockSample()
		if err == nil {
			errNanos := (sample.BootAfterNanos - sample.BootBeforeNanos + 1) / 2
			if errNanos > c.maxError {
				c.maxError = errNanos
			}
			c.writeClockSample(struct {
				Kind         string           `json:"kind"`
				EpisodeNanos int64            `json:"episode_nanos"`
				Sample       data.ClockSample `json:"sample"`
			}{"host_realtime_sandwich", sample.BootBeforeNanos + (sample.BootAfterNanos-sample.BootBeforeNanos)/2 - c.session.RequestBootNanos, sample})
		}
		select {
		case <-c.ctx.Done():
			if simDone != nil {
				<-simDone
			}
			return
		case <-ticker.C:
		}
	}
}

type rosClockWriter struct {
	capture  *ros2Capture
	mu       sync.Mutex
	buf      string
	sec      int64
	haveSec  bool
	last     int64
	haveLast bool
}

func (w *rosClockWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf += string(p)
	for {
		i := strings.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(w.buf[:i])
		w.buf = w.buf[i+1:]
		if strings.HasPrefix(line, "sec:") {
			w.sec, _ = strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "sec:")), 10, 64)
			w.haveSec = true
		}
		if strings.HasPrefix(line, "nanosec:") && w.haveSec {
			nsec, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "nanosec:")), 10, 64)
			if err != nil {
				continue
			}
			stamp := w.sec*int64(time.Second) + nsec
			if w.haveLast && stamp < w.last {
				w.capture.discontinuities++
			}
			w.last, w.haveLast = stamp, true
			_, receipt, _, _ := data.CaptureReceipt()
			w.capture.writeClockSample(struct {
				Kind         string `json:"kind"`
				EpisodeNanos int64  `json:"episode_nanos"`
				SimTimeNanos int64  `json:"sim_time_nanos"`
			}{"ros_clock", receipt - w.capture.session.RequestBootNanos, stamp})
		}
	}
	return len(p), nil
}

func (c *ros2Capture) Stop(context.Context) ([]data.CaptureResult, error) {
	c.stopOnce.Do(func() {
		c.cancel()
		execResult := <-c.recordDone
		<-c.samplerDone
		c.clockMu.Lock()
		_ = c.clockFile.Sync()
		_ = c.clockFile.Close()
		c.clockMu.Unlock()
		if execResult.err != nil && !strings.Contains(execResult.err.Error(), context.Canceled.Error()) {
			c.stopErr = fmt.Errorf("rosbag2 stopped with code %d: %s", execResult.code, summarizeROS2Output(execResult.err, execResult.output))
		}
		if _, statErr := os.Stat(c.staging); statErr != nil {
			c.stopErr = errors.Join(c.stopErr, fmt.Errorf("rosbag2 output missing: %w", statErr))
		} else if moveErr := moveCaptureDirectory(c.staging, c.destination); moveErr != nil {
			c.stopErr = errors.Join(c.stopErr, moveErr)
		}
		mapping := data.ClockMapping{ID: "ros-host-realtime-sandwich-1", SourceClockDomain: "ROSBAG2_STORAGE_TIME", CanonicalClock: "CLOCK_BOOTTIME", MaxErrorNanos: c.maxError, Samples: c.samples, Algorithm: "sampled-realtime-boottime-sandwich-v1"}
		if c.simClock {
			mapping.Discontinuity = "ROS /clock retained as an independent, potentially resetting domain"
		}
		// One result per selected source, sharing this recorder's clock
		// mapping. SourceDetail is left empty on purpose: it would overwrite
		// the discovered Detail in the manifest, and for a topic source that
		// Detail is the message type, which is worth more there than a note
		// about which bag holds it.
		c.stopResult = make([]data.CaptureResult, 0, len(c.sources))
		for _, source := range c.sources {
			c.stopResult = append(c.stopResult, data.CaptureResult{SourceID: source.ID, DropAccounting: "unavailable", Discontinuities: c.discontinuities, Mappings: []data.ClockMapping{mapping}})
		}
	})
	return c.stopResult, c.stopErr
}

func summarizeROS2Output(err error, output string) string {
	message := strings.TrimSpace(output)
	if len(message) > 512 {
		message = message[:512]
	}
	if message != "" {
		return message
	}
	if err != nil {
		return err.Error()
	}
	return "no diagnostic output"
}

func moveCaptureDirectory(source, destination string) error {
	if err := os.Rename(source, destination); err == nil {
		return nil
	} else if !errors.Is(err, unix.EXDEV) {
		return err
	}
	if err := copyCaptureDirectory(source, destination); err != nil {
		return err
	}
	return os.RemoveAll(source)
}

func copyCaptureDirectory(source, destination string) error {
	if err := os.Mkdir(destination, 0o750); err != nil {
		return err
	}
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil || rel == "." {
			return err
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.Mkdir(target, 0o750)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("rosbag contains non-regular entry %s", rel)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
		if err != nil {
			in.Close()
			return err
		}
		_, copyErr := io.Copy(out, in)
		if syncErr := out.Sync(); copyErr == nil {
			copyErr = syncErr
		}
		if closeErr := out.Close(); copyErr == nil {
			copyErr = closeErr
		}
		in.Close()
		return copyErr
	})
}
