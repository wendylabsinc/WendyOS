//go:build linux

// Package sandbox starts helper subprocesses of the root agent as a non-root
// user, so a bug in a helper that parses bytes from a device or a network peer
// is not a root compromise of the host.
//
// A sandboxed helper has a non-root uid and gid, only the supplementary groups
// it was given, no capabilities, and no_new_privs, so no setuid or
// file-capability binary can give any of that back. Started through Isolated,
// its uid is one nothing else on the host has. It still shares the host's
// filesystem view, network and process namespaces, and the agent's environment.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// Isolated helpers each get a uid from this range, which systemd's uid map
// (https://systemd.io/UIDS-GIDS/) leaves unused: above regular and homed
// accounts, below its dynamic service users. It is under 65536, so it also
// exists in a user namespace that maps only the first 65536 ids.
const (
	firstUID = 60706
	uidCount = 478
)

// Identity is who helpers are started as. A nil Identity starts them unchanged.
type Identity struct {
	name string
	cred syscall.Credential
	// own gives each start a uid and gid of its own in place of cred's.
	own bool
}

func (id *Identity) String() string {
	if id.own {
		return fmt.Sprintf("a uid of its own (%d-%d)", firstUID, firstUID+uidCount-1)
	}
	return fmt.Sprintf("%s (uid %d)", id.name, id.cred.Uid)
}

// Isolated is an identity whose every start runs as a different uid, one that
// no account, group or running process on this host has. Helpers started
// through it therefore cannot trace, signal or read one another, or anything
// else on the host. The uids have no passwd entry; access to devices comes from
// groups, of which the ones this host does not have are skipped.
//
// Like Lookup, it proves itself with a throwaway process first, and returns a
// nil Identity when the agent is not root.
func Isolated(groups ...string) (*Identity, error) {
	if os.Geteuid() != 0 {
		return nil, nil
	}
	return proven(&Identity{own: true, cred: syscall.Credential{Groups: groupIDs(groups)}})
}

// Lookup resolves username and whichever of groups exist on this host, then
// starts one throwaway process as them: an error here means this host cannot
// run a sandboxed helper at all, before any real helper is risked on it.
// Helpers started as one user are not protected from each other; use Isolated
// unless the helper must be that user.
//
// It returns a nil Identity when the agent is not root, because there is then
// no privilege to drop.
func Lookup(username string, groups ...string) (*Identity, error) {
	if os.Geteuid() != 0 {
		return nil, nil
	}
	u, err := user.Lookup(username)
	if err != nil {
		return nil, err
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("user %q has uid %q", username, u.Uid)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("user %q has gid %q", username, u.Gid)
	}
	if uid == 0 {
		return nil, fmt.Errorf("user %q is root", username)
	}
	return proven(&Identity{name: username, cred: syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: groupIDs(groups)}})
}

func groupIDs(names []string) []uint32 {
	var ids []uint32
	for _, name := range names {
		// A group this host does not have owns nothing here, so skipping it loses no access.
		if g, err := user.LookupGroup(name); err == nil {
			if n, err := strconv.ParseUint(g.Gid, 10, 32); err == nil {
				ids = append(ids, uint32(n))
			}
		}
	}
	return ids
}

func proven(id *Identity) (*Identity, error) {
	// A shell with no script and /dev/null for stdin reads end of file and exits 0.
	probe := exec.Command("/bin/sh")
	if err := id.Start(probe); err != nil {
		return nil, fmt.Errorf("starting a process as %s: %w", id, err)
	}
	if err := probe.Wait(); err != nil {
		return nil, fmt.Errorf("running a process as %s: %w", id, err)
	}
	return id, nil
}

// HideMemory makes this process's memory and environment unreadable through
// ptrace and /proc, even by its own uid. A helper that is handed a secret calls
// it before it reads the secret.
func HideMemory() error {
	return unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)
}

// Start is cmd.Start with the child running as id.
func (id *Identity) Start(cmd *exec.Cmd) error {
	if id == nil {
		return cmd.Start()
	}
	if cmd.Dir == "" {
		// The agent's working directory may not be searchable by id.
		cmd.Dir = "/"
	}
	if id.own {
		// The uid has no passwd entry. GLib warns on stderr when it has to look
		// for a home directory there, so name one that nobody can write.
		cmd.Env = append(cmd.Environ(), "HOME=/")
	}
	startOnce.Do(func() { go noNewPrivsThread() })
	started := make(chan error, 1)
	starts <- start{id, cmd, started}
	return <-started
}

type start struct {
	id      *Identity
	cmd     *exec.Cmd
	started chan<- error
}

var (
	startOnce sync.Once
	starts    = make(chan start)
)

// noNewPrivsThread starts every sandboxed helper from one thread.
//
// no_new_privs belongs to a thread, is inherited by each process forked from
// it, and can never be cleared. So one thread gets the flag and does nothing
// else. It stays locked, so the runtime neither runs other goroutines on it nor
// clones new threads from it, and the flag reaches nothing else the agent
// starts. It never exits: a thread's exit delivers the parent-death signal of
// every child that thread ever started, and which thread started the agent's
// other children (avahibridge, dbusproxy) is not ours to know.
//
// Starting one helper at a time is also what makes a uid of its own safe to
// pick: the previous helper already has its uid by the time the next is chosen.
//
// ponytail: starts are serialized on this thread. Each is a scan of /proc, a
// fork and an exec; add threads if helpers ever start often enough to queue.
func noNewPrivsThread() {
	runtime.LockOSThread()
	err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0)
	if err != nil {
		err = fmt.Errorf("setting no_new_privs: %w", err)
	}
	for s := range starts {
		cred, startErr := s.id.cred, err
		if startErr == nil && s.id.own {
			var live map[uint32]bool
			if live, startErr = liveUIDs(); startErr == nil {
				cred.Uid, startErr = pickUID(live)
				cred.Gid = cred.Uid
			}
		}
		if startErr != nil && s.cmd.Err == nil {
			// Start reports Err itself, after closing any pipes made for cmd.
			s.cmd.Err = startErr
		}
		if s.cmd.SysProcAttr == nil {
			s.cmd.SysProcAttr = &syscall.SysProcAttr{}
		}
		s.cmd.SysProcAttr.Credential = &cred
		s.started <- s.cmd.Start()
	}
}

// liveUIDs returns every uid a running process has. It reads each status file
// rather than the owner of /proc/<pid>, which is root for a process that hid
// its memory.
func liveUIDs() (map[uint32]bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("finding the uids in use: %w", err)
	}
	live := map[uint32]bool{}
	for _, e := range entries {
		status, err := os.ReadFile("/proc/" + e.Name() + "/status")
		if err != nil {
			continue // not a process, or one that just exited
		}
		for line := range strings.SplitSeq(string(status), "\n") {
			if ids, ok := strings.CutPrefix(line, "Uid:"); ok {
				for id := range strings.FieldsSeq(ids) {
					if n, err := strconv.ParseUint(id, 10, 32); err == nil {
						live[uint32(n)] = true
					}
				}
				break
			}
		}
	}
	return live, nil
}

// nextUID walks the range, so a uid is not handed out again the moment its
// helper exits. Only noNewPrivsThread touches it.
var nextUID uint32

// pickUID returns a uid in the private range that no running process, account
// or group has. The uid doubles as the helper's gid.
func pickUID(live map[uint32]bool) (uint32, error) {
	for range uidCount {
		uid := firstUID + nextUID%uidCount
		nextUID++
		n := strconv.FormatUint(uint64(uid), 10)
		// Only "no such id" frees a uid; a lookup that fails otherwise decides nothing.
		_, userErr := user.LookupId(n)
		_, groupErr := user.LookupGroupId(n)
		var noUser user.UnknownUserIdError
		var noGroup user.UnknownGroupIdError
		if !live[uid] && errors.As(userErr, &noUser) && errors.As(groupErr, &noGroup) {
			return uid, nil
		}
	}
	return 0, fmt.Errorf("no uid in %d-%d is free", firstUID, firstUID+uidCount-1)
}
