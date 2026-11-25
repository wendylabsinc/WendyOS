package commands

import (
	"errors"
	"os"
	"testing"
)

// killOK is the stub kill outcome for "the process went away".
func killOK(record *[]int) func(int) error {
	return func(pid int) error {
		*record = append(*record, pid)
		return nil
	}
}

func withStubs(t *testing.T, find func(string) []portHolder, kill func(int) error, confirm func(string) bool) {
	t.Helper()
	origFind, origKill, origConfirm, origDelay := findPortHoldersFn, killProcessFn, confirmFn, killSettleDelay
	findPortHoldersFn, killProcessFn, confirmFn, killSettleDelay = find, kill, confirm, 0
	t.Cleanup(func() {
		findPortHoldersFn, killProcessFn, confirmFn, killSettleDelay = origFind, origKill, origConfirm, origDelay
	})
}

// Every kill is stubbed. Choose holder PIDs above both protected process IDs
// so a real test-runner PID cannot accidentally turn a holder into "self".
func holderTestPID(offset int) int {
	return max(os.Getpid(), os.Getppid()) + 1 + offset
}

func TestOfferPortBusyRetry_NoHoldersFound(t *testing.T) {
	var killed []int
	withStubs(t,
		func(string) []portHolder { return nil },
		killOK(&killed),
		func(string) bool { t.Fatal("must not prompt when no holder is identified"); return false },
	)

	if offerPortBusyRetry("/dev/ttyFAKE") {
		t.Error("offerPortBusyRetry() = true, want false (no holder to kill)")
	}
	if len(killed) != 0 {
		t.Errorf("killed = %v, want none", killed)
	}
}

func TestOfferPortBusyRetry_HolderFound_UserConfirmsKill(t *testing.T) {
	var killed []int
	var askedQuestion string
	withStubs(t,
		func(string) []portHolder { return []portHolder{{pid: holderTestPID(0), command: "wendy run"}} },
		killOK(&killed),
		func(q string) bool { askedQuestion = q; return true },
	)

	if !offerPortBusyRetry("/dev/ttyFAKE") {
		t.Error("offerPortBusyRetry() = false, want true (kill confirmed, should retry automatically)")
	}
	if len(killed) != 1 || killed[0] != holderTestPID(0) {
		t.Errorf("killed = %v, want [%d]", killed, holderTestPID(0))
	}
	if askedQuestion != "Kill this process and retry?" {
		t.Errorf("question = %q, want singular phrasing", askedQuestion)
	}
}

func TestOfferPortBusyRetry_HolderFound_UserDeclines(t *testing.T) {
	var killed []int
	withStubs(t,
		func(string) []portHolder { return []portHolder{{pid: holderTestPID(0), command: "wendy run"}} },
		killOK(&killed),
		func(string) bool { return false },
	)

	if offerPortBusyRetry("/dev/ttyFAKE") {
		t.Error("offerPortBusyRetry() = true, want false (kill declined)")
	}
	if len(killed) != 0 {
		t.Errorf("killed = %v, want none", killed)
	}
}

func TestOfferPortBusyRetry_MultipleHolders_Pluralized(t *testing.T) {
	var killed []int
	var askedQuestion string
	withStubs(t,
		func(string) []portHolder {
			return []portHolder{{pid: holderTestPID(0), command: "a"}, {pid: holderTestPID(1), command: ""}}
		},
		killOK(&killed),
		func(q string) bool { askedQuestion = q; return true },
	)

	if !offerPortBusyRetry("/dev/ttyFAKE") {
		t.Error("offerPortBusyRetry() = false, want true")
	}
	if len(killed) != 2 {
		t.Errorf("killed = %v, want 2 pids", killed)
	}
	if askedQuestion != "Kill these processes and retry?" {
		t.Errorf("question = %q, want plural phrasing", askedQuestion)
	}
}

// A cancelled flash leaves this process holding the port's fd, so lsof names
// us. Offering to SIGKILL ourselves would kill the CLI mid-device-write: a
// self-only holder list must behave exactly like an empty one.
func TestOfferPortBusyRetry_SelfPidFilteredOut(t *testing.T) {
	for _, tt := range []struct {
		name string
		pid  int
	}{
		{"own pid", os.Getpid()},
		{"parent pid", os.Getppid()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var killed []int
			withStubs(t,
				func(string) []portHolder {
					return []portHolder{{pid: tt.pid, command: "wendy install"}}
				},
				killOK(&killed),
				func(string) bool { t.Fatal("must not offer to kill this process"); return false },
			)

			if offerPortBusyRetry("/dev/ttyFAKE") {
				t.Error("offerPortBusyRetry() = true, want false (self is not a killable holder)")
			}
			if len(killed) != 0 {
				t.Errorf("killed = %v, want none", killed)
			}
		})
	}
}

// Only the real, third-party holder survives the self filter.
func TestOfferPortBusyRetry_SelfFilteredButOtherHolderRemains(t *testing.T) {
	var killed []int
	withStubs(t,
		func(string) []portHolder {
			return []portHolder{
				{pid: os.Getpid(), command: "wendy install"},
				{pid: holderTestPID(0), command: "screen /dev/ttyFAKE"},
			}
		},
		killOK(&killed),
		func(q string) bool {
			if q != "Kill this process and retry?" {
				t.Errorf("question = %q, want singular phrasing (only one real holder)", q)
			}
			return true
		},
	)

	if !offerPortBusyRetry("/dev/ttyFAKE") {
		t.Error("offerPortBusyRetry() = false, want true")
	}
	if len(killed) != 1 || killed[0] != holderTestPID(0) {
		t.Errorf("killed = %v, want [%d] (never our own pid)", killed, holderTestPID(0))
	}
}

// A kill that fails (a root-owned holder, typically) must not be reported as
// an automatic retry: nothing changed, so the caller should ask the user.
func TestOfferPortBusyRetry_KillFails(t *testing.T) {
	withStubs(t,
		func(string) []portHolder { return []portHolder{{pid: holderTestPID(0), command: "sudo screen"}} },
		func(int) error { return errors.New("operation not permitted") },
		func(string) bool { return true },
	)

	if offerPortBusyRetry("/dev/ttyFAKE") {
		t.Error("offerPortBusyRetry() = true, want false (the kill did not succeed)")
	}
}

// A partial kill still changed something, so retrying automatically is right.
func TestOfferPortBusyRetry_PartialKillSucceeds(t *testing.T) {
	withStubs(t,
		func(string) []portHolder {
			return []portHolder{{pid: holderTestPID(0), command: "root-owned"}, {pid: holderTestPID(1), command: "screen"}}
		},
		func(pid int) error {
			if pid == holderTestPID(0) {
				return errors.New("operation not permitted")
			}
			return nil
		},
		func(string) bool { return true },
	)

	if !offerPortBusyRetry("/dev/ttyFAKE") {
		t.Error("offerPortBusyRetry() = false, want true (one holder was killed)")
	}
}
