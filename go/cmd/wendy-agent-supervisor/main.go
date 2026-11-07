//go:build linux

// wendy-agent-supervisor starts the WendyOS agent, tests new agents kept on
// /data, and falls back to the image's agent when one is bad.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/version"
	"github.com/wendylabsinc/wendy/go/internal/supervisor"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

const (
	imageAgent = "/opt/wendyos/bin/wendy-agent"
	storeDir   = "/data/wendy-agent/agent"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-v":
			fmt.Println(version.Version)
			return
		case "check":
			os.Exit(check(os.Args[2:]))
		case "fetch":
			os.Exit(fetch())
		}
	}
	os.Exit(run())
}

// check is what the agent runs before it accepts an upload, so the CLI gets
// the reason at once instead of after a restart.
func check(args []string) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	sha := fs.String("sha256", "", "expected sha256 of the agent binary")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: wendy-agent-supervisor check --sha256 <hex> <file>")
		return 2
	}
	want := supervisor.Want{SHA256: *sha, Arch: runtime.GOARCH}
	v, err := supervisor.Check(context.Background(), fs.Arg(0), want, supervisor.SandboxedVersion)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(v)
	return 0
}

// fetch is the automatic agent update, run by wendyos-agent-updater.timer.
func fetch() int {
	log, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer log.Sync()
	repo, pin := supervisor.FetchSettings(os.Getenv, "/etc/default/wendy-agent", "/data/etc/default/wendy-agent")
	f := &supervisor.Fetcher{
		Store:         supervisor.Store{Dir: storeDir},
		StatusPath:    supervisor.StatusPath,
		Build:         supervisor.BuildID("/"),
		Arch:          runtime.GOARCH,
		Repo:          repo,
		Pin:           pin,
		API:           "https://api.github.com",
		Client:        &http.Client{Timeout: 20 * time.Minute},
		RemoteSeen:    supervisor.RemoteSeenFunc(supervisor.RemoteSeenDir),
		CloudEnrolled: supervisor.CloudEnrolledFunc(supervisor.RemoteSeenDir),
		Uptime:        uptime,
		// The relay client connects within seconds of a start; this leaves room
		// for its backoff after a short outage.
		Settle: 3 * time.Minute,
		Wake:   wakeSupervisor,
		Log:    log,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	msg, err := f.Fetch(ctx)
	if err != nil {
		log.Error("automatic agent update failed", zap.Error(err))
		return 1
	}
	log.Info(msg, zap.String("repo", repo), zap.String("pin", pin))
	return 0
}

// wakeSupervisor signals the supervisor alone; the agent must not see SIGHUP.
func wakeSupervisor(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "kill", "--kill-whom=main", "--signal=SIGHUP", "wendyos-agent.service").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func run() int {
	if os.Getenv(supervisor.EnvSupervisor) != "" {
		// Uploaded as an agent by mistake: exit at once so its trial fails fast.
		fmt.Fprintln(os.Stderr, "wendy-agent-supervisor: refusing to run under another supervisor")
		return 1
	}
	// Before anything slow: until then SIGHUP would end the supervisor.
	wake := wakeOnHangup()
	log, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer log.Sync()

	self, _ := os.Executable()
	store := supervisor.Store{Dir: storeDir}
	imageVersion := agentVersion(imageAgent)
	// The rootfs cannot change while it runs, and hashing it again is not free.
	build := supervisor.BuildID("/")
	port := 50051
	sock := supervisor.AgentSocket
	if os.Getenv("WENDY_LOCAL_SOCKET") == "off" {
		sock = ""
	}
	if p, err := strconv.Atoi(os.Getenv("WENDY_AGENT_PORT")); err == nil {
		port = p
	}

	cfg := supervisor.Config{
		Store:        store,
		ImageAgent:   imageAgent,
		ImageVersion: imageVersion,
		Arch:         runtime.GOARCH,
		StatusPath:   supervisor.StatusPath,
		// Only settings an older agent must read back: identity, leases and
		// cursors move forward even when a trial is rejected.
		SnapshotDirs: []supervisor.SnapshotDir{
			{Path: "/data/etc/wendy-agent", Only: []string{"local-mesh.json", "nan-mesh.json", "mesh-disabled", "build-host-enabled"}},
			{Path: "/data/etc/NetworkManager/system-connections"},
		},
		Self:           self,
		TrialDeadline:  3 * time.Minute,
		MinTrialUptime: 10 * time.Second,
		ProbeEvery:     5 * time.Second,
		LivenessEvery:  time.Minute,
		LivenessMisses: 5,
		// Long enough for several slow probes while an OS install loads the disk.
		GraceMissWindow: time.Minute,
		RestartDelay:    2 * time.Second,
		StopTimeout:     80 * time.Second,
	}
	deps := supervisor.Deps{
		DataMounted:   func() bool { return isMountpoint("/data") },
		Emergency:     supervisor.EmergencyFunc("/config/wendy-agent-use-image", "/proc/cmdline"),
		BootID:        bootID,
		Build:         func() string { return build },
		Uptime:        uptime,
		RunVersion:    supervisor.SandboxedVersion,
		OSGate:        func(ctx context.Context) error { return osGate(ctx, imageVersion) },
		Probe:         supervisor.TrialProbe(store, sock, port),
		Alive:         supervisor.AliveProbe(sock),
		RemoteSeen:    supervisor.RemoteSeenFunc(supervisor.RemoteSeenDir),
		Serving:       supervisor.ServingProbe(port),
		ReloadNetwork: reloadNetwork,
		Wake:          wake,
		Updating:      supervisor.UpdatingFunc(supervisor.RemoteSeenDir),
		UnitProcs:     supervisor.UnitProcs,
		Log:           log,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	log.Info("starting wendy-agent-supervisor", zap.String("version", version.Version), zap.String("image_agent", imageVersion))
	if err := supervisor.New(cfg, deps).Run(ctx); err != nil {
		log.Error("supervisor stopped", zap.Error(err))
		return 1
	}
	return 0
}

// wakeOnHangup turns SIGHUP, which the automatic update sends, into a wake
// that is never lost and never queues more than one restart.
func wakeOnHangup() <-chan struct{} {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	wake := make(chan struct{}, 1)
	go func() {
		for range hup {
			select {
			case wake <- struct{}{}:
			default:
			}
		}
	}()
	return wake
}

// osGate runs the OS update commit/rollback with the image's agent. Only an
// agent from the same release is asked: an older one would treat the unknown
// `utils os-gate` as a normal start.
func osGate(ctx context.Context, imageVersion string) error {
	if imageVersion != version.Version && !version.IsDev(imageVersion) {
		return fmt.Errorf("image agent %s is not from this release (%s)", imageVersion, version.Version)
	}
	return runGate(ctx, imageAgent, 150*time.Second)
}

func runGate(ctx context.Context, agent string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, agent, "utils", "os-gate")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	// A timed-out gate takes its updater with it, or that would still be
	// committing while the image agent runs the gate again.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	return cmd.Run()
}

func reloadNetwork() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := exec.CommandContext(ctx, "nmcli", "connection", "reload").Run()
	if errors.Is(err, exec.ErrNotFound) {
		return nil
	}
	return err
}

func agentVersion(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return version.Version
	}
	v, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(v)
}

func isMountpoint(dir string) bool {
	var d, root syscall.Stat_t
	if syscall.Stat(dir, &d) != nil || syscall.Stat("/", &root) != nil {
		return false
	}
	return d.Dev != root.Dev
}

func bootID() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// uptime is CLOCK_BOOTTIME: it keeps counting while suspended and never jumps
// when the wall clock is corrected.
func uptime() time.Duration {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return 0
	}
	return time.Duration(ts.Nano())
}
