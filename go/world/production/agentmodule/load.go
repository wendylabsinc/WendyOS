package agentmodule

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/appservice"
	"github.com/wendylabsinc/wendy/go/world/production/authenticatedtime"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/simclock"
	"golang.org/x/sys/unix"
)

type FileConfig struct {
	Policy              Policy                 `json:"policy"`
	RootsDER            [][]byte               `json:"roots_der"`
	SocketDirectory     string                 `json:"socket_directory"`
	JournalPath         string                 `json:"journal_path"`
	ProofArchivePath    string                 `json:"proof_archive_path,omitempty"`
	SimulationClockFile string                 `json:"simulation_clock_file,omitempty"`
	SimulationClock     *SimulationClockConfig `json:"simulation_clock,omitempty"`
	EvidenceDirectory   string                 `json:"evidence_directory,omitempty"`
	Limits              appservice.Limits      `json:"limits,omitempty"`
	Replication         *ReplicaConfig         `json:"replication,omitempty"`
	NativeCameras       []NativeCameraBinding  `json:"native_cameras,omitempty"`
}

type SimulationClockConfig struct {
	URL           string         `json:"url"`
	ReadTokenFile string         `json:"read_token_file"`
	RunID         string         `json:"run_id"`
	BootID        string         `json:"boot_id"`
	PackageSHA256 string         `json:"package_sha256"`
	Frame         world.FrameRef `json:"frame"`
	AnchorUnixNS  int64          `json:"anchor_unix_ns"`
}

// ReadCommissionedFile requires an owner-controlled, bounded regular file.
// The agent runs as root; apps cannot replace its commissioned configuration.
func ReadCommissionedFile(path string, limit int) ([]byte, error) {
	return readCommissionedFile(path, limit, false)
}
func readCommissionedFile(path string, limit int, private bool) ([]byte, error) {
	if limit < 1 || limit > 256<<10 || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, ErrDenied
	}
	// Traverse from an anchored descriptor. O_NOFOLLOW on the final file
	// alone would still allow an ancestor to be replaced with a symlink.
	parent, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(parent) }()
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for _, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(parent, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		var st unix.Stat_t
		err = unix.Fstat(next, &st)
		// Root-owned sticky temporary directories are safe to traverse: the
		// next descriptor pins the owned child and other users cannot rename it.
		trusted := st.Uid == 0 || st.Uid == uint32(os.Geteuid())
		protected := st.Mode&0022 == 0 || st.Uid == 0 && st.Mode&unix.S_ISVTX != 0
		if err != nil || !trusted || !protected {
			_ = unix.Close(next)
			return nil, ErrDenied
		}
		_ = unix.Close(parent)
		parent = next
	}
	fd, err := unix.Openat(parent, parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "world-commissioning")
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != uint32(os.Geteuid()) || st.Mode&0022 != 0 {
		return nil, ErrDenied
	}
	if private && st.Mode&0077 != 0 {
		return nil, ErrDenied
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil || len(b) > limit {
		return nil, ErrDenied
	}
	return b, nil
}

// Load is the real agent startup path. An absent optional configuration leaves
// World unavailable. It acquires authenticated physical time independently;
// neither the machine wall clock nor the simulation clock can replace it.
func Load(ctx context.Context, path string, identity Identity, peer appservice.PeerCheck) (*Manager, error) {
	return LoadWithOptions(ctx, path, identity, peer, HostOptions{})
}

// Initialize creates the journal once, separately from normal startup. It
// cannot sign, expose a listener or recover a missing previously created DB.
// Existing/incomplete journal state is refused by servicejournal.Create.
func Initialize(ctx context.Context, path string) error {
	m, err := load(ctx, path, unavailableIdentity{}, func(context.Context, appservice.Peer, serviceorigin.Scope) error { return ErrDenied }, true, HostOptions{})
	if err != nil {
		return err
	}
	if m == nil {
		return ErrDenied
	}
	return m.Close()
}

type unavailableIdentity struct{}

func (unavailableIdentity) Snapshot(context.Context) (IdentitySnapshot, error) {
	return IdentitySnapshot{}, ErrDenied
}
func (unavailableIdentity) Current(uint64) bool                    { return false }
func (unavailableIdentity) WithCurrent(uint64, func() error) error { return ErrDenied }

func load(ctx context.Context, path string, identity Identity, peer appservice.PeerCheck, initialize bool, options HostOptions) (*Manager, error) {
	raw, err := ReadCommissionedFile(path, 256<<10)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f FileConfig
	if jws.DecodeStrict(raw, &f) != nil || f.Policy.validateWithReplication(f.Replication) != nil || f.Replication.validate(f.Policy, f.JournalPath, f.ProofArchivePath, f.EvidenceDirectory) != nil || appservice.ValidateLimits(f.Limits) != nil || validateNativeCameras(f.Policy, f.NativeCameras) != nil {
		return nil, ErrDenied
	}
	if f.Policy.ExecutionDomain == "simulation" && ((f.SimulationClockFile == "") == (f.SimulationClock == nil)) || f.Policy.ExecutionDomain == "physical" && (f.SimulationClockFile != "" || f.SimulationClock != nil) {
		return nil, ErrDenied
	}
	if f.ProofArchivePath != "" && (!filepath.IsAbs(f.ProofArchivePath) || filepath.Clean(f.ProofArchivePath) != f.ProofArchivePath) {
		return nil, ErrDenied
	}
	if f.EvidenceDirectory != "" && (!filepath.IsAbs(f.EvidenceDirectory) || filepath.Clean(f.EvidenceDirectory) != f.EvidenceDirectory) {
		return nil, ErrDenied
	}
	var tokenBytes []byte
	if f.SimulationClock != nil {
		c := f.SimulationClock
		validFrame := false
		for _, frame := range f.Policy.Frames {
			if frame == c.Frame {
				validFrame = true
				break
			}
		}
		if !validFrame {
			return nil, ErrDenied
		}
		tokenBytes, err = readCommissionedFile(c.ReadTokenFile, 4096, true)
		if err != nil {
			return nil, ErrDenied
		}
	}
	tokenDigest := sha256.Sum256(tokenBytes)
	for _, p := range []string{f.SocketDirectory, f.JournalPath} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return nil, ErrDenied
		}
	}
	current := commissionedCurrent(path, raw, f.SimulationClock, tokenDigest)
	clock, err := authenticatedtime.AcquireStartup(ctx, authenticatedtime.Config{Servers: authenticatedtime.WendyServers(), QueryTimeout: 3 * time.Second, MaxHoldover: 10 * time.Minute, DriftPPM: 100, MaxWidth: 5 * time.Second})
	if err != nil {
		return nil, err
	}
	now := func() time.Time {
		w, e := clock.Now()
		if e != nil {
			return time.Time{}
		}
		return w.Latest
	}
	var closeSample func() error
	var captureSource *CaptureSource
	if f.SimulationClock != nil {
		c := f.SimulationClock
		provider, e := simclock.New(simclock.Config{URL: c.URL, Token: strings.TrimSpace(string(tokenBytes)), RunID: c.RunID, BootID: c.BootID, PackageSHA256: c.PackageSHA256, Frame: c.Frame, Domain: f.Policy.SampleClockDomain, Anchor: time.Unix(0, c.AnchorUnixNS), Physical: clock})
		if e != nil {
			return nil, e
		}
		now = func() time.Time {
			t, e := provider.Now()
			if e != nil {
				return time.Time{}
			}
			return t
		}
		closeSample = provider.Close
		captureSource = &CaptureSource{RunID: c.RunID, BootID: c.BootID, PackageSHA256: c.PackageSHA256, AnchorUnixNS: c.AnchorUnixNS, Frame: c.Frame}
	} else if f.Policy.ExecutionDomain == "simulation" {
		// This source is an explicitly commissioned simulation input only. It
		// supplies sample age, never certificate validity or device authority.
		now = func() time.Time {
			b, e := ReadCommissionedFile(f.SimulationClockFile, 4096)
			if e != nil {
				return time.Time{}
			}
			var c struct {
				Domain string    `json:"domain"`
				Now    time.Time `json:"now"`
			}
			if jws.DecodeStrict(b, &c) != nil || c.Domain != f.Policy.SampleClockDomain {
				return time.Time{}
			}
			return c.Now
		}
	}
	m, err := New(ctx, Config{Policy: f.Policy, Roots: f.RootsDER, Directory: f.SocketDirectory, JournalPath: f.JournalPath, ProofArchivePath: f.ProofArchivePath, CreateProofArchive: initialize && f.ProofArchivePath != "", EvidenceDirectory: f.EvidenceDirectory, CaptureSource: captureSource, NativeCameras: f.NativeCameras, CreateJournal: initialize, Replication: f.Replication, CreateReplication: initialize && f.Replication != nil, UID: os.Geteuid(), GID: 2000, Limits: f.Limits}, Dependencies{Trace: options.Trace, Identity: identity, Clock: clock, SampleNow: now, SampleClose: closeSample, PeerCheck: peer, PolicyCurrent: current})
	if err != nil && closeSample != nil {
		_ = closeSample()
	}
	return m, err
}

// The entire commissioned file is immutable during a module incarnation,
// including optional archive paths and resource budgets.
func commissionedCurrent(path string, original []byte, simulation *SimulationClockConfig, tokenDigest [32]byte) func(context.Context) error {
	digest := sha256.Sum256(original)
	return func(ctx context.Context) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		b, e := ReadCommissionedFile(path, 256<<10)
		if e != nil || sha256.Sum256(b) != digest {
			return ErrDenied
		}
		if simulation != nil {
			token, e := readCommissionedFile(simulation.ReadTokenFile, 4096, true)
			if e != nil || sha256.Sum256(token) != tokenDigest {
				return ErrDenied
			}
		}
		return nil
	}
}
