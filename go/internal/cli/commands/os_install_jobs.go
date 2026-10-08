//go:build darwin || linux || windows

package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/onboarding"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/flashengine"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/flashpack"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/rcm"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/t234"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func installationJobStore(root string) (onboarding.JobStore, error) {
	if root == "" {
		dir, err := config.ConfigDir()
		if err != nil {
			return onboarding.JobStore{}, err
		}
		root = filepath.Join(dir, "install-jobs")
	}
	root, err := filepath.Abs(root)
	return onboarding.JobStore{Root: root}, err
}

func installationJobBackend() onboarding.Backend {
	return onboarding.Backend{Plan: planOSInstall, Drives: installDrives,
		Start: func(ctx context.Context, o onboarding.StartOptions) (*onboarding.Job, error) {
			s, err := installationJobStore("")
			if err != nil {
				return nil, err
			}
			return s.Start(ctx, o, installationJobHooks())
		},
		Status: func(ctx context.Context, id string) (*onboarding.Job, error) {
			s, err := installationJobStore("")
			if err != nil {
				return nil, err
			}
			return s.Status(ctx, id)
		},
		Resume: func(ctx context.Context, o onboarding.ResumeOptions) (*onboarding.Job, error) {
			s, err := installationJobStore("")
			if err != nil {
				return nil, err
			}
			return s.Resume(ctx, o, installationJobHooks())
		},
	}
}

func installationJobHooks() onboarding.JobHooks {
	return onboarding.JobHooks{Plan: planOSInstall, Probe: probeInstallJobTarget, Ready: installJobPrivileges, Launch: launchInstallJob,
		Verify: func(ctx context.Context, j *onboarding.Job, o onboarding.ResumeOptions) (*onboarding.Verification, error) {
			if os.Getenv("WENDY_AGENT_SOCKET") != "" {
				return nil, fmt.Errorf("verify from a host without WENDY_AGENT_SOCKET overriding the explicit target")
			}
			return onboarding.Verify(ctx, onboarding.VerifyOptions{Address: o.Address, PublicKey: o.PublicKey, DeviceType: j.Plan.DeviceType, OSVersion: j.Plan.Version, Timeout: 15 * time.Second}, connectMCPDevice)
		},
	}
}

func fingerprintInstallTarget(t *onboarding.Target) {
	t.Fingerprint = ""
	b, _ := json.Marshal(t)
	sum := sha256.Sum256(b)
	t.Fingerprint = hex.EncodeToString(sum[:])
}

func probeInstallJobTarget(ctx context.Context, j *onboarding.Job) (*onboarding.Target, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if j.Plan.Method == "usb-recovery" {
		devs, err := listInstallJobRecoveryDevices()
		if err != nil {
			return nil, err
		}
		var match []rcm.RecoveryDevice
		for _, d := range devs {
			if j.Options.DeviceType == thorDeviceType && d.IsThor() || isT234RecoveryDevice(j.Options.DeviceType) && orinRecoveryMatch(j.Options.DeviceType)(d) {
				match = append(match, d)
			}
		}
		if len(match) != 1 {
			return nil, fmt.Errorf("expected exactly one %s in USB recovery; found %d. Check the recovery button sequence and disconnect other matching boards", j.Options.DeviceType, len(match))
		}
		d := match[0]
		if !rcm.ValidChipID(d.ECID) {
			return nil, fmt.Errorf("cannot read the recovery board's chip identity; fix USB permissions or use the interactive installer")
		}
		if d.PathKey == "" {
			return nil, fmt.Errorf("the recovery board has no physical USB path; resumable jobs require a path to correlate the flashing gadget, so use a directly attached USB port or the interactive installer")
		}
		t := &onboarding.Target{ID: d.PathKey, Name: d.Describe(), Identity: "USB chip ID " + d.ECID, USBPath: d.PathKey, USBProduct: d.Product, USBInstance: d.Instance, USBECID: d.ECID}
		if t.ID == "" {
			t.ID = d.Instance
		}
		if t.ID == "" {
			return nil, fmt.Errorf("recovery board has no stable USB location")
		}
		fingerprintInstallTarget(t)
		return t, nil
	}
	if j.Plan.Target == nil {
		return nil, fmt.Errorf("installation plan has no host drive")
	}
	drives, err := installDrives()
	if err != nil {
		return nil, err
	}
	for _, d := range drives {
		if d.ID != j.Plan.Target.ID {
			continue
		}
		if d != *j.Plan.Target {
			return nil, fmt.Errorf("drive %s no longer matches the planned model, capacity or removable status; create a new plan", d.ID)
		}
		identity, err := installJobDriveIdentity(ctx, d.ID)
		if err != nil {
			return nil, err
		}
		t := &onboarding.Target{ID: d.ID, Name: d.Name, Capacity: d.Capacity, Removable: d.Removable, Identity: identity}
		fingerprintInstallTarget(t)
		return t, nil
	}
	return nil, fmt.Errorf("planned drive %s is not present in the writable-drive inventory", j.Plan.Target.ID)
}

func launchInstallJob(_ context.Context, j *onboarding.Job) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"install", "jobs", "__worker", "--job-id", j.ID, "--job-root", j.StorePath}
	cmd := installJobProcess(self, args)
	log, err := os.OpenFile(j.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd.Stdout, cmd.Stderr = log, log
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	// Surface immediate elevation/exec failure while the caller still holds the
	// job transition lock. A running worker waits for that lock before starting.
	select {
	case err := <-done:
		if err == nil {
			return nil
		}
		var b []byte
		if f, openErr := os.Open(j.LogPath); openErr == nil {
			if st, statErr := f.Stat(); statErr == nil {
				_, _ = f.Seek(max(st.Size()-2048, 0), io.SeekStart)
				b, _ = io.ReadAll(io.LimitReader(f, 2048))
			}
			_ = f.Close()
		}
		return fmt.Errorf("installation worker could not start: %w: %s", err, strings.TrimSpace(string(b)))
	case <-time.After(300 * time.Millisecond):
		return nil
	}
}

func newOSInstallJobsCmd() *cobra.Command {
	var root string
	cmd := &cobra.Command{Use: "jobs", Short: "Start, inspect and resume durable installation jobs", PersistentPreRunE: func(*cobra.Command, []string) error { return nil }, PersistentPostRunE: func(*cobra.Command, []string) error { return nil }}
	cmd.PersistentFlags().StringVar(&root, "job-root", "", "Installation job directory (normally ~/.wendy/install-jobs)")
	var start onboarding.StartOptions
	startCmd := &cobra.Command{Use: "start", Short: "Plan a job and return physical setup instructions without writing", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		s, err := installationJobStore(root)
		if err != nil {
			return err
		}
		j, err := s.Start(c.Context(), start, installationJobHooks())
		if err != nil {
			return err
		}
		return json.NewEncoder(c.OutOrStdout()).Encode(j)
	}}
	startCmd.Flags().StringVar(&start.DeviceType, "device-type", "", "Exact board type")
	startCmd.Flags().StringVar(&start.Carrier, "carrier", "", "Jetson carrier: developer-kit")
	startCmd.Flags().StringVar(&start.Version, "version", "", "Exact release, otherwise latest stable")
	startCmd.Flags().StringVar(&start.Storage, "storage", "", "sd, nvme or emmc")
	startCmd.Flags().StringVar(&start.Drive, "drive", "", "Exact host drive path")
	startCmd.Flags().BoolVar(&start.RootfsOnly, "rootfs-only", false, "Write only the Orin rootfs media")
	var statusID string
	statusCmd := &cobra.Command{Use: "status", Short: "Read job progress and required physical steps", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		s, err := installationJobStore(root)
		if err != nil {
			return err
		}
		j, err := s.Status(c.Context(), statusID)
		if err != nil {
			return err
		}
		return json.NewEncoder(c.OutOrStdout()).Encode(j)
	}}
	statusCmd.Flags().StringVar(&statusID, "job-id", "", "Job ID returned by start")
	var resume onboarding.ResumeOptions
	resumeCmd := &cobra.Command{Use: "resume", Short: "Probe, authorize a write, or verify first boot", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		s, err := installationJobStore(root)
		if err != nil {
			return err
		}
		j, err := s.Resume(c.Context(), resume, installationJobHooks())
		if err != nil {
			return err
		}
		return json.NewEncoder(c.OutOrStdout()).Encode(j)
	}}
	resumeCmd.Flags().StringVar(&resume.JobID, "job-id", "", "Job ID returned by start")
	resumeCmd.Flags().BoolVar(&resume.ConfirmErase, "confirm-erase", false, "Authorize erasing the exact observed target")
	resumeCmd.Flags().BoolVar(&resume.ConfirmInternal, "confirm-internal", false, "Also authorize erasing this non-removable host disk")
	resumeCmd.Flags().StringVar(&resume.TargetID, "target-id", "", "Exact fingerprint returned by the target probe")
	resumeCmd.Flags().StringVar(&resume.Address, "address", "", "Explicit first-boot device address")
	resumeCmd.Flags().StringVar(&resume.PublicKey, "expected-public-key", "", "Expected first-boot device public key")
	var workerID string
	worker := &cobra.Command{Use: "__worker", Hidden: true, Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		s, err := installationJobStore(root)
		if err != nil {
			return err
		}
		return s.Run(c.Context(), workerID, executeInstallJob)
	}}
	worker.Flags().StringVar(&workerID, "job-id", "", "Job ID")
	cmd.AddCommand(startCmd, statusCmd, resumeCmd, worker)
	return cmd
}

// executeInstallJob calls the same image, RCM, T234 and Thor engines as the
// terminal installer. It has no input prompts, target picker or implicit retry.
func executeInstallJob(ctx context.Context, j *onboarding.Job, checkpoint onboarding.Checkpoint) error {
	if err := installJobPrivileges(ctx, j); err != nil {
		return err
	}
	if j.Plan.Method != "usb-recovery" {
		return executeMediaInstallJob(ctx, j, checkpoint)
	}
	cacheBase, err := osCacheDir()
	if err != nil {
		return err
	}
	// A per-job cache cannot reuse an older extracted tree whose checksum was
	// never recorded. The artifact URL and hash were pinned when the job began.
	cache := filepath.Join(cacheBase, "install-jobs", j.ID)
	if err := os.MkdirAll(cache, 0700); err != nil {
		return err
	}
	detail := func(s string) { fmt.Fprintln(os.Stdout, s) }
	if err := checkpoint("download_and_verify", false); err != nil {
		return err
	}
	if j.Options.DeviceType == thorDeviceType {
		return executeThorInstallJob(ctx, j, cache, detail, checkpoint)
	}
	return executeOrinInstallJob(ctx, j, cache, detail, checkpoint)
}

func recheckInstallJobTarget(ctx context.Context, j *onboarding.Job) error {
	t, err := probeInstallJobTarget(ctx, j)
	if err != nil {
		return err
	}
	if j.Target == nil || t.Fingerprint != j.Target.Fingerprint {
		return fmt.Errorf("target identity changed since erase authorization; no write was started")
	}
	return nil
}

func executeMediaInstallJob(ctx context.Context, j *onboarding.Job, checkpoint onboarding.Checkpoint) error {
	if err := checkpoint("download_and_verify", false); err != nil {
		return err
	}
	path, err := downloadImageInto(&imageInfo{DownloadURL: j.Plan.ArtifactURL, Version: j.Plan.Version}, nil)
	if err != nil {
		return err
	}
	defer os.Remove(path)
	if err := verifySHA256(path, j.Plan.ArtifactSHA256); err != nil {
		return err
	}
	stream, err := openMeasuredInstallJobImage(ctx, path, j.Plan.Target.Capacity)
	if err != nil {
		return err
	}
	defer stream.Close()
	if err := recheckInstallJobTarget(ctx, j); err != nil {
		return err
	}
	drives, err := listAllDrives()
	if err != nil {
		return err
	}
	var target *drive
	for _, d := range drives {
		if d.DevicePath == j.Target.ID {
			target = &d
			break
		}
	}
	if target == nil {
		return fmt.Errorf("target media disappeared before writing")
	}
	if target.SizeBytes <= 0 || stream.uncompressedSize <= 0 || stream.uncompressedSize > target.SizeBytes {
		return fmt.Errorf("image size is unknown or exceeds the target capacity")
	}
	if err := checkpoint("writing_media", true); err != nil {
		return err
	}
	// The image already contains its agent. Optional first-boot provisioning is
	// intentionally left to the terminal installer or the network tools.
	return writeImageToDisk(stream, stream.uncompressedSize, *target, nil)
}

func executeOrinInstallJob(ctx context.Context, j *onboarding.Job, cache string, detail func(string), checkpoint onboarding.Checkpoint) error {
	ref := flashpack.RecoveryRef{Device: j.Options.DeviceType, Storage: j.Plan.Storage, Version: j.Plan.Version}
	info := &recoveryFlashpackInfo{URL: j.Plan.ArtifactURL, Checksum: j.Plan.ArtifactSHA256, Version: j.Plan.Version, Device: j.Options.DeviceType, Storage: j.Plan.Storage}
	fp, _, err := resolveT234Flashpack(cache, ref, info, detail)
	if err != nil {
		return err
	}
	workspace, layout, err := prepareT234Workspace(fp)
	if err != nil {
		return err
	}
	defer os.RemoveAll(workspace)
	plan, err := t234.LoadXMLPlan(layout, workspace, fp.Manifest.RootfsDevice)
	if err != nil {
		return err
	}
	handoff, err := os.MkdirTemp("", "wendy-install-handoff-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(handoff)
	if err := recheckInstallJobTarget(ctx, j); err != nil {
		return err
	}
	d := rcm.RecoveryDevice{PathKey: j.Target.USBPath, Product: j.Target.USBProduct, Instance: j.Target.USBInstance, ECID: j.Target.USBECID}
	if err := checkpoint("recovery_boot", false); err != nil {
		return err
	}
	if err := orinStageOneVerified(fp, d, os.Stdout, j.Target.USBECID); err != nil {
		return err
	}
	t := fp.Manifest.Target
	stage := &t234.Stage2{FlashPackagePath: fp.FlashPackageImage(), LayoutPath: layout, ImagesDir: workspace, Plan: plan, PortPath: d.PathKey, StatusPath: fp.Manifest.Layout.FlashPackageStatus, LogsPath: fp.Manifest.Layout.FlashPackageLogs, ExpectedIdentity: t234.IdentityExpectation{ModuleID: t.ModuleID, ModuleSKU: t.ModuleSKU, CarrierID: t.CarrierID, CarrierSKU: t.CarrierSKU}, Out: os.Stdout, Detail: detail, RunHelper: runT234Helper, TempDir: handoff}
	stage.StrictPort = true
	if err := checkpoint("writing_recovery", true); err != nil {
		return err
	}
	if err := stage.SendFlashPackage(ctx); err != nil {
		return fmt.Errorf("recovery handoff failed: %w. This job requires the flashing gadget at the confirmed USB path; if USB speed changes its path, inspect the device and use the interactive recovery installer", err)
	}
	if err := stage.WriteRootfsDevice(ctx); err != nil {
		return err
	}
	status, err := stage.AwaitFinalStatus(ctx)
	if err != nil {
		return err
	}
	if status == nil || !status.Success {
		return fmt.Errorf("device did not report a successful final recovery status")
	}
	return nil
}

// Unknown-size gzip/zstd streams are measured before opening any target. The
// capacity limit bounds decompression and rejects oversized images after at most
// one byte beyond the selected media's capacity. Reopen from byte zero to write.
func openMeasuredInstallJobImage(ctx context.Context, path string, capacity int64) (*imageStream, error) {
	if capacity <= 0 || capacity == int64(^uint64(0)>>1) {
		return nil, fmt.Errorf("invalid target capacity")
	}
	s, err := openInstallJobImage(path)
	if err != nil {
		return nil, err
	}
	if s.uncompressedSize > capacity {
		s.Close()
		return nil, fmt.Errorf("image exceeds the target capacity")
	}
	if s.uncompressedSize > 0 {
		return s, nil
	}
	size, err := io.Copy(io.Discard, io.LimitReader(installJobContextReader{ctx: ctx, reader: s}, capacity+1))
	closeErr := s.Close()
	if err != nil {
		return nil, fmt.Errorf("measuring decompressed image: %w", err)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if size <= 0 || size > capacity {
		return nil, fmt.Errorf("decompressed image is empty or exceeds the target capacity")
	}
	s, err = openInstallJobImage(path)
	if err != nil {
		return nil, err
	}
	s.uncompressedSize = size
	return s, nil
}

type installJobContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r installJobContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func openInstallJobImage(path string) (*imageStream, error) {
	s, err := openLocalImageStream(path)
	if err == nil || !isZstdFile(path) {
		return s, err
	}
	// Published seekable-zstd images report size through their footer. Also
	// accept ordinary zstd frames, whose size may need a bounded measuring pass.
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	d, err := zstd.NewReader(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &imageStream{ReadCloser: &installJobZstdStream{Decoder: d, file: f}}, nil
}

type installJobZstdStream struct {
	*zstd.Decoder
	file *os.File
}

func (s *installJobZstdStream) Close() error { s.Decoder.Close(); return s.file.Close() }

func executeThorInstallJob(ctx context.Context, j *onboarding.Job, cache string, detail func(string), checkpoint onboarding.Checkpoint) error {
	// Always use the pinned download rather than resolving the release again.
	plan := thorFlashPlan{version: j.Plan.Version, info: &thorFlashpackInfo{URL: j.Plan.ArtifactURL, Checksum: j.Plan.ArtifactSHA256, Version: j.Plan.Version}}
	fp, _, err := downloadAndExtractFlashpack(cache, plan, detail)
	if err != nil {
		return err
	}
	source := filepath.Join(fp.FlashWorkspaceDir(), "flash-images")
	workspace, _, err := prepareMutableWorkspace(source, filepath.Join(source, "config-partition.fat32.img"))
	if err != nil {
		return err
	}
	defer os.RemoveAll(workspace)
	if err := recheckInstallJobTarget(ctx, j); err != nil {
		return err
	}
	dev := thorDevice{PathKey: j.Target.USBPath, Label: j.Target.Name, Instance: j.Target.USBInstance, ExpectedECID: j.Target.USBECID}
	if err := thorPrepareHost(io.Discard); err != nil {
		return err
	}
	if err := checkpoint("recovery_boot", false); err != nil {
		return err
	}
	if err := thorStageOne(fp, dev, os.Stdout); err != nil {
		return err
	}
	transport, closer, err := thorOpenGadget(dev, os.Stdout)
	if err != nil {
		return fmt.Errorf("opening confirmed Thor flashing gadget: %w. Jobs require its original USB path; if re-enumeration changes that path, inspect the target and use the interactive recovery installer", err)
	}
	defer closer()
	if err := checkpoint("writing_partitions", true); err != nil {
		return err
	}
	return flashengine.Run(ctx, transport, flashengine.Options{FlashImagesDir: workspace, NvddLocalPath: filepath.Join(fp.BundleDir(), "unified_flash", "tools", "flashtools", "flash", "nvdd"), Out: os.Stdout, Progress: throttledDetail(detail, byteProgress)})
}
