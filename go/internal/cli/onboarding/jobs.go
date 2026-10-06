package onboarding

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/gofrs/flock"
)

const (
	JobWaitingTarget = "waiting_for_target"
	JobAwaitingErase = "awaiting_erase_confirmation"
	JobQueued        = "queued"
	JobRunning       = "running"
	JobAwaitingBoot  = "waiting_for_boot"
	JobInterrupted   = "write_outcome_unknown"
	JobCompleted     = "completed"
)

type StartOptions struct{ Options }

type ResumeOptions struct {
	JobID           string `json:"job_id"`
	ConfirmErase    bool   `json:"confirm_erase,omitempty"`
	ConfirmInternal bool   `json:"confirm_internal,omitempty"`
	TargetID        string `json:"target_id,omitempty"`
	Address         string `json:"address,omitempty"`
	PublicKey       string `json:"public_key,omitempty"`
}

// Target records what the host actually observed. Fingerprint is an opaque
// confirmation token derived from the observed identity, never a default target.
type Target struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
	Capacity    int64  `json:"capacity_bytes,omitempty"`
	Removable   bool   `json:"removable"`
	Identity    string `json:"identity"`
	USBPath     string `json:"usb_path,omitempty"`
	USBProduct  uint16 `json:"usb_product,omitempty"`
	USBInstance string `json:"usb_instance,omitempty"`
	USBECID     string `json:"usb_ecid,omitempty"`
}

type Job struct {
	ID                string        `json:"id"`
	State             string        `json:"state"`
	Phase             string        `json:"phase"`
	Options           Options       `json:"options"`
	Plan              *Plan         `json:"plan"`
	Target            *Target       `json:"target,omitempty"`
	Instructions      []string      `json:"instructions"`
	Error             string        `json:"error,omitempty"`
	LogPath           string        `json:"log_path"`
	StorePath         string        `json:"store_path"`
	CreatedAt         time.Time     `json:"created_at"`
	UpdatedAt         time.Time     `json:"updated_at"`
	EraseAuthorizedAt *time.Time    `json:"erase_authorized_at,omitempty"`
	WriteStartedAt    *time.Time    `json:"write_started_at,omitempty"`
	WriteCompletedAt  *time.Time    `json:"write_completed_at,omitempty"`
	Verification      *Verification `json:"verification,omitempty"`
}

// JobHooks separate durable transitions from platform-specific probes and writes.
// Launch must return after starting a worker that calls Store.Run. Execute must
// call checkpoint("writing", true) before its first persistent target mutation.
type JobHooks struct {
	Plan   func(context.Context, Options) (*Plan, error)
	Probe  func(context.Context, *Job) (*Target, error)
	Ready  func(context.Context, *Job) error
	Launch func(context.Context, *Job) error
	Verify func(context.Context, *Job, ResumeOptions) (*Verification, error)
}

type JobStore struct{ Root string }

var jobIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

var ErrJobUnsupported = errors.New("installation job method unsupported")

func (s JobStore) path(id string) (string, error) {
	if !jobIDPattern.MatchString(id) {
		return "", fmt.Errorf("invalid installation job ID")
	}
	return filepath.Join(s.Root, id+".json"), nil
}

func (s JobStore) lock(id string) (*flock.Flock, error) {
	path, err := s.path(id)
	if err != nil {
		return nil, err
	}
	l, err := s.newLock(path + ".lock")
	if err != nil {
		return nil, err
	}
	if err := l.Lock(); err != nil {
		return nil, err
	}
	return l, nil
}

func (s JobStore) newLock(path string) (*flock.Flock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	err = preserveJobOwner(f, s.Root)
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return flock.New(path), nil
}

func (s JobStore) load(id string) (*Job, error) {
	path, err := s.path(id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading installation job: %w", err)
	}
	var j Job
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, err
	}
	if j.ID != id || j.Plan == nil {
		return nil, fmt.Errorf("invalid installation job record")
	}
	return &j, nil
}

// save makes each checkpoint durable before any destructive work proceeds.
func (s JobStore) save(j *Job) error {
	path, err := s.path(j.ID)
	if err != nil {
		return err
	}
	j.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Root, ".job-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if err := preserveJobOwner(f, s.Root); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// Directory fsync is unavailable on some hosts; the file itself was synced.
	if d, err := os.Open(s.Root); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func (s JobStore) Start(ctx context.Context, opts StartOptions, h JobHooks) (*Job, error) {
	if h.Plan == nil {
		return nil, fmt.Errorf("installation planner is unavailable")
	}
	p, err := h.Plan(ctx, opts.Options)
	if err != nil {
		return nil, err
	}
	switch p.Method {
	case "removable-media", "rootfs-only":
		if p.Target == nil {
			return nil, fmt.Errorf("select an explicit drive before starting an installation job")
		}
	case "usb-recovery":
	default:
		return nil, fmt.Errorf("%w: %q; follow the installation plan for that target", ErrJobUnsupported, p.Method)
	}
	if p.ArtifactURL == "" || len(p.ArtifactSHA256) != 64 {
		return nil, fmt.Errorf("installation requires a published artifact URL and SHA-256")
	}
	if _, err := hex.DecodeString(p.ArtifactSHA256); err != nil {
		return nil, fmt.Errorf("invalid artifact SHA-256")
	}
	if err := os.MkdirAll(s.Root, 0700); err != nil {
		return nil, err
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	// Pin the resolved release and storage, not a mutable "latest" request.
	opts.Options.Version, opts.Options.Storage = p.Version, p.Storage
	j := &Job{ID: hex.EncodeToString(id), State: JobWaitingTarget, Phase: "connect_target", Options: opts.Options, Plan: p, StorePath: s.Root, CreatedAt: now}
	j.LogPath = filepath.Join(s.Root, j.ID+".log")
	j.Instructions = targetInstructions(p)
	log, err := os.OpenFile(j.LogPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	if err := log.Close(); err != nil {
		return nil, err
	}
	if err := s.save(j); err != nil {
		return nil, err
	}
	return j, nil
}

func targetInstructions(p *Plan) []string {
	if p.Method != "usb-recovery" {
		return []string{"Attach the selected media to this host and check its path, model and capacity. Resume to probe the target; no write occurs during this probe.", "Use Ethernet for first boot. These jobs do not pre-seed Wi-Fi or cloud enrollment."}
	}
	button := "Use the developer kit's Force Recovery button or header while applying power/reset, then release Force Recovery."
	if p.DeviceType == "jetson-agx-thor" {
		button = "Hold Force Recovery (middle button), tap Reset (right button), then release Force Recovery."
	}
	if p.DeviceType == "jetson-orin-nano" {
		button = "Disconnect power. Bridge FC REC to GND, pins 9 and 10 on the button header under the module, with a jumper. Connect power to enter recovery, then remove the jumper. This developer kit has no recovery buttons."
	}
	steps := []string{"Disconnect other recovery-mode Jetsons. Attach a USB data cable to the selected developer kit's recovery port and keep it connected until the write finishes.", button, "Resume to probe the board. No flash occurs until the observed target is returned and explicitly authorized.", "Use Ethernet for first boot. These jobs do not pre-seed Wi-Fi or cloud enrollment.", "Jobs pin the recovery chip ID before booting the flash payload, then require the flashing gadget at the same USB path. A USB speed change that changes that path requires inspection and the interactive installer; the job will not select a different port automatically."}
	if p.HostOS == "darwin" {
		steps = append(steps, "If macOS says the attached disk is unreadable, choose Ignore. Do not initialize or eject the flashing disk.")
	}
	if p.HostOS == "windows" {
		steps = append(steps, "If Windows asks to format the attached disk, choose Cancel. Do not format or eject the flashing disk.")
	}
	return steps
}

func bootInstructions(j *Job) []string {
	steps := []string{"The write finished. First boot and application health are not yet verified."}
	if j.Plan.Method == "usb-recovery" {
		if j.Options.DeviceType == "jetson-agx-thor" {
			steps = append(steps, "Release Force Recovery, then power-cycle the Thor or press Reset (right button) once to boot WendyOS.")
		} else if j.Options.DeviceType == "jetson-orin-nano" {
			steps = append(steps, "Confirm the FC REC-to-GND jumper is removed, then allow the Orin Nano to reboot. Power-cycle normally if it stays in recovery.")
		} else {
			steps = append(steps, "Release Force Recovery and allow the Jetson to reboot. Power-cycle normally if it stays in recovery.")
		}
	} else {
		steps = append(steps, "Safely eject the media, insert it into the intended board, and power the board on.")
	}
	return append(steps, "Connect Ethernet and allow first boot to finish. Discover and identify this device, then resume with its explicit address to verify the installed version and board. Include a known public key if available.")
}

// reconcile only recovers work when the per-job worker lock proves no worker is
// alive. A completed or uncertain write is never returned to an executable state.
func (s JobStore) reconcile(j *Job) error {
	if j.State != JobRunning && j.State != JobQueued {
		return nil
	}
	if j.State == JobQueued && time.Since(j.UpdatedAt) < 15*time.Second {
		return nil
	}
	l, err := s.newLock(filepath.Join(s.Root, j.ID+".worker.lock"))
	if err != nil {
		return err
	}
	ok, err := l.TryLock()
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	defer l.Unlock()
	if j.WriteCompletedAt != nil {
		j.State = JobAwaitingBoot
		j.Instructions = bootInstructions(j)
	} else if j.WriteStartedAt != nil {
		j.State = JobInterrupted
		j.Error = "The worker stopped after the write began. The target may be partially written; this job will not repeat the write."
		j.Instructions = []string{"Inspect the target and installation log. You may resume with an explicit address to verify a device that booted successfully. If recovery is required, create a new job and explicitly authorize its write."}
	} else {
		j.State = JobWaitingTarget
		j.Error = "The worker stopped before a persistent write. Reconnect the target and resume to probe and authorize it again."
		j.EraseAuthorizedAt = nil
		j.Instructions = targetInstructions(j.Plan)
	}
	return s.save(j)
}

func (s JobStore) Status(_ context.Context, id string) (*Job, error) {
	l, err := s.lock(id)
	if err != nil {
		return nil, err
	}
	defer l.Unlock()
	j, err := s.load(id)
	if err != nil {
		return nil, err
	}
	if err := s.reconcile(j); err != nil {
		return nil, err
	}
	return j, nil
}

func (s JobStore) Resume(ctx context.Context, opts ResumeOptions, h JobHooks) (*Job, error) {
	l, err := s.lock(opts.JobID)
	if err != nil {
		return nil, err
	}
	defer l.Unlock()
	j, err := s.load(opts.JobID)
	if err != nil {
		return nil, err
	}
	if err := s.reconcile(j); err != nil {
		return nil, err
	}
	switch j.State {
	case JobCompleted, JobQueued, JobRunning:
		return j, nil
	case JobAwaitingBoot, JobInterrupted:
		if opts.Address == "" {
			return j, nil
		}
		if h.Verify == nil {
			return nil, fmt.Errorf("installation verification unavailable")
		}
		v, err := h.Verify(ctx, j, opts)
		if err != nil {
			return nil, err
		}
		j.Verification = v
		if v.Verified {
			j.State = JobCompleted
			j.Phase = "verified"
			j.Error = ""
			j.Instructions = []string{"The expected agent, OS version and board responded. Check cloud enrollment and application readiness separately."}
		}
	case JobWaitingTarget:
		if h.Probe == nil {
			return nil, fmt.Errorf("target probe unavailable")
		}
		target, err := h.Probe(ctx, j)
		if err != nil {
			j.Error = err.Error()
			break
		}
		if target == nil || target.Fingerprint == "" {
			return nil, fmt.Errorf("probe returned no target identity")
		}
		j.Target = target
		j.State = JobAwaitingErase
		j.Phase = "confirm_erase"
		j.Error = ""
		j.Instructions = []string{j.Plan.EraseScope, "Confirm this exact observed target with confirm_erase=true and target_id set to its fingerprint. Non-removable host media also requires confirm_internal=true."}
	case JobAwaitingErase:
		if !opts.ConfirmErase || j.Target == nil || opts.TargetID != j.Target.Fingerprint {
			return nil, fmt.Errorf("explicit erase confirmation and the observed target fingerprint are required")
		}
		if j.Plan.Method != "usb-recovery" && !j.Target.Removable && !opts.ConfirmInternal {
			return nil, fmt.Errorf("this host drive is non-removable; explicit confirm_internal is required")
		}
		if h.Probe == nil || h.Launch == nil {
			return nil, fmt.Errorf("installer execution unavailable")
		}
		target, err := h.Probe(ctx, j)
		if err != nil {
			j.Error = err.Error()
			break
		}
		if target == nil || target.Fingerprint != j.Target.Fingerprint {
			j.State = JobWaitingTarget
			j.Target = nil
			j.Error = "Target identity changed. Resume to probe it again before confirming a write."
			break
		}
		if h.Ready != nil {
			if err := h.Ready(ctx, j); err != nil {
				j.Error = err.Error()
				break
			}
		}
		now := time.Now().UTC()
		j.EraseAuthorizedAt = &now
		j.State = JobQueued
		j.Phase = "preparing"
		j.Error = ""
		j.Instructions = []string{"The worker is preparing the pinned artifact. Check status for progress and the next physical step."}
		if err := s.save(j); err != nil {
			return nil, err
		}
		if err := h.Launch(ctx, j); err != nil {
			j.State = JobAwaitingErase
			j.EraseAuthorizedAt = nil
			j.Error = err.Error()
		}
	default:
		return nil, fmt.Errorf("invalid installation state %q", j.State)
	}
	if err := s.save(j); err != nil {
		return nil, err
	}
	return j, nil
}

type Checkpoint func(phase string, beginsWrite bool) error

// Run is the detached worker entry point. Both its process lock and the durable
// write boundary must succeed before the executor can touch target storage.
func (s JobStore) Run(ctx context.Context, id string, execute func(context.Context, *Job, Checkpoint) error) error {
	if _, err := s.path(id); err != nil {
		return err
	}
	worker, err := s.newLock(filepath.Join(s.Root, id+".worker.lock"))
	if err != nil {
		return err
	}
	ok, err := worker.TryLock()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("installation worker already running")
	}
	defer worker.Unlock()
	// One host flash at a time also protects helpers and shared artifact caches.
	host, err := s.newLock(filepath.Join(s.Root, "host-install.lock"))
	if err != nil {
		return err
	}
	ok, err = host.TryLock()
	if err != nil {
		return err
	}
	if !ok {
		return s.failQueued(id, fmt.Errorf("another installation is running on this host; wait for it to finish, then probe and authorize this job again"))
	}
	defer host.Unlock()
	l, err := s.lock(id)
	if err != nil {
		return err
	}
	j, err := s.load(id)
	if err == nil && (j.State != JobQueued || j.EraseAuthorizedAt == nil || j.WriteStartedAt != nil || j.Target == nil) {
		err = fmt.Errorf("job is not authorized to begin a new write")
	}
	if err == nil {
		j.State = JobRunning
		err = s.save(j)
	}
	l.Unlock()
	if err != nil {
		return err
	}
	checkpoint := func(phase string, beginsWrite bool) error {
		l, err := s.lock(id)
		if err != nil {
			return err
		}
		defer l.Unlock()
		j.Phase = phase
		if beginsWrite && j.WriteStartedAt == nil {
			now := time.Now().UTC()
			j.WriteStartedAt = &now
		}
		return s.save(j)
	}
	err = execute(ctx, j, checkpoint)
	l, lockErr := s.lock(id)
	if lockErr != nil {
		return errors.Join(err, lockErr)
	}
	defer l.Unlock()
	if err != nil {
		j.Error = err.Error()
		if j.WriteStartedAt != nil {
			j.State = JobInterrupted
			j.Instructions = []string{"A write began and did not report success. Do not repeat this job. Inspect the log and target; verify an explicit address if it booted, or create a new recovery job after inspection."}
		} else {
			j.State = JobWaitingTarget
			j.EraseAuthorizedAt = nil
			j.Instructions = targetInstructions(j.Plan)
		}
	} else {
		if j.WriteStartedAt == nil {
			return fmt.Errorf("installer returned success without recording a write boundary")
		}
		now := time.Now().UTC()
		j.WriteCompletedAt = &now
		j.State = JobAwaitingBoot
		j.Phase = "boot_device"
		j.Error = ""
		j.Instructions = bootInstructions(j)
	}
	return errors.Join(err, s.save(j))
}

func (s JobStore) failQueued(id string, cause error) error {
	l, err := s.lock(id)
	if err != nil {
		return errors.Join(cause, err)
	}
	defer l.Unlock()
	j, err := s.load(id)
	if err != nil {
		return errors.Join(cause, err)
	}
	if j.State == JobQueued && j.WriteStartedAt == nil {
		j.State = JobWaitingTarget
		j.EraseAuthorizedAt = nil
		j.Error = cause.Error()
		j.Instructions = targetInstructions(j.Plan)
		return errors.Join(cause, s.save(j))
	}
	return cause
}
