//go:build windows

package commands

import (
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// configurePostStartProcessGroup wires up Windows lifecycle controls for the
// postStart hook so that, on context cancellation, every descendant of cmd.exe
// (including grandchildren spawned via `start /B …`) is terminated atomically
// — even when cmd.exe has already exited, which is the common shape of a
// `start /B` hook.
//
// Strategy: a Job Object with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE. Once the
// last handle to the job is closed, the kernel terminates every process in
// it. Children inherit the job at creation time and remain bound to it for
// life, so the job survives cmd.exe exiting before its grandchildren do.
//
// The returned function MUST be called after cmd.Start(), regardless of
// whether Start succeeded:
//   - On success it assigns cmd.Process to the job so its descendants
//     inherit it.
//   - On failure (cmd.Process == nil) it releases the job handle.
//
// If Job Object setup fails for any reason (older Windows, weird permissions),
// we fall back to the previous taskkill /T behavior — strictly weaker but
// still better than the exec.CommandContext default.
func configurePostStartProcessGroup(cmd *exec.Cmd) func() {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		installTaskkillCancel(cmd)
		return func() {}
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		installTaskkillCancel(cmd)
		return func() {}
	}

	var (
		closeOnce sync.Once
		closeJob  = func() { closeOnce.Do(func() { _ = windows.CloseHandle(job) }) }
	)

	cmd.Cancel = func() error {
		// Closing the last handle to a job with KILL_ON_JOB_CLOSE terminates
		// every process in it — including cmd.exe and any descendants
		// spawned via `start /B`. This is the whole point of using a job.
		closeJob()
		return os.ErrProcessDone
	}
	// Belt-and-suspenders: if Wait is still pending after the cancel, force
	// kill the direct child too.
	cmd.WaitDelay = 5 * time.Second

	return func() {
		if cmd.Process == nil {
			closeJob()
			return
		}
		ph, err := windows.OpenProcess(
			windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
			false,
			uint32(cmd.Process.Pid),
		)
		if err != nil {
			closeJob()
			installTaskkillCancel(cmd)
			return
		}
		defer windows.CloseHandle(ph)
		if err := windows.AssignProcessToJobObject(job, ph); err != nil {
			closeJob()
			installTaskkillCancel(cmd)
			return
		}
	}
}

// installTaskkillCancel is the fallback when Job Object setup fails. It
// replaces cmd.Cancel with the original taskkill /T /F approach. This is
// strictly weaker than a job because it cannot reach descendants of an
// already-exited cmd.exe, but it preserves the prior behavior on hosts
// where job creation is unavailable.
func installTaskkillCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		return cmd.Process.Kill()
	}
}
