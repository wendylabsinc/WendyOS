//go:build linux

// Package sandbox starts helper subprocesses of the root agent as a non-root
// user, so a bug in a helper that parses bytes from a device or a network peer
// is not a root compromise of the host.
//
// A sandboxed helper has a non-root uid and gid, only the supplementary groups
// it was given, no capabilities, and no_new_privs, so no setuid or
// file-capability binary can give any of that back. It still shares the host's
// filesystem view, network and process namespaces, and the agent's environment.
package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// cacheRoot holds one private cache directory per identity. /run is writable
// only by root, so nothing but the agent can plant an entry here.
const cacheRoot = "/run/wendy-sandbox"

// Identity is a non-root user that helpers are started as. A nil Identity
// starts them unchanged.
type Identity struct {
	name  string
	cred  syscall.Credential
	cache string
}

func (id *Identity) String() string {
	return fmt.Sprintf("%s (uid %d)", id.name, id.cred.Uid)
}

// Lookup resolves username and whichever of groups exist on this host, then
// starts one throwaway process as them: an error here means this host cannot
// run a sandboxed helper at all, before any real helper is risked on it.
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
	id := &Identity{name: username, cred: syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
	for _, name := range groups {
		// A group this host does not have owns nothing here, so skipping it loses no access.
		if g, err := user.LookupGroup(name); err == nil {
			if n, err := strconv.ParseUint(g.Gid, 10, 32); err == nil {
				id.cred.Groups = append(id.cred.Groups, uint32(n))
			}
		}
	}

	// Without a cache directory it can write, GStreamer rescans every plugin on
	// each start. Best effort: a helper works without one, only slower.
	dir := filepath.Join(cacheRoot, u.Uid)
	_ = os.MkdirAll(cacheRoot, 0o755)
	_ = os.Mkdir(dir, 0o700)
	if os.Lchown(dir, int(uid), int(gid)) == nil {
		id.cache = dir
	}

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

// Start is cmd.Start with the child running as id.
func (id *Identity) Start(cmd *exec.Cmd) error {
	if id == nil {
		return cmd.Start()
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cred := id.cred
	cmd.SysProcAttr.Credential = &cred
	if cmd.Dir == "" {
		// The agent's working directory may not be searchable by id.
		cmd.Dir = "/"
	}
	if id.cache != "" {
		cmd.Env = append(cmd.Environ(), "XDG_CACHE_HOME="+id.cache)
	}
	startOnce.Do(func() { go noNewPrivsThread() })
	started := make(chan error, 1)
	starts <- start{cmd, started}
	return <-started
}

type start struct {
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
// ponytail: starts are serialized on this thread. Each is one fork and exec;
// add threads if helpers ever start often enough to queue.
func noNewPrivsThread() {
	runtime.LockOSThread()
	err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0)
	for s := range starts {
		if err != nil {
			// Start reports Err itself, after closing any pipes made for cmd.
			s.cmd.Err = fmt.Errorf("setting no_new_privs: %w", err)
		}
		s.started <- s.cmd.Start()
	}
}
