package onboarding

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func jobFixture(t *testing.T) (JobStore, JobHooks, *Job) {
	t.Helper()
	s := JobStore{Root: t.TempDir()}
	h := JobHooks{
		Plan: func(context.Context, Options) (*Plan, error) {
			return &Plan{Method: "removable-media", DeviceType: "raspberry-pi-5", Version: "1.2.3", Storage: "sd", ArtifactURL: "https://example.invalid/image.zip", ArtifactSHA256: strings.Repeat("a", 64), Target: &Drive{ID: "/dev/test", Name: "Test media", Capacity: 16000000000, Removable: true}, EraseScope: "Erase selected media"}, nil
		},
		Probe: func(context.Context, *Job) (*Target, error) {
			return &Target{ID: "/dev/test", Fingerprint: "target-a", Removable: true}, nil
		},
		Launch: func(context.Context, *Job) error { return nil },
	}
	j, err := s.Start(context.Background(), StartOptions{Options: Options{DeviceType: "raspberry-pi-5", Drive: "/dev/test"}}, h)
	if err != nil {
		t.Fatal(err)
	}
	return s, h, j
}

func queueJob(t *testing.T, s JobStore, h JobHooks, j *Job) *Job {
	t.Helper()
	j, err := s.Resume(context.Background(), ResumeOptions{JobID: j.ID}, h)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != JobAwaitingErase {
		t.Fatalf("state %s", j.State)
	}
	j, err = s.Resume(context.Background(), ResumeOptions{JobID: j.ID, ConfirmErase: true, TargetID: j.Target.Fingerprint, ConfirmInternal: true}, h)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != JobQueued {
		t.Fatalf("state %s: %s", j.State, j.Error)
	}
	return j
}

func TestInstallationJobsRequireObservedTargetAndSeparateEraseConfirmation(t *testing.T) {
	s, h, j := jobFixture(t)
	if j.State != JobWaitingTarget || j.Options.Version != "1.2.3" || j.Options.Storage != "sd" {
		t.Fatalf("unresolved job: %+v", j)
	}
	launches := 0
	h.Launch = func(context.Context, *Job) error { launches++; return nil }
	// Sending confirmation before the physical probe cannot flash unseen media.
	j, err := s.Resume(context.Background(), ResumeOptions{JobID: j.ID, ConfirmErase: true, TargetID: "target-a"}, h)
	if err != nil {
		t.Fatal(err)
	}
	if launches != 0 || j.State != JobAwaitingErase {
		t.Fatalf("probe launched a write: %+v", j)
	}
	for _, o := range []ResumeOptions{{JobID: j.ID}, {JobID: j.ID, ConfirmErase: true, TargetID: "different"}} {
		if _, err := s.Resume(context.Background(), o, h); err == nil {
			t.Fatal("accepted missing/mismatched authorization")
		}
	}
	if launches != 0 {
		t.Fatal("launched without authorization")
	}
	if _, err := s.Resume(context.Background(), ResumeOptions{JobID: j.ID, ConfirmErase: true, TargetID: "target-a"}, h); err != nil {
		t.Fatal(err)
	}
	if launches != 1 {
		t.Fatalf("launches=%d", launches)
	}
}

func TestInstallationJobsDetectTargetReplacement(t *testing.T) {
	s, h, j := jobFixture(t)
	j, err := s.Resume(context.Background(), ResumeOptions{JobID: j.ID}, h)
	if err != nil {
		t.Fatal(err)
	}
	h.Probe = func(context.Context, *Job) (*Target, error) {
		return &Target{Fingerprint: "target-b", Removable: true}, nil
	}
	h.Launch = func(context.Context, *Job) error { t.Fatal("launched on replaced target"); return nil }
	j, err = s.Resume(context.Background(), ResumeOptions{JobID: j.ID, ConfirmErase: true, TargetID: "target-a"}, h)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != JobWaitingTarget || j.Target != nil {
		t.Fatalf("replacement did not reset authorization: %+v", j)
	}
}

func TestInstallationJobsRequireInternalDriveAuthorization(t *testing.T) {
	s, h, j := jobFixture(t)
	h.Probe = func(context.Context, *Job) (*Target, error) {
		return &Target{Fingerprint: "internal", Removable: false}, nil
	}
	if _, err := s.Resume(context.Background(), ResumeOptions{JobID: j.ID}, h); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resume(context.Background(), ResumeOptions{JobID: j.ID, ConfirmErase: true, TargetID: "internal"}, h); err == nil {
		t.Fatal("internal wipe lacked explicit authorization")
	}
}

func TestInstallationJobsPersistWriteBoundaryAndNeverRepeatUncertainWrite(t *testing.T) {
	s, h, j := jobFixture(t)
	j = queueJob(t, s, h, j)
	err := s.Run(context.Background(), j.ID, func(_ context.Context, j *Job, checkpoint Checkpoint) error {
		if err := checkpoint("writing", true); err != nil {
			return err
		}
		persisted, err := s.load(j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if persisted.WriteStartedAt == nil {
			t.Fatal("write boundary not persisted before mutation")
		}
		return errors.New("USB disconnected")
	})
	if err == nil {
		t.Fatal("missing write error")
	}
	// A new process/store still sees the uncertain result and cannot rerun it.
	s = JobStore{Root: s.Root}
	j, err = s.Status(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != JobInterrupted {
		t.Fatalf("state=%s", j.State)
	}
	h.Launch = func(context.Context, *Job) error { t.Fatal("repeated uncertain write"); return nil }
	if _, err := s.Resume(context.Background(), ResumeOptions{JobID: j.ID, ConfirmErase: true, TargetID: "target-a"}, h); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(context.Background(), j.ID, func(context.Context, *Job, Checkpoint) error { t.Fatal("executed uncertain job"); return nil }); err == nil {
		t.Fatal("worker accepted uncertain job")
	}
}

func TestInstallationJobsCompleteOnlyAfterVerifiedFirstBoot(t *testing.T) {
	s, h, j := jobFixture(t)
	j = queueJob(t, s, h, j)
	if err := s.Run(context.Background(), j.ID, func(_ context.Context, _ *Job, c Checkpoint) error { return c("writing", true) }); err != nil {
		t.Fatal(err)
	}
	j, err := s.Status(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != JobAwaitingBoot || j.WriteCompletedAt == nil {
		t.Fatalf("write result %+v", j)
	}
	calls := 0
	h.Verify = func(_ context.Context, j *Job, o ResumeOptions) (*Verification, error) {
		calls++
		if o.Address != "board.local" || j.Plan.Version != "1.2.3" {
			t.Fatal("lost explicit identity/version")
		}
		return &Verification{Address: o.Address, Reachable: true, Verified: calls > 1, PublicKey: "key"}, nil
	}
	j, err = s.Resume(context.Background(), ResumeOptions{JobID: j.ID}, h)
	if err != nil || calls != 0 {
		t.Fatal("verified without explicit address")
	}
	j, err = s.Resume(context.Background(), ResumeOptions{JobID: j.ID, Address: "board.local"}, h)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != JobAwaitingBoot {
		t.Fatal("reachability alone completed installation")
	}
	j, err = s.Resume(context.Background(), ResumeOptions{JobID: j.ID, Address: "board.local"}, h)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != JobCompleted {
		t.Fatal("verified job did not complete")
	}
	if err := s.Run(context.Background(), j.ID, func(context.Context, *Job, Checkpoint) error { t.Fatal("repeated completed write"); return nil }); err == nil {
		t.Fatal("accepted completed job")
	}
}

func TestInstallationJobsRecoverCrashedWorkerConservatively(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_write", true: "after_write"}[started], func(t *testing.T) {
			s, _, j := jobFixture(t)
			j.State = JobRunning
			if started {
				now := time.Now()
				j.WriteStartedAt = &now
			}
			if err := s.save(j); err != nil {
				t.Fatal(err)
			}
			j, err := s.Status(context.Background(), j.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := JobWaitingTarget
			if started {
				want = JobInterrupted
			}
			if j.State != want {
				t.Fatalf("state %s, want %s", j.State, want)
			}
		})
	}
}

func TestInstallationJobsDoNotReconcileLiveWorker(t *testing.T) {
	s, h, j := jobFixture(t)
	j = queueJob(t, s, h, j)
	started, finish, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- s.Run(context.Background(), j.ID, func(_ context.Context, _ *Job, c Checkpoint) error {
			if err := c("writing", true); err != nil {
				return err
			}
			close(started)
			<-finish
			return nil
		})
	}()
	<-started
	status, err := s.Status(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != JobRunning {
		t.Fatalf("live worker reconciled to %s", status.State)
	}
	close(finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestInstallationJobsSaveHostContentionWithoutWaitingForRecovery(t *testing.T) {
	s, h, j := jobFixture(t)
	j = queueJob(t, s, h, j)
	host, err := s.newLock(filepath.Join(s.Root, "host-install.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Lock(); err != nil {
		t.Fatal(err)
	}
	defer host.Unlock()
	if err := s.Run(context.Background(), j.ID, func(context.Context, *Job, Checkpoint) error { t.Fatal("ran while host was busy"); return nil }); err == nil {
		t.Fatal("accepted concurrent worker")
	}
	j, err = s.Status(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != JobWaitingTarget || j.EraseAuthorizedAt != nil || !strings.Contains(j.Error, "another installation") {
		t.Fatalf("contention not recorded: %+v", j)
	}
}

func TestInstallationJobsPrewriteFailureRequiresFreshAuthorization(t *testing.T) {
	s, h, j := jobFixture(t)
	j = queueJob(t, s, h, j)
	if err := s.Run(context.Background(), j.ID, func(context.Context, *Job, Checkpoint) error { return errors.New("download checksum mismatch") }); err == nil {
		t.Fatal("missing failure")
	}
	j, err := s.Status(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != JobWaitingTarget || j.WriteStartedAt != nil || j.EraseAuthorizedAt != nil {
		t.Fatalf("unsafe retry state: %+v", j)
	}
	queueJob(t, s, h, j)
}

func TestInstallationJobsRejectUnsupportedAndUnsafeRecords(t *testing.T) {
	s, h, _ := jobFixture(t)
	h.Plan = func(context.Context, Options) (*Plan, error) { return &Plan{Method: "agent"}, nil }
	if _, err := s.Start(context.Background(), StartOptions{}, h); err == nil {
		t.Fatal("accepted agent-only execution")
	}
	for _, id := range []string{"../outside", "", strings.Repeat("0", 33)} {
		if _, err := s.Status(context.Background(), id); err == nil {
			t.Fatalf("accepted invalid ID %q", id)
		}
	}
	files, err := os.ReadDir(s.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if filepath.Ext(f.Name()) == ".json" {
			info, _ := f.Info()
			if info.Mode().Perm() != 0600 {
				t.Fatalf("job mode=%o", info.Mode().Perm())
			}
		}
	}
}

func TestInstallationJobsIncludeBoardAndHostPhysicalInstructions(t *testing.T) {
	for host, prompt := range map[string]string{"darwin": "choose Ignore", "windows": "choose Cancel"} {
		steps := strings.Join(targetInstructions(&Plan{Method: "usb-recovery", DeviceType: "jetson-orin-nano", HostOS: host}), " ")
		for _, want := range []string{"FC REC to GND", "pins 9 and 10", "remove the jumper", prompt} {
			if !strings.Contains(steps, want) {
				t.Fatalf("%s instructions omit %q", host, want)
			}
		}
	}
}
