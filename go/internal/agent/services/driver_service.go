package services

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/shared/sigverify"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// Driver add-on store on the /data partition, consumed by the OS-side
// /usr/sbin/wendyos-sysext-apply.sh (merge -> overlay -> depmod -> modprobe).
const (
	driverEnabledDir  = "/data/extensions/enabled"
	driverModulesDir  = "/data/extensions/modules-load.d"
	sysextApplyScript = "/usr/sbin/wendyos-sysext-apply.sh"
	driverSystemPath  = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

	// A self-describing add-on bakes its module list into the .raw; systemd-sysext
	// exposes it here once merged. ListDrivers falls back to this when the /data
	// override is absent, mirroring wendyos-sysext-apply's precedence.
	driverBakedModulesDir   = "/usr/lib/modules-load.d"
	driverPrivatePayloadDir = "/usr/lib/wendyos-driver-payloads"
	driverActiveStateDir    = "/usr/lib/wendyos-driver-state"
)

// maxDriverImageSize caps a driver .raw to guard against a runaway upload/download.
const maxDriverImageSize = 512 << 20 // 512 MiB

// DriverInstallSpec describes a driver add-on to install from a URL. Both the
// first-boot seed (configpartition) and the InstallDriver RPC's URL path build
// the same steps: fetch -> verify sha256 + signature -> place on /data -> apply.
type DriverInstallSpec struct {
	Name          string
	KernelVersion string
	SHA256        string
	Signature     []byte
	ArtifactURL   string
	ModulesLoad   []string
	// StageOnly stores the add-on for KernelVersion without applying it, so a
	// rebuild is in place before the OTA that boots into that kernel.
	StageOnly bool
}

// DriverService implements WendyDriverService: verify a signed driver add-on
// (.raw), store it on /data under its stable name, and apply it via the OS
// sysext-apply path. Drivers are built ahead of time in CI; the device never
// compiles them.
type DriverService struct {
	agentpbv2.UnimplementedWendyDriverServiceServer

	logger   *zap.Logger
	verifier *sigverify.Verifier

	// apply touches the shared /usr sysext overlay; serialize concurrent applies.
	mu sync.Mutex

	// requireSignature makes an install fail closed when no signing key is
	// embedded. Set for callers that are not already root-equivalent.
	requireSignature bool

	// Seams for tests.
	enabledDir        string
	modulesDir        string
	bakedModulesDir   string
	privatePayloadDir string
	activeStateDir    string
	applyScript       string
	unameR            func() string
	loadedModules     func() []string
	httpGet           func(ctx context.Context, url string) (io.ReadCloser, error)
}

// NewDriverService builds the driver service with production defaults. Signature
// verification uses the shared pinned-key verifier (a fail-safe no-op until a
// driver-signing key is embedded, matching the agent-update/container paths).
func NewDriverService(logger *zap.Logger) *DriverService {
	return &DriverService{
		logger:            logger,
		verifier:          sigverify.DefaultVerifier,
		enabledDir:        driverEnabledDir,
		modulesDir:        driverModulesDir,
		bakedModulesDir:   driverBakedModulesDir,
		privatePayloadDir: driverPrivatePayloadDir,
		activeStateDir:    driverActiveStateDir,
		applyScript:       sysextApplyScript,
		unameR:            unameRelease,
		loadedModules:     loadedKernelModules,
		httpGet:           httpGetBody,
	}
}

// NewSeedDriverService builds the service for the first-boot config-partition
// seed. That partition is unauthenticated storage — anyone who can write the
// FAT32 filesystem gets a module loaded, with no operator in the loop — so this
// path requires a real signature and fails closed while no key is embedded,
// unlike the mTLS RPC whose caller is already root-equivalent.
func NewSeedDriverService(logger *zap.Logger) *DriverService {
	s := NewDriverService(logger)
	s.requireSignature = true
	return s
}

// driverApplyStream is satisfied by both the bidi (install) and server-streaming
// (remove) gRPC stream servers, so the progress helpers work for either.
type driverApplyStream interface {
	Send(*agentpbv2.DriverApplyResponse) error
}

func sendDriverProgress(s driverApplyStream, phase string, percent int32) {
	_ = s.Send(&agentpbv2.DriverApplyResponse{
		ResponseType: &agentpbv2.DriverApplyResponse_Progress_{
			Progress: &agentpbv2.DriverApplyResponse_Progress{Phase: phase, Percent: percent},
		},
	})
}

func sendDriverFailure(s driverApplyStream, msg string) error {
	return s.Send(&agentpbv2.DriverApplyResponse{
		ResponseType: &agentpbv2.DriverApplyResponse_Failed_{
			Failed: &agentpbv2.DriverApplyResponse_Failed{ErrorMessage: msg},
		},
	})
}

func sendDriverCompleted(s driverApplyStream, name string, rebootRequired bool) error {
	return s.Send(&agentpbv2.DriverApplyResponse{
		ResponseType: &agentpbv2.DriverApplyResponse_Completed_{
			Completed: &agentpbv2.DriverApplyResponse_Completed{Name: name, RebootRequired: rebootRequired},
		},
	})
}

// InstallDriver stages a driver .raw (streamed chunks or fetched from a URL),
// verifies its sha256 + signature, places it under its stable name on /data,
// writes the modules-load config, and applies it. Progress is streamed back.
func (s *DriverService) InstallDriver(stream grpc.BidiStreamingServer[agentpbv2.InstallDriverRequest, agentpbv2.DriverApplyResponse]) error {
	// Locking happens in finalize: staging is client-paced with no server-side
	// deadline, so holding the lock here would let one stalled client wedge
	// every driver RPC.
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "no install request received: %v", err)
	}
	spec := first.GetSpec()
	if spec == nil {
		return status.Error(codes.InvalidArgument, "first InstallDriver message must carry a DriverSpec")
	}
	if err := validateDriverName(spec.GetName()); err != nil {
		return sendDriverFailure(stream, err.Error())
	}
	if err := s.checkKernel(spec.GetName(), spec.GetKernelVersion(), spec.GetArtifactUrl() != "", spec.GetStageOnly()); err != nil {
		return sendDriverFailure(stream, err.Error())
	}

	// Stage bytes: either the client's chunks or a URL the agent fetches itself.
	sendDriverProgress(stream, "receiving", 10)
	var tmpPath string
	var digest []byte
	if url := spec.GetArtifactUrl(); url != "" {
		tmpPath, digest, err = s.stageFromURL(stream.Context(), url)
	} else {
		tmpPath, digest, err = s.stageFromStream(stream)
	}
	if err != nil {
		return sendDriverFailure(stream, err.Error())
	}
	defer os.Remove(tmpPath)

	sendDriverProgress(stream, "applying", 70)
	install := DriverInstallSpec{
		Name:          spec.GetName(),
		KernelVersion: spec.GetKernelVersion(),
		SHA256:        spec.GetSha256(),
		Signature:     spec.GetSignature(),
		ArtifactURL:   spec.GetArtifactUrl(),
		ModulesLoad:   spec.GetModulesLoad(),
		StageOnly:     spec.GetStageOnly(),
	}
	rebootRequired, err := s.finalize(stream.Context(), install, tmpPath, digest)
	if err != nil {
		return sendDriverFailure(stream, err.Error())
	}

	if install.StageOnly {
		s.logger.Info("driver add-on staged for a future kernel",
			zap.String("name", spec.GetName()), zap.String("kernel", spec.GetKernelVersion()))
	} else {
		s.logger.Info("driver add-on installed", zap.String("name", spec.GetName()), zap.String("sha256", hex.EncodeToString(digest)))
	}
	return sendDriverCompleted(stream, spec.GetName(), rebootRequired)
}

// InstallFromURL installs a driver add-on described by spec, fetching the .raw
// from spec.ArtifactURL. Used by the first-boot seed; it shares the exact
// verify/place/apply path as the InstallDriver RPC.
func (s *DriverService) InstallFromURL(ctx context.Context, spec DriverInstallSpec) error {
	// Serialized in finalize, not here — see InstallDriver.
	if err := validateDriverName(spec.Name); err != nil {
		return err
	}
	if err := s.checkKernel(spec.Name, spec.KernelVersion, true, spec.StageOnly); err != nil {
		return err
	}
	if spec.ArtifactURL == "" {
		return fmt.Errorf("driver %q has no artifact URL", spec.Name)
	}
	tmpPath, digest, err := s.stageFromURL(ctx, spec.ArtifactURL)
	if err != nil {
		return err
	}
	defer os.Remove(tmpPath)
	// The seed runs at first boot: no operator to tell, nothing resident to conflict.
	if _, err := s.finalize(ctx, spec, tmpPath, digest); err != nil {
		return err
	}
	s.logger.Info("driver add-on installed", zap.String("name", spec.Name), zap.String("sha256", hex.EncodeToString(digest)))
	return nil
}

// checkKernel rejects a .ko built for another kernel. Remote installs must
// declare the version: nobody inspected the artifact, so silence is not consent.
// A local file may omit it - the operator picked the exact bytes.
func (s *DriverService) checkKernel(name, kernelVersion string, remote, stageOnly bool) error {
	if stageOnly {
		// Staging targets a kernel this device is not running; finalize checks the
		// image against the kernel it was published for instead. Only a URL fetch
		// has to declare one, because only there is the manifest the sole witness
		// to what the bytes are.
		if kernelVersion == "" && remote {
			return fmt.Errorf("driver %q cannot be staged from a URL without a kernel version", name)
		}
		return nil
	}
	if kernelVersion == "" {
		if remote {
			return fmt.Errorf("driver %q does not declare a kernel version; refusing to install it unverified", name)
		}
		return nil
	}
	if running := s.unameR(); kernelVersion != running {
		return fmt.Errorf("driver %q was built for kernel %s but this device runs %s", name, kernelVersion, running)
	}
	return nil
}

// finalize verifies a staged .raw (sha256 + signature), places it under its
// stable name, writes the modules-load config, and applies it. It holds s.mu:
// place+apply mutate shared state (/data store and the merged /usr), so they
// must not interleave with another install or a remove.
func (s *DriverService) finalize(ctx context.Context, spec DriverInstallSpec, tmpPath string, digest []byte) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	got := hex.EncodeToString(digest)
	if spec.SHA256 == "" && spec.ArtifactURL != "" {
		// Nobody inspected these bytes: the agent chose them from a URL. Without a
		// declared digest, and with signing not yet enforced, nothing identifies
		// what was fetched. A local file is the operator's own choice.
		return false, fmt.Errorf("driver %q was fetched without a declared sha256; refusing to install it unverified", spec.Name)
	}
	if spec.SHA256 != "" && !strings.EqualFold(spec.SHA256, got) {
		return false, fmt.Errorf("sha256 mismatch: expected %s, got %s", spec.SHA256, got)
	}
	// SECURITY: with no signing key embedded the verifier is a no-op, so sha256
	// proves integrity but not origin. Accepted only for the mTLS RPC pre-GA (that
	// caller already has a root shell on the same server); the unauthenticated
	// seed sets requireSignature and is refused below.
	if !s.verifier.Enabled() {
		if s.requireSignature {
			return false, fmt.Errorf("driver %q cannot be authenticated (no signing key embedded); refusing install from an unauthenticated source", spec.Name)
		}
		s.logger.Warn("driver signature verification is DISABLED (no signing key embedded); installing unverified kernel modules",
			zap.String("name", spec.Name), zap.String("sha256", got))
	}
	if err := s.verifier.Verify(digest, spec.Signature); err != nil {
		switch {
		case errors.Is(err, sigverify.ErrUnsigned):
			return false, fmt.Errorf("driver add-on is unsigned; refusing install")
		case errors.Is(err, sigverify.ErrBadSignature):
			return false, fmt.Errorf("driver add-on signature verification failed; refusing install")
		default:
			return false, fmt.Errorf("driver add-on signature verification error: %v", err)
		}
	}
	// The image is the last word on which kernel it targets; checked after the
	// signature gate so a signing key also gates squashfs parsing. Staging aims at
	// a kernel this device is not running, so it checks against the published one —
	// or, for streamed bytes that declare none, whatever the image itself names. A
	// URL fetch must still declare one: there the manifest is the only cross-check.
	target := s.unameR()
	if spec.StageOnly {
		target = spec.KernelVersion
		if target == "" {
			if spec.ArtifactURL != "" {
				return false, fmt.Errorf("driver %q cannot be staged from a URL without a kernel version", spec.Name)
			}
			target, _ = imageKernel(tmpPath, spec.Name)
		}
	}
	if err := verifyImageKernel(tmpPath, spec.Name, spec.KernelVersion, target); err != nil {
		return false, err
	}
	// Sample residency now, before snapshotDriver stashes the conf and before the
	// apply modprobes this add-on's own modules: afterwards a freshly loaded
	// module is indistinguishable from one that never went away. The incoming
	// image is the only source for a self-describing add-on's list before it
	// merges, so it joins what is already declared on disk.
	declared := modulesUnion(s.declaredModules(spec.Name), spec.ModulesLoad)
	resident := s.residentModules(modulesUnion(declared, imageModules(tmpPath, spec.Name)))

	// The image picks its own bucket: one pinning no kernel goes to the unpinned
	// bucket rather than being stranded in this kernel's by the next OTA.
	kernel, _ := imageKernel(tmpPath, spec.Name)
	if err := validateKernelDir(kernel); err != nil {
		return false, err
	}

	// Roll back to the pre-install state on failure: a reinstall/upgrade that
	// fails must leave the working version in place, not delete it, and must not
	// leave the rest of the add-ons unmerged (a failed refresh unmerges all).
	snap, err := s.snapshotDriver(kernel, spec.Name)
	if err != nil {
		return false, err
	}
	if err := s.place(kernel, spec.Name, tmpPath, spec.ModulesLoad); err != nil {
		s.removePlaced(kernel, spec.Name) // place() may have renamed the .raw before failing
		snap.restore()
		return false, err
	}
	if spec.StageOnly {
		// Deliberately no apply: the image is for a kernel that is not running, so
		// merging it now would do nothing useful and a failure would be reported
		// against an OS update the operator has not started yet.
		snap.commit()
		s.pruneStore(target)
		return false, nil
	}
	previousGeneration := s.activationGeneration(spec.Name)
	if err := s.apply(ctx, spec.Name); err != nil {
		s.removePlaced(kernel, spec.Name)
		snap.restore()
		s.reapply(ctx)
		return false, fmt.Errorf("apply failed: %v", err)
	}
	snap.commit()

	// Still resident from before the install: modprobe could not replace it, so
	// the kernel runs the old code until a reboot.
	// A fresh runtime receipt proves which resident modules were actually unloaded.
	// Older runtimes have no receipt, so retain the conservative reboot fallback.
	if generation := s.activationGeneration(spec.Name); generation != "" && generation != previousGeneration {
		replaced := readModulesConf(filepath.Join(s.activeStateDir, spec.Name, "replaced-modules"))
		resident = modulesDifference(resident, replaced)
	}
	if stuck := s.residentModules(resident); len(stuck) > 0 {
		s.logger.Info("driver add-on installed but a reboot is needed to load it",
			zap.String("name", spec.Name), zap.Strings("resident_modules", stuck))
		return true, nil
	}
	return false, nil
}

// stagedFile accumulates a .raw to a temp file on the /data store while hashing
// it and enforcing the size cap.
type stagedFile struct {
	f       *os.File
	path    string
	hasher  hash.Hash
	written int64
}

// newStagedFile creates a temp file on the same filesystem as the enabled dir so
// the eventual rename into place is atomic.
func (s *DriverService) newStagedFile() (*stagedFile, error) {
	if err := os.MkdirAll(s.enabledDir, 0o755); err != nil {
		return nil, fmt.Errorf("preparing driver store: %w", err)
	}
	// Beside the store, not inside a kernel directory: the rename must stay on one
	// filesystem, and a stray .tmp must never look like an add-on.
	f, err := os.CreateTemp(filepath.Dir(s.enabledDir), ".driver-*.raw.tmp")
	if err != nil {
		return nil, fmt.Errorf("creating temp file: %w", err)
	}
	return &stagedFile{f: f, path: f.Name(), hasher: sha256.New()}, nil
}

func (sf *stagedFile) Write(p []byte) (int, error) {
	sf.written += int64(len(p))
	if sf.written > maxDriverImageSize {
		return 0, fmt.Errorf("driver image exceeds the %d-byte limit", maxDriverImageSize)
	}
	if _, err := sf.f.Write(p); err != nil {
		return 0, err
	}
	sf.hasher.Write(p)
	return len(p), nil
}

// commit flushes and closes the temp file, returning its sha256 digest.
func (sf *stagedFile) commit() ([]byte, error) {
	if sf.written == 0 {
		sf.abort()
		return nil, fmt.Errorf("driver image is empty")
	}
	if err := sf.f.Sync(); err != nil {
		sf.abort()
		return nil, fmt.Errorf("syncing driver image: %w", err)
	}
	if err := sf.f.Close(); err != nil {
		os.Remove(sf.path)
		return nil, fmt.Errorf("closing driver image: %w", err)
	}
	return sf.hasher.Sum(nil), nil
}

func (sf *stagedFile) abort() {
	sf.f.Close()
	os.Remove(sf.path)
}

// stageFromURL downloads the .raw to a temp file, returning its path + sha256.
func (s *DriverService) stageFromURL(ctx context.Context, url string) (string, []byte, error) {
	sf, err := s.newStagedFile()
	if err != nil {
		return "", nil, err
	}
	body, err := s.httpGet(ctx, url)
	if err != nil {
		sf.abort()
		return "", nil, fmt.Errorf("fetching driver image: %w", err)
	}
	defer body.Close()
	if _, err := io.Copy(sf, body); err != nil {
		sf.abort()
		return "", nil, fmt.Errorf("downloading driver image: %w", err)
	}
	digest, err := sf.commit()
	if err != nil {
		return "", nil, err
	}
	return sf.path, digest, nil
}

// stageFromStream reads the .raw from the client's InstallDriver chunks.
func (s *DriverService) stageFromStream(stream grpc.BidiStreamingServer[agentpbv2.InstallDriverRequest, agentpbv2.DriverApplyResponse]) (string, []byte, error) {
	sf, err := s.newStagedFile()
	if err != nil {
		return "", nil, err
	}
	for {
		msg, rerr := stream.Recv()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			sf.abort()
			return "", nil, fmt.Errorf("receiving driver image: %w", rerr)
		}
		if chunk := msg.GetChunk(); chunk != nil {
			if _, werr := sf.Write(chunk.GetData()); werr != nil {
				sf.abort()
				return "", nil, werr
			}
		}
	}
	digest, err := sf.commit()
	if err != nil {
		return "", nil, err
	}
	return sf.path, digest, nil
}

// place atomically moves the staged .raw to enabled/<name>.raw and writes the
// modules-load config. The on-device filename derives from the verified name, so
// it matches the image's extension-release (systemd-sysext merges by name).
func (s *DriverService) place(kernel, name, tmpPath string, modules []string) error {
	dst := s.rawPath(kernel, name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("preparing driver store: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return fmt.Errorf("setting driver image permissions: %w", err)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return fmt.Errorf("placing driver image: %w", err)
	}
	conf := s.confPath(kernel, name)
	if err := os.MkdirAll(filepath.Dir(conf), 0o755); err != nil {
		return fmt.Errorf("preparing modules-load dir: %w", err)
	}
	if len(modules) == 0 {
		os.Remove(conf) //nolint:errcheck // no modules to autoload for this add-on
		return fsyncDir(filepath.Dir(dst))
	}
	if err := os.WriteFile(conf, []byte(strings.Join(modules, "\n")+"\n"), 0o644); err != nil {
		return fmt.Errorf("writing modules-load config: %w", err)
	}
	if err := fsyncDir(filepath.Dir(dst)); err != nil {
		return err
	}
	return fsyncDir(filepath.Dir(conf))
}

// fsyncDir persists a directory entry created by rename or write. /data is the
// only record that an add-on is installed, so losing the entry to a power cut
// leaves the merged /usr disagreeing with the store.
func fsyncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening %s to sync: %w", filepath.Base(path), err)
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return fmt.Errorf("syncing %s: %w", filepath.Base(path), syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing %s: %w", filepath.Base(path), closeErr)
	}
	return nil
}

// removePlaced deletes a driver add-on's on-disk state (its .raw and modules-load
// conf), used to roll back a partially-applied install so a failure leaves nothing
// installed or declared.
func (s *DriverService) removePlaced(kernel, name string) {
	os.Remove(s.rawPath(kernel, name))  //nolint:errcheck
	os.Remove(s.confPath(kernel, name)) //nolint:errcheck
}

// driverSnapshot holds a driver's pre-operation state so a failed install or
// remove restores the working version instead of destroying it. Backups are
// renames to dotted .bak names, which the apply script's globs ignore. A remove
// spans every kernel bucket, so the set is a list rather than one pair.
type driverSnapshot struct {
	stashed  []stashedFile
	rawFound bool
}

type stashedFile struct{ path, bak string }

// snapshotDriver moves one kernel's copy of a driver aside, for an install.
func (s *DriverService) snapshotDriver(kernel, name string) (*driverSnapshot, error) {
	return s.snapshotPaths(s.rawPath(kernel, name), s.confPath(kernel, name))
}

// snapshotDriverAllKernels moves every copy of a driver aside, for a remove:
// leaving a staged rebuild behind would resurrect the add-on at the next OTA.
func (s *DriverService) snapshotDriverAllKernels(name string) (*driverSnapshot, error) {
	var paths []string
	kernels, err := s.storedKernels()
	if err != nil {
		return nil, err
	}
	for _, kernel := range kernels {
		paths = append(paths, s.rawPath(kernel, name), s.confPath(kernel, name))
	}
	return s.snapshotPaths(paths...)
}

// snapshotPaths stashes each path that exists. It fails rather than returning a
// partial snapshot: callers rely on restore() to undo everything, so a
// half-stashed driver must not reach the mutating step.
func (s *DriverService) snapshotPaths(paths ...string) (*driverSnapshot, error) {
	snap := &driverSnapshot{}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				continue // nothing installed here
			}
			// Anything else and we cannot tell whether a working copy is there.
			// Reporting "absent" would let the caller's rollback delete it.
			snap.restore()
			return nil, fmt.Errorf("checking %s: %w", filepath.Base(path), err)
		}
		bak := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".bak")
		if err := os.Rename(path, bak); err != nil {
			snap.restore()
			return nil, fmt.Errorf("setting %s aside: %w", filepath.Base(path), err)
		}
		snap.stashed = append(snap.stashed, stashedFile{path: path, bak: bak})
		if strings.HasSuffix(path, ".raw") {
			snap.rawFound = true
		}
	}
	return snap, nil
}

// installed reports whether the driver existed before the operation.
func (snap *driverSnapshot) installed() bool { return snap.rawFound }

// restore puts the snapshotted state back, replacing whatever the failed
// operation left behind.
func (snap *driverSnapshot) restore() {
	for _, f := range snap.stashed {
		os.Remove(f.path)        //nolint:errcheck // replaced by the backup below
		os.Rename(f.bak, f.path) //nolint:errcheck // best effort; nothing better on failure
	}
}

// commit drops the backups once the operation has succeeded.
func (snap *driverSnapshot) commit() {
	for _, f := range snap.stashed {
		os.Remove(f.bak) //nolint:errcheck
	}
}

// reapply re-runs the apply script after a rollback. A failed apply tears down
// the module overlay and aborts `systemd-sysext refresh` wholesale, which
// unmerges every healthy add-on too, so the restored set must be merged again.
func (s *DriverService) reapply(ctx context.Context) {
	// No subject: this restores every add-on, so any failure is ours to report.
	if err := s.apply(ctx, ""); err != nil {
		s.logger.Error("could not restore the previous driver set after a failed operation; a reboot will re-apply it",
			zap.Error(err))
	}
}

// apply runs the sysext-apply script detached from the caller's cancellation: a
// CLI Ctrl-C/disconnect must not SIGKILL it mid-merge and leave /usr half-applied.
// A 2-minute timeout still bounds a hung script.
// subject names the add-on this apply is for, so the script scopes its exit
// status to it. Empty when restoring the whole set, where any failure is ours.
func (s *DriverService) apply(ctx context.Context, subject string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	args := []string{}
	if subject != "" {
		args = append(args, subject)
	}
	cmd := exec.CommandContext(ctx, s.applyScript, args...)
	cmd.Env = driverEnvWithPath()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v (%s)", s.applyScript, err, strings.TrimSpace(string(out)))
	}
	s.logger.Info("sysext-apply completed", zap.String("output", strings.TrimSpace(string(out))))
	return nil
}

// RemoveDriver drops a driver add-on from the enabled set and re-applies.
func (s *DriverService) RemoveDriver(req *agentpbv2.RemoveDriverRequest, stream grpc.ServerStreamingServer[agentpbv2.DriverApplyResponse]) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	name := req.GetName()
	if err := validateDriverName(name); err != nil {
		return sendDriverFailure(stream, err.Error())
	}
	installed, err := s.anyCopyInstalled(name)
	if err != nil {
		return sendDriverFailure(stream, err.Error())
	}
	if !installed {
		return sendDriverFailure(stream, fmt.Sprintf("driver %q is not installed", name))
	}

	// Read before anything moves: the snapshot stashes the /data conf and the
	// apply unmerges the baked one.
	declared := s.declaredModules(name)

	sendDriverProgress(stream, "removing", 30)
	// Snapshot rather than delete outright: if the unmerge fails the driver is
	// still merged into /usr, so putting it back keeps /data agreeing with the
	// running system and lets the operator retry instead of stranding a
	// merged-but-unlisted driver until reboot.
	snap, err := s.snapshotDriverAllKernels(name)
	if err != nil {
		return sendDriverFailure(stream, err.Error())
	}
	if !snap.installed() {
		return sendDriverFailure(stream, fmt.Sprintf("driver %q is not installed", name))
	}

	sendDriverProgress(stream, "applying", 70)
	if err := s.apply(stream.Context(), name); err != nil {
		snap.restore()
		s.reapply(stream.Context())
		return sendDriverFailure(stream, fmt.Sprintf("apply failed: %v", err))
	}
	snap.commit()

	s.logger.Info("driver add-on removed", zap.String("name", name))
	// The .ko is gone from /usr but the module is never force-unloaded, so the
	// driver keeps running until a reboot.
	stuck := s.residentModules(declared)
	if len(stuck) > 0 {
		s.logger.Info("driver add-on removed but its modules are still loaded",
			zap.String("name", name), zap.Strings("resident_modules", stuck))
	}
	return sendDriverCompleted(stream, name, len(stuck) > 0)
}

// ListDrivers reports the installed (declared) drivers plus realized state.
func (s *DriverService) ListDrivers(ctx context.Context, _ *agentpbv2.ListDriversRequest) (*agentpbv2.ListDriversResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	resp := &agentpbv2.ListDriversResponse{
		BaseVersion:      osReleaseVersionID(),
		KernelVersion:    s.unameR(),
		LoadedModules:    s.loadedModules(),
		MergedExtensions: mergedSysextNames(ctx),
	}

	loaded := make(map[string]bool, len(resp.LoadedModules))
	for _, m := range resp.LoadedModules {
		loaded[m] = true
	}

	// Every bucket, not just the running kernel's: an add-on left behind by an
	// OTA must still be listed and flagged, rather than silently disappearing.
	copies, err := s.installedCopies()
	if err != nil {
		return nil, err
	}
	for _, c := range copies {
		// Self-describing add-on: the module list is baked into the .raw and
		// surfaces at the merged path once systemd-sysext merges it. declaredModules
		// mirrors wendyos-sysext-apply's precedence (/data override, then baked-in).
		mods := s.declaredModules(c.name)
		// Version stays empty: the image carries no version field to read.
		kernel, readable := imageKernel(c.rawPath, c.name)
		resp.Installed = append(resp.Installed, &agentpbv2.InstalledDriver{
			Name:          c.name,
			KernelVersion: kernel,
			Unreadable:    !readable,
			ModulesLoad:   mods,
			Loaded:        readable && (kernel == "" || kernel == s.unameR()) && s.payloadActive(c.name) && allModulesLoaded(mods, loaded),
		})
	}
	return resp, nil
}

// --- helpers ---

// unpinnedKernelDir holds add-ons that declare no WENDYOS_KERNEL (udev rules,
// firmware). They apply to every kernel, so they cannot live in one kernel's
// directory. "any" is not a legal kernel release, so it cannot collide.
const unpinnedKernelDir = "any"

// rawPath is where an add-on's image lives for a given kernel. The store is keyed
// by kernel so a rebuild can be staged before the OTA that needs it, and so a
// rollback still finds the copy built for the slot it returns to.
func (s *DriverService) rawPath(kernel, name string) string {
	return filepath.Join(s.enabledDir, kernelDir(kernel), name+".raw")
}

// confPath is the /data modules-load override for an add-on, keyed alongside its
// image: two kernels' builds may declare different module names.
func (s *DriverService) confPath(kernel, name string) string {
	return filepath.Join(s.modulesDir, kernelDir(kernel), name+".conf")
}

func kernelDir(kernel string) string {
	if kernel == "" {
		return unpinnedKernelDir
	}
	return kernel
}

// MigrateStore moves add-ons from the flat layout into their kernel bucket. It
// lives here rather than in the apply script because only this side can read a
// squashfs. Re-runnable; a failure leaves the flat copy, which still merges.
func (s *DriverService) MigrateStore() {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, _ := os.ReadDir(s.enabledDir) //nolint:errcheck // absent dir => nothing to migrate
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".raw") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".raw")
		from := filepath.Join(s.enabledDir, e.Name())
		kernel, readable := imageKernel(from, name)
		if !readable {
			// Nothing identifies which bucket it belongs in. Leaving it flat keeps
			// it listed and reported unreadable rather than hidden in a guess.
			s.logger.Warn("leaving an unreadable driver add-on in the legacy store layout", zap.String("name", name))
			continue
		}
		if err := validateKernelDir(kernel); err != nil {
			s.logger.Warn("leaving a driver add-on with an unusable kernel field in place",
				zap.String("name", name), zap.Error(err))
			continue
		}
		if err := s.moveInto(kernel, name, from); err != nil {
			s.logger.Warn("could not migrate a driver add-on to the kernel-keyed store",
				zap.String("name", name), zap.Error(err))
			continue
		}
		s.logger.Info("migrated driver add-on to the kernel-keyed store",
			zap.String("name", name), zap.String("kernel", kernelDir(kernel)))
	}
}

// moveInto relocates one add-on and its /data override into a kernel bucket.
func (s *DriverService) moveInto(kernel, name, from string) error {
	dst := s.rawPath(kernel, name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(from, dst); err != nil {
		return err
	}
	if legacyConf := filepath.Join(s.modulesDir, name+".conf"); fileExists(legacyConf) {
		conf := s.confPath(kernel, name)
		if err := os.MkdirAll(filepath.Dir(conf), 0o755); err != nil {
			return err
		}
		if err := os.Rename(legacyConf, conf); err != nil {
			return err
		}
	}
	return fsyncDir(filepath.Dir(dst))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// pruneStore drops add-ons for kernels this device has left behind. It also keeps
// the most recently touched other bucket: that is the previous kernel, and a
// rollback would land on a slot with no drivers without it.
func (s *DriverService) pruneStore(stagedTarget string) {
	keep := map[string]bool{s.unameR(): true, unpinnedKernelDir: true}
	if stagedTarget != "" {
		keep[stagedTarget] = true
	}
	type bucket struct {
		name    string
		modTime time.Time
	}
	var others []bucket
	entries, _ := os.ReadDir(s.enabledDir) //nolint:errcheck
	for _, e := range entries {
		if !e.IsDir() || keep[e.Name()] || validateKernelDir(e.Name()) != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		others = append(others, bucket{e.Name(), info.ModTime()})
	}
	sort.Slice(others, func(i, j int) bool { return others[i].modTime.After(others[j].modTime) })
	for i, b := range others {
		if i == 0 {
			continue // the previous kernel: a rollback still needs it
		}
		for _, root := range []string{s.enabledDir, s.modulesDir} {
			os.RemoveAll(filepath.Join(root, b.name)) //nolint:errcheck // best effort
		}
		s.logger.Info("pruned driver add-ons for a kernel this device no longer runs",
			zap.String("kernel", b.name))
	}
}

// installedCopy is one add-on image found in the store, and the bucket it sits
// in. dir is carried because the legacy flat layout has no bucket at all.
type installedCopy struct {
	name    string
	rawPath string
}

// installedCopies returns every add-on in the store, deduped by name with the
// running kernel's copy preferred so a staged name resolves to the one that can
// load. Top-level entries are the flat layout, read until migration moves them.
func (s *DriverService) installedCopies() ([]installedCopy, error) {
	dirs := []string{filepath.Join(s.enabledDir, s.unameR()), filepath.Join(s.enabledDir, unpinnedKernelDir), s.enabledDir}
	kernels, err := s.storedKernels()
	if err != nil {
		return nil, err
	}
	for _, kernel := range kernels {
		dirs = append(dirs, filepath.Join(s.enabledDir, kernel))
	}
	seen := map[string]bool{}
	var out []installedCopy
	for _, dir := range dirs {
		entries, err := readDriverDir(dir)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".raw") {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".raw")
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, installedCopy{name: name, rawPath: filepath.Join(dir, e.Name())})
		}
	}
	return out, nil
}

// anyCopyInstalled reports whether the add-on exists under any kernel.
func (s *DriverService) anyCopyInstalled(name string) (bool, error) {
	copies, err := s.installedCopies()
	if err != nil {
		return false, err
	}
	for _, c := range copies {
		if c.name == name {
			return true, nil
		}
	}
	return false, nil
}

// storedKernels lists the buckets present in the store, running kernel first so
// callers that dedupe by name prefer the copy that can actually load. The
// unpinned bucket is always considered, even before it exists.
func (s *DriverService) storedKernels() ([]string, error) {
	seen := map[string]bool{}
	out := []string{s.unameR(), unpinnedKernelDir}
	for _, k := range out {
		seen[k] = true
	}
	entries, err := readDriverDir(s.enabledDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || seen[e.Name()] || validateKernelDir(e.Name()) != nil {
			continue
		}
		seen[e.Name()] = true
		out = append(out, e.Name())
	}
	return out, nil
}

// readDriverDir distinguishes an absent store or bucket from one that exists but
// cannot be inspected. The former means no drivers; the latter must fail closed
// so an OTA cannot mistake an I/O, mount, or permission failure for an empty set.
func readDriverDir(dir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if err == nil {
		return entries, nil
	}
	if os.IsNotExist(err) {
		return nil, nil
	}
	return nil, fmt.Errorf("reading driver store %s: %w", dir, err)
}

// validateKernelDir guards the one kernel string that is not ours: migration
// reads WENDYOS_KERNEL out of a stored image, so a crafted add-on could otherwise
// steer a path out of the store.
func validateKernelDir(kernel string) error {
	if kernel == "" {
		return nil // the unpinned bucket
	}
	for _, r := range kernel {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-', r == '+':
		default:
			return fmt.Errorf("invalid kernel version %q", kernel)
		}
	}
	if kernel == "." || kernel == ".." {
		return fmt.Errorf("invalid kernel version %q", kernel)
	}
	return nil
}

func validateDriverName(name string) error {
	if name == "" {
		return fmt.Errorf("driver name is empty")
	}
	// A leading dot would install fine but never merge: the apply script's
	// "$ENABLED"/*.raw globs skip dotfiles, so it would look applied and do nothing.
	// A leading dash becomes the apply script's subject argument, where it matches
	// no add-on and quietly zeroes the exit status this install is judged by.
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "-") || strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("invalid driver name %q", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return fmt.Errorf("invalid driver name %q (allowed: letters, digits, '.', '_', '-')", name)
		}
	}
	return nil
}

// driverEnvWithPath is the whole environment for the privileged apply script:
// built from nothing, so an inherited IFS/BASH_ENV/LD_* can never steer it.
func driverEnvWithPath() []string {
	return []string{"PATH=" + driverSystemPath}
}

func ptr[T any](v T) *T { return &v }

func httpGetBody(ctx context.Context, rawURL string) (io.ReadCloser, error) {
	if err := validateArtifactURL(rawURL); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	// Redirects run on the caller's goroutine while dials run on their own, so
	// the current host is shared through an atomic rather than a closed-over var.
	var host atomic.Pointer[string]
	host.Store(ptr(req.URL.Hostname()))
	client := &http.Client{
		Timeout: 5 * time.Minute,
		// Check the address actually dialled: Control sees the resolved IP, so a
		// validated name cannot be rebound to an internal one in between.
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: 30 * time.Second,
				Control: func(_, address string, _ syscall.RawConn) error {
					return checkDialAddress(*host.Load(), address)
				},
			}).DialContext,
		},
		// Every hop must pass the same checks, or a permitted URL could bounce
		// the fetch somewhere it would never have been allowed to go directly.
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects fetching driver image")
			}
			if err := validateArtifactURL(r.URL.String()); err != nil {
				return err
			}
			host.Store(ptr(r.URL.Hostname()))
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		client.CloseIdleConnections()
		return nil, fmt.Errorf("unexpected status %d fetching %s", resp.StatusCode, rawURL)
	}
	// The per-request Control hook pins this transport to one URL, so it cannot be
	// shared. Retire its idle connections with the body instead of leaving a
	// socket parked for the life of the agent.
	return closerFunc{ReadCloser: resp.Body, after: client.CloseIdleConnections}, nil
}

// closerFunc runs after once the wrapped body is closed.
type closerFunc struct {
	io.ReadCloser
	after func()
}

func (c closerFunc) Close() error {
	err := c.ReadCloser.Close()
	c.after()
	return err
}

// driverRegistryHost is the only host the agent fetches driver images from by
// default: the public bucket the CLI resolves add-ons out of. Anything else has
// to be named explicitly, because this fetch is a request primitive aimed at
// whatever the caller asks for.
const driverRegistryHost = "storage.googleapis.com"

// driverExtraHostsEnv widens the allowlist (comma-separated) for bench and
// on-prem registries. Setting it on the agent unit takes root, so hosts named
// here are also exempt from the internal-address check.
const driverExtraHostsEnv = "WENDYOS_DRIVER_ARTIFACT_HOSTS"

func extraArtifactHosts() map[string]bool {
	hosts := map[string]bool{}
	for _, h := range strings.Split(os.Getenv(driverExtraHostsEnv), ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			hosts[h] = true
		}
	}
	return hosts
}

// validateArtifactURL vets a URL the agent will fetch on the caller's behalf.
// Default-deny by host: unrestricted, this is an SSRF probe into whatever the
// device can reach. httpGetBody re-runs it on every redirect hop.
func validateArtifactURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid artifact URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("artifact URL scheme %q not allowed (use http or https)", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("artifact URL has no host")
	}
	// Credentials in the URL would be sent to whatever the host resolves to, and
	// land in logs; the registry never needs them.
	if u.User != nil {
		return fmt.Errorf("artifact URL must not embed credentials")
	}
	host := strings.ToLower(u.Hostname())
	if host != driverRegistryHost && !extraArtifactHosts()[host] {
		return fmt.Errorf("artifact URL host %q is not an allowed driver registry (set %s to allow it)", host, driverExtraHostsEnv)
	}
	return nil
}

// checkDialAddress rejects a connection into the device's own networks. It sees
// the resolved address at dial time, so a name that passed validation cannot be
// rebound to an internal IP afterwards. Explicitly opted-in hosts are exempt.
func checkDialAddress(host, address string) error {
	if extraArtifactHosts()[strings.ToLower(host)] {
		return nil
	}
	ipStr, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("unexpected dial address %q: %w", address, err)
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return fmt.Errorf("unresolvable dial address %q", address)
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return fmt.Errorf("artifact host resolved to the internal address %s; refusing to fetch", ip)
	}
	return nil
}

func unameRelease() string {
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

// Reads the field directly rather than calling parseOSRelease: that lives in the
// linux-only distro file, and this one is built for every platform.
func osReleaseVersionID() string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return ""
	}
	defer f.Close()
	return parseKeyValues(f)["VERSION_ID"]
}

func loadedKernelModules() []string {
	f, err := os.Open("/proc/modules")
	if err != nil {
		return nil
	}
	defer f.Close()
	var mods []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if fields := strings.Fields(scanner.Text()); len(fields) > 0 {
			mods = append(mods, fields[0])
		}
	}
	return mods
}

func readModulesConf(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseModulesConf(f)
}

// parseModulesConf reads a modules-load.d list: one module per line, '#' comments.
func parseModulesConf(r io.Reader) []string {
	var mods []string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		mods = append(mods, line)
	}
	return mods
}

// declaredModules returns the modules an add-on autoloads, mirroring the apply
// script's precedence (/data override, then the list baked into the image).
// Call it before an apply: the baked copy unmerges with the add-on.
func (s *DriverService) declaredModules(name string) []string {
	// Same precedence the apply script uses: this kernel, then unpinned, then the
	// pre-keyed flat override, then the list baked into the image.
	candidates := []string{
		s.confPath(s.unameR(), name),
		s.confPath(unpinnedKernelDir, name),
		filepath.Join(s.modulesDir, name+".conf"),
		filepath.Join(s.privatePayloadDir, name, "modules-load.conf"),
		filepath.Join(s.bakedModulesDir, name+".conf"),
	}
	for _, path := range candidates {
		if data, err := os.ReadFile(path); err == nil {
			return parseModulesConf(strings.NewReader(string(data)))
		}
	}
	return nil
}

// Private metadata is inert until the runtime records successful exposure.
// A rejected package must not appear loaded just because another add-on loaded
// modules with the same names. Legacy images retain their original semantics.
func (s *DriverService) payloadActive(name string) bool {
	_, err := os.Stat(filepath.Join(s.privatePayloadDir, name))
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(s.activeStateDir, name, "files"))
	return err == nil
}

func (s *DriverService) activationGeneration(name string) string {
	data, _ := os.ReadFile(filepath.Join(s.activeStateDir, name, "generation"))
	return strings.TrimSpace(string(data))
}

func modulesDifference(modules, removed []string) []string {
	gone := make(map[string]bool, len(removed))
	for _, m := range removed {
		gone[strings.ReplaceAll(m, "-", "_")] = true
	}
	var kept []string
	for _, m := range modules {
		if !gone[strings.ReplaceAll(m, "-", "_")] {
			kept = append(kept, m)
		}
	}
	return kept
}

// modulesUnion is the set an install can disturb: what the add-on already
// declared plus what it is about to. A renamed module leaves the old one running.
func modulesUnion(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	var out []string
	for _, group := range [][]string{a, b} {
		for _, m := range group {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

// residentModules returns those of mods still present in /proc/modules. A module
// is never force-unloaded, so a resident one runs code no on-disk change replaces.
func (s *DriverService) residentModules(mods []string) []string {
	if len(mods) == 0 {
		return nil
	}
	loaded := make(map[string]bool)
	for _, m := range s.loadedModules() {
		loaded[m] = true
	}
	var resident []string
	for _, m := range mods {
		// modprobe treats '-' and '_' interchangeably; /proc/modules uses '_'.
		if loaded[strings.ReplaceAll(m, "-", "_")] || loaded[m] {
			resident = append(resident, m)
		}
	}
	return resident
}

func allModulesLoaded(mods []string, loaded map[string]bool) bool {
	if len(mods) == 0 {
		return false
	}
	for _, m := range mods {
		// modprobe treats '-' and '_' interchangeably; /proc/modules uses '_'.
		if !loaded[strings.ReplaceAll(m, "-", "_")] && !loaded[m] {
			return false
		}
	}
	return true
}

// lookPathIn resolves an executable against an explicit PATH rather than the
// process's own, so a privileged child cannot be steered by an inherited PATH.
func lookPathIn(pathList, name string) (string, error) {
	for _, dir := range filepath.SplitList(pathList) {
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s not found in %s", name, pathList)
}

func mergedSysextNames(ctx context.Context) []string {
	// Resolved against the scrubbed PATH explicitly: exec looks a bare name up in
	// the parent's PATH, so cmd.Env alone would not decide which binary runs.
	bin, err := lookPathIn(driverSystemPath, "systemd-sysext")
	if err != nil {
		return nil
	}
	cmd := exec.CommandContext(ctx, bin, "status", "--json=short")
	cmd.Env = driverEnvWithPath()
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return parseMergedSysextNames(out)
}

func parseMergedSysextNames(out []byte) []string {
	var rows []struct {
		Hierarchy  string          `json:"hierarchy"`
		Extensions json.RawMessage `json:"extensions"`
	}
	if json.Unmarshal(out, &rows) != nil {
		return nil
	}
	var names []string
	for _, row := range rows {
		if row.Hierarchy != "/usr" {
			continue
		}
		var extensions []string
		if json.Unmarshal(row.Extensions, &extensions) == nil {
			names = append(names, extensions...)
		}
	}
	return names
}
