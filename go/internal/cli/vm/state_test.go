package vm

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func createTestVM(t *testing.T, s *Store, name string, meta Meta) {
	t.Helper()
	const body = "image-bytes"
	if err := s.CreateFrom(name, strings.NewReader(body), int64(len(body)), 1<<20, meta); err != nil {
		t.Fatalf("CreateFrom(%q) = %v", name, err)
	}
}

func TestExitingLauncherCannotClearStateDuringNewStart(t *testing.T) {
	s := newTestStore(t)
	createTestVM(t, s, "dev", Meta{})
	if err := s.WriteState(State{Name: "dev", PID: 100}); err != nil {
		t.Fatal(err)
	}
	lock, err := s.acquireRunLock("dev")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	// The new start has taken the lock but has not written its state yet.
	// An old launcher must not even clear its matching old PID in this window.
	s.clearStateForPID("dev", 100)
	if _, err := s.ReadState("dev"); err != nil {
		t.Fatalf("cleanup ignored new owner's lock: %v", err)
	}
	if err := s.WriteState(State{Name: "dev", PID: 200}); err != nil {
		t.Fatal(err)
	}
	_ = lock.Close()
	s.clearStateForPID("dev", 100)
	if state, err := s.ReadState("dev"); err != nil || state.PID != 200 {
		t.Fatalf("new state lost: %+v %v", state, err)
	}
	s.clearStateForPID("dev", 200)
	if _, err := os.Stat(s.StatePath("dev")); !os.IsNotExist(err) {
		t.Fatal("matching stale state not removed")
	}
}

func TestCreateRecordsProvenance(t *testing.T) {
	s := newTestStore(t)
	createTestVM(t, s, "dev", Meta{ImageVersion: "0.19.0", ImageSource: "pr/1834"})

	got, ok := s.ReadMeta("dev")
	if !ok {
		t.Fatal("ReadMeta() reported the record unreadable")
	}
	if got.ImageVersion != "0.19.0" || got.ImageSource != "pr/1834" {
		t.Errorf("ReadMeta() = %+v, want version 0.19.0 from pr/1834", got)
	}
	if got.DiskBytes != 1<<20 {
		t.Errorf("ReadMeta().DiskBytes = %d, want %d", got.DiskBytes, 1<<20)
	}
	if got.CreatedAt.IsZero() {
		t.Error("ReadMeta().CreatedAt is zero, want it stamped at create")
	}
}

func TestMetaAndStateAreOwnerOnly(t *testing.T) {
	s := newTestStore(t)
	createTestVM(t, s, "dev", Meta{})
	if err := s.WriteState(State{Name: "dev", PID: 1234}); err != nil {
		t.Fatalf("WriteState() = %v", err)
	}
	for _, path := range []string{s.MetaPath("dev"), s.StatePath("dev")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %v, want 0600", filepath.Base(path), perm)
		}
	}
}

func TestWritingRecordsLeavesNoTempFile(t *testing.T) {
	s := newTestStore(t)
	createTestVM(t, s, "dev", Meta{})
	if err := s.WriteState(State{Name: "dev"}); err != nil {
		t.Fatalf("WriteState() = %v", err)
	}
	entries, err := os.ReadDir(s.Dir("dev"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("left %s behind; writes must be tmp+rename", e.Name())
		}
	}
}

func TestReadMetaToleratesAGarbageRecord(t *testing.T) {
	// VMs created before meta.json existed, or whose record was truncated,
	// must stay usable rather than becoming unlistable.
	s := newTestStore(t)
	createTestVM(t, s, "dev", Meta{ImageVersion: "0.19.0"})
	if err := os.WriteFile(s.MetaPath("dev"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("corrupt meta: %v", err)
	}
	got, ok := s.ReadMeta("dev")
	if ok {
		t.Error("ReadMeta() reported garbage as a good read; the caller would write the empty record back")
	}
	if got.Name != "dev" || got.ImageVersion != "" {
		t.Errorf("ReadMeta() on garbage = %+v, want a zero Meta named dev", got)
	}
}

func TestStatusOnAVMThatWasNeverStarted(t *testing.T) {
	s := newTestStore(t)
	createTestVM(t, s, "dev", Meta{})

	st, err := s.Status("dev")
	if err != nil {
		t.Fatalf("Status() = %v", err)
	}
	if !st.Exists || st.Running {
		t.Errorf("Status() = %+v, want exists and not running", st)
	}
}

func TestStatusOnAnAbsentVM(t *testing.T) {
	st, err := newTestStore(t).Status("ghost")
	if err != nil {
		t.Fatalf("Status() = %v", err)
	}
	if st.Exists || st.Running {
		t.Errorf("Status() = %+v, want neither exists nor running", st)
	}
}

func TestStatusReapsStateLeftByAVMThatDied(t *testing.T) {
	// A VM killed with SIGKILL never clears its own state. Because liveness is
	// the kernel-held lock rather than the recorded pid, the next caller can
	// tell the record is stale and tidy it up -- no daemon, no boot sweep.
	s := newTestStore(t)
	createTestVM(t, s, "dev", Meta{})
	if err := s.WriteState(State{Name: "dev", PID: 999999, AgentPort: 50051}); err != nil {
		t.Fatalf("WriteState() = %v", err)
	}

	st, err := s.Status("dev")
	if err != nil {
		t.Fatalf("Status() = %v", err)
	}
	if st.Running {
		t.Error("Status() reported running for a VM whose lock nobody holds")
	}
	if _, err := os.Stat(s.StatePath("dev")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stale state.json still present: %v", err)
	}
}

func TestStatusReportsRunningWhileTheLockIsHeld(t *testing.T) {
	s := newTestStore(t)
	createTestVM(t, s, "dev", Meta{})

	lock, err := s.acquireRunLock("dev")
	if err != nil {
		t.Fatalf("acquireRunLock() = %v", err)
	}
	defer lock.Close()
	if err := s.WriteState(State{Name: "dev", PID: 4321, AgentPort: 50053, StartedAt: time.Now()}); err != nil {
		t.Fatalf("WriteState() = %v", err)
	}

	st, err := s.Status("dev")
	if err != nil {
		t.Fatalf("Status() = %v", err)
	}
	if !st.Running {
		t.Fatal("Status() reported not running while the lock is held")
	}
	if st.State.AgentPort != 50053 {
		t.Errorf("Status().State.AgentPort = %d, want 50053", st.State.AgentPort)
	}
}

func TestAcquireRunLockRefusesASecondHolder(t *testing.T) {
	// Two `wendy` invocations starting the same VM: one wins, and the loser
	// gets ErrAlreadyRunning so it can adopt rather than fail.
	s := newTestStore(t)
	createTestVM(t, s, "dev", Meta{})

	first, err := s.acquireRunLock("dev")
	if err != nil {
		t.Fatalf("first acquireRunLock() = %v", err)
	}
	defer first.Close()

	if _, err := s.acquireRunLock("dev"); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("second acquireRunLock() = %v, want ErrAlreadyRunning", err)
	}
}

func TestRunLockFreesWhenTheHolderCloses(t *testing.T) {
	s := newTestStore(t)
	createTestVM(t, s, "dev", Meta{})

	lock, err := s.acquireRunLock("dev")
	if err != nil {
		t.Fatalf("acquireRunLock() = %v", err)
	}
	held, err := s.runLockHeld("dev")
	if err != nil || !held {
		t.Fatalf("runLockHeld() = %v, %v; want true, nil", held, err)
	}

	lock.Close()
	held, err = s.runLockHeld("dev")
	if err != nil || held {
		t.Fatalf("runLockHeld() after close = %v, %v; want false, nil", held, err)
	}
}

func TestStatusesCoversEveryVMSorted(t *testing.T) {
	s := newTestStore(t)
	createTestVM(t, s, "alpha", Meta{})
	createTestVM(t, s, "beta", Meta{})

	got, err := s.Statuses()
	if err != nil {
		t.Fatalf("Statuses() = %v", err)
	}
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "beta" {
		t.Fatalf("Statuses() = %+v, want alpha then beta", got)
	}
}

func TestStatusDoesNotBlockAConcurrentStart(t *testing.T) {
	// A status poll runs every couple of seconds behind the simulator tab. If
	// it took the exclusive lock, even briefly, a start landing in that window
	// would fail with a spurious ErrAlreadyRunning.
	s := newTestStore(t)
	createTestVM(t, s, "dev", Meta{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			_, _ = s.Status("dev")
		}
	}()
	for range 200 {
		lock, err := s.acquireRunLock("dev")
		if err != nil {
			t.Errorf("acquireRunLock() failed while a status poll was running: %v", err)
			break
		}
		lock.Close()
	}
	<-done
}

func TestStatusesSurvivesOneUnreadableVM(t *testing.T) {
	// One bad directory must not empty the whole list, which is what the
	// simulator tab renders.
	s := newTestStore(t)
	createTestVM(t, s, "good", Meta{})
	if err := os.MkdirAll(s.Dir("broken"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	got, err := s.Statuses()
	if err != nil {
		t.Fatalf("Statuses() = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Statuses() returned %d rows, want both VMs listed", len(got))
	}
}

func TestRemoveRefusesARunningVM(t *testing.T) {
	// The guard belongs here, not only in the cobra command: deleting the image
	// a live emulator has mapped corrupts it.
	s := newTestStore(t)
	createTestVM(t, s, "dev", Meta{})
	lock, err := s.acquireRunLock("dev")
	if err != nil {
		t.Fatalf("acquireRunLock() = %v", err)
	}
	defer lock.Close()

	if err := s.Remove("dev"); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("Remove() on a running VM = %v, want ErrAlreadyRunning", err)
	}
	if _, err := os.Stat(s.DiskPath("dev")); err != nil {
		t.Errorf("the disk was deleted anyway: %v", err)
	}
}

func TestPickHostPortRejectsAnEmptySearch(t *testing.T) {
	if _, err := PickHostPort(50051, 0); err == nil {
		t.Error("PickHostPort(_, 0) = nil error, want a refusal rather than port 0")
	}
}

func TestReapLeavesTheRecordOfAVMThatJustStarted(t *testing.T) {
	// The probe that decides a VM is gone is lock-free, so a start can win the
	// lock and write its record between that probe and the reap. Deleting it
	// would leave a live VM reading as "starting" forever, and unstoppable.
	s := newTestStore(t)
	createTestVM(t, s, "dev", Meta{})

	lock, err := s.acquireRunLock("dev")
	if err != nil {
		t.Fatalf("acquireRunLock() = %v", err)
	}
	defer lock.Close()
	if err := s.WriteState(State{Name: "dev", PID: 4321}); err != nil {
		t.Fatalf("WriteState() = %v", err)
	}

	if reaped, err := s.reapStaleState("dev"); err != nil || reaped {
		t.Fatalf("reapStaleState() = %v, %v; want lock owner preserved", reaped, err)
	}
	if _, err := os.Stat(s.StatePath("dev")); err != nil {
		t.Errorf("reap deleted the record of a VM holding the lock: %v", err)
	}
}

func TestReapOnANeverStartedVMCreatesNoLockFile(t *testing.T) {
	s := newTestStore(t)
	createTestVM(t, s, "dev", Meta{})

	if _, err := s.Status("dev"); err != nil {
		t.Fatalf("Status() = %v", err)
	}
	if _, err := os.Stat(s.LockPath("dev")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a status read created %s; reads must not", s.LockPath("dev"))
	}
}

// The hostname a VM's guest reports is how a leaked mDNS sighting of it is
// recognised. It is learned on first contact and kept beside the provenance,
// which must survive the write.
func TestRecordHostnameSurvivesAlongsideProvenance(t *testing.T) {
	s := newTestStore(t)
	createTestVM(t, s, "dev", Meta{ImageVersion: "0.19.0", ImageSource: "pr/249"})

	if err := s.RecordHostname("dev", "wendyos-gentle-forest"); err != nil {
		t.Fatalf("RecordHostname() = %v", err)
	}
	got, ok := s.ReadMeta("dev")
	if !ok {
		t.Fatal("ReadMeta() reported the record unreadable")
	}
	if got.Hostname != "wendyos-gentle-forest" {
		t.Errorf("Hostname = %q, want wendyos-gentle-forest", got.Hostname)
	}
	if got.ImageVersion != "0.19.0" || got.ImageSource != "pr/249" {
		t.Errorf("provenance clobbered: %+v", got)
	}
}

// A record that cannot be read must not be replaced by a hostname-only one:
// that would silently lose where the image came from.
func TestRecordHostnameRefusesToClobberAnUnreadableRecord(t *testing.T) {
	s := newTestStore(t)
	createTestVM(t, s, "dev", Meta{ImageVersion: "0.19.0"})
	const garbage = "{not json"
	if err := os.WriteFile(s.MetaPath("dev"), []byte(garbage), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.RecordHostname("dev", "wendyos-gentle-forest"); err == nil {
		t.Fatal("RecordHostname() = nil, want an error for an unreadable record")
	}
	data, err := os.ReadFile(s.MetaPath("dev"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != garbage {
		t.Errorf("unreadable record was overwritten:\n%s", data)
	}
}

// An empty hostname says nothing; recording it must not erase what is known.
func TestRecordHostnameIgnoresEmpty(t *testing.T) {
	s := newTestStore(t)
	createTestVM(t, s, "dev", Meta{})
	if err := s.RecordHostname("dev", "wendyos-gentle-forest"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordHostname("dev", ""); err != nil {
		t.Fatalf("RecordHostname(\"\") = %v, want nil", err)
	}
	if got, _ := s.ReadMeta("dev"); got.Hostname != "wendyos-gentle-forest" {
		t.Errorf("Hostname = %q after recording empty, want the earlier value kept", got.Hostname)
	}
}
