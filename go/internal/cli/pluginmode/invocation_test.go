package pluginmode

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeProcess makes the running executable exe and PATH's wendy onPath ("" =
// not on PATH) for the rest of the test.
func fakeProcess(t *testing.T, exe, onPath string) {
	t.Helper()
	oldExe, oldLook := executable, lookPath
	executable = func() (string, error) { return exe, nil }
	lookPath = func(string) (string, error) {
		if onPath == "" {
			return "", exec.ErrNotFound
		}
		return onPath, nil
	}
	t.Cleanup(func() { executable, lookPath = oldExe, oldLook })
}

// managedInstall lays out <root>/cli/<version>/wendy and, when withPointer,
// the <root>/bin/wendy pointer. It returns both paths, resolved.
func managedInstall(t *testing.T, root string, withPointer bool) (exe, pointer string) {
	t.Helper()
	name := "wendy"
	if runtime.GOOS == "windows" {
		name = "wendy.exe"
	}
	exe = filepath.Join(root, "cli", "2026.10.01-120000", name)
	writeFile(t, exe, "#!/bin/sh\n")
	pointer = filepath.Join(root, "bin", name)
	if withPointer {
		writeFile(t, pointer, "#!/bin/sh\n") // a copy is enough: only its existence matters
	}
	return resolvePath(exe), filepath.Join(resolvePath(root), "bin", name)
}

func TestCLIInvocation(t *testing.T) {
	t.Run("Homebrew binary", func(t *testing.T) {
		brew := filepath.Join(t.TempDir(), "homebrew", "bin", "wendy")
		writeFile(t, brew, "#!/bin/sh\n")
		fakeProcess(t, brew, "")
		if got := CLIInvocation(); got != "wendy" {
			t.Errorf("CLIInvocation = %q, want wendy", got)
		}
	})
	t.Run("managed, not on PATH: the bin pointer", func(t *testing.T) {
		exe, pointer := managedInstall(t, t.TempDir(), true)
		fakeProcess(t, exe, "")
		if got := CLIInvocation(); got != pointer {
			t.Errorf("CLIInvocation = %q, want %q", got, pointer)
		}
	})
	t.Run("managed, PATH reaches the pointer", func(t *testing.T) {
		exe, pointer := managedInstall(t, t.TempDir(), true)
		fakeProcess(t, exe, pointer)
		if got := CLIInvocation(); got != "wendy" {
			t.Errorf("CLIInvocation = %q, want wendy", got)
		}
	})
	t.Run("managed, PATH reaches the versioned binary", func(t *testing.T) {
		exe, _ := managedInstall(t, t.TempDir(), true)
		fakeProcess(t, exe, exe)
		if got := CLIInvocation(); got != "wendy" {
			t.Errorf("CLIInvocation = %q, want wendy", got)
		}
	})
	t.Run("managed, PATH reaches a different wendy", func(t *testing.T) {
		exe, pointer := managedInstall(t, t.TempDir(), true)
		other := filepath.Join(t.TempDir(), "wendy")
		writeFile(t, other, "#!/bin/sh\n")
		fakeProcess(t, exe, other)
		if got := CLIInvocation(); got != pointer {
			t.Errorf("CLIInvocation = %q, want %q", got, pointer)
		}
	})
	t.Run("managed, no pointer: the versioned binary", func(t *testing.T) {
		exe, _ := managedInstall(t, t.TempDir(), false)
		fakeProcess(t, exe, "")
		if got := CLIInvocation(); got != exe {
			t.Errorf("CLIInvocation = %q, want %q", got, exe)
		}
	})
	t.Run("a path with a space is quoted", func(t *testing.T) {
		exe, pointer := managedInstall(t, filepath.Join(t.TempDir(), "John Doe", ".wendy"), true)
		fakeProcess(t, exe, "")
		if got, want := CLIInvocation(), `"`+pointer+`"`; got != want {
			t.Errorf("CLIInvocation = %q, want %q", got, want)
		}
	})
}

func TestRewriteCLIHints(t *testing.T) {
	const msg = "auth entry has no certificates; re-run 'wendy auth login'. Update it with `wendy device update`."
	t.Run("not managed: unchanged", func(t *testing.T) {
		fakeProcess(t, "/opt/homebrew/bin/wendy", "")
		if got := RewriteCLIHints(msg); got != msg {
			t.Errorf("RewriteCLIHints = %q, want it unchanged", got)
		}
	})
	t.Run("managed, not on PATH: both quote styles", func(t *testing.T) {
		exe, pointer := managedInstall(t, t.TempDir(), true)
		fakeProcess(t, exe, "")
		want := "auth entry has no certificates; re-run '" + pointer + " auth login'. Update it with `" + pointer + " device update`."
		if got := RewriteCLIHints(msg); got != want {
			t.Errorf("RewriteCLIHints =\n%q\nwant\n%q", got, want)
		}
	})
	t.Run("text without a command is untouched", func(t *testing.T) {
		exe, _ := managedInstall(t, t.TempDir(), true)
		fakeProcess(t, exe, "")
		const plain = "Wendy device wendy-pi is offline"
		if got := RewriteCLIHints(plain); got != plain {
			t.Errorf("RewriteCLIHints = %q, want it unchanged", got)
		}
	})
}
