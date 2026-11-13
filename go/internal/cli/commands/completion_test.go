package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func executeCompletion(t *testing.T, args ...string) (stdout, stderr *bytes.Buffer, err error) {
	t.Helper()
	return executeCompletionInHome(t, t.TempDir(), args...)
}

func executeCompletionInHome(t *testing.T, home string, args ...string) (stdout, stderr *bytes.Buffer, err error) {
	t.Helper()
	// Isolate from host env so the command uses a self-contained sandbox.
	// Without this, host home/ZDOTDIR/XDG_* values could redirect rc and script
	// paths outside the test's tmp dir and clobber the developer's real config.
	for _, k := range []string{"HOME", "USERPROFILE"} {
		t.Setenv(k, home)
	}
	t.Setenv("WENDY_CONFIG_DIR", filepath.Join(home, ".wendy"))
	for _, k := range []string{"HOMEDRIVE", "HOMEPATH", "ZDOTDIR", "XDG_DATA_HOME", "XDG_CONFIG_HOME"} {
		t.Setenv(k, "")
	}
	root := NewRootCmd()
	root.PersistentPreRunE = nil
	root.PersistentPostRunE = nil
	stdout = new(bytes.Buffer)
	stderr = new(bytes.Buffer)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(args)
	err = root.Execute()
	return stdout, stderr, err
}

func TestCompletion_GeneratesNonEmpty(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		t.Run(shell, func(t *testing.T) {
			stdout, _, err := executeCompletion(t, "completion", shell)
			if err != nil {
				t.Fatalf("completion %s: %v", shell, err)
			}
			if stdout.Len() < 200 {
				t.Errorf("completion %s output too short (%d bytes); got: %q", shell, stdout.Len(), stdout.String())
			}
		})
	}
}

func TestCompletion_HasInstallSubcommand(t *testing.T) {
	root := NewRootCmd()
	var compCmd *cobra.Command
	for _, c := range root.Commands() {
		if c.Name() == "completion" {
			compCmd = c
			break
		}
	}
	if compCmd == nil {
		t.Fatal("expected `completion` subcommand on root")
	}
	if compCmd.GroupID != "settings" {
		t.Errorf("completion.GroupID = %q; want %q", compCmd.GroupID, "settings")
	}
	want := map[string]bool{"bash": true, "zsh": true, "fish": true, "powershell": true, "install": true}
	have := []string{}
	for _, c := range compCmd.Commands() {
		have = append(have, c.Name())
		delete(want, c.Name())
	}
	if len(want) > 0 {
		t.Errorf("missing completion children: %v (have: %v)", want, have)
	}
}

func TestDetectShell(t *testing.T) {
	tests := []struct {
		name     string
		override string
		goos     string
		shellEnv string
		want     string
		wantErr  bool
	}{
		{name: "override bash", override: "bash", goos: "linux", want: "bash"},
		{name: "override pwsh maps to powershell", override: "pwsh", goos: "linux", want: "powershell"},
		{name: "override unsupported", override: "tcsh", goos: "linux", wantErr: true},
		{name: "windows defaults to powershell", goos: "windows", want: "powershell"},
		{name: "linux bash", goos: "linux", shellEnv: "/bin/bash", want: "bash"},
		{name: "linux zsh", goos: "linux", shellEnv: "/usr/bin/zsh", want: "zsh"},
		{name: "macos fish via brew", goos: "darwin", shellEnv: "/opt/homebrew/bin/fish", want: "fish"},
		{name: "linux unknown", goos: "linux", shellEnv: "/bin/tcsh", wantErr: true},
		{name: "linux empty SHELL", goos: "linux", shellEnv: "", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := func(k string) string {
				if k == "SHELL" {
					return tc.shellEnv
				}
				return ""
			}
			got, err := detectShell(tc.override, tc.goos, env)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v; wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("got %q; want %q", got, tc.want)
			}
		})
	}
}

func TestComputeInstallPlan(t *testing.T) {
	tests := []struct {
		name        string
		shell       string
		goos        string
		home        string
		env         map[string]string
		exists      map[string]bool
		wantScript  string
		wantRC      string
		wantBlockIn string // substring expected in rcBlock; empty if no rc edit
	}{
		{
			name:       "bash with bash-completion present uses XDG path",
			shell:      "bash",
			goos:       "linux",
			home:       "/h/u",
			exists:     map[string]bool{"/h/u/.local/share/bash-completion": true},
			wantScript: "/h/u/.local/share/bash-completion/completions/wendy",
		},
		{
			name:        "bash without bash-completion uses fallback",
			shell:       "bash",
			goos:        "linux",
			home:        "/h/u",
			wantScript:  "/h/u/.wendy/completions/wendy.bash",
			wantRC:      "/h/u/.bashrc",
			wantBlockIn: "/h/u/.wendy/completions/wendy.bash",
		},
		{
			name:        "bash darwin fallback notes bash_profile",
			shell:       "bash",
			goos:        "darwin",
			home:        "/h/u",
			wantScript:  "/h/u/.wendy/completions/wendy.bash",
			wantRC:      "/h/u/.bashrc",
			wantBlockIn: completionRcSentinel,
		},
		{
			name:       "bash honors XDG_DATA_HOME",
			shell:      "bash",
			goos:       "linux",
			home:       "/h/u",
			env:        map[string]string{"XDG_DATA_HOME": "/x/data"},
			exists:     map[string]bool{"/x/data/bash-completion": true},
			wantScript: "/x/data/bash-completion/completions/wendy",
		},
		{
			name:        "zsh default home",
			shell:       "zsh",
			home:        "/h/u",
			wantScript:  "/h/u/.zfunc/_wendy",
			wantRC:      "/h/u/.zshrc",
			wantBlockIn: "fpath=",
		},
		{
			name:        "zsh honors ZDOTDIR for rc",
			shell:       "zsh",
			home:        "/h/u",
			env:         map[string]string{"ZDOTDIR": "/h/u/.config/zsh"},
			wantScript:  "/h/u/.zfunc/_wendy",
			wantRC:      "/h/u/.config/zsh/.zshrc",
			wantBlockIn: "fpath=",
		},
		{
			name:       "fish default config",
			shell:      "fish",
			home:       "/h/u",
			wantScript: "/h/u/.config/fish/completions/wendy.fish",
		},
		{
			name:       "fish honors XDG_CONFIG_HOME",
			shell:      "fish",
			home:       "/h/u",
			env:        map[string]string{"XDG_CONFIG_HOME": "/x/cfg"},
			wantScript: "/x/cfg/fish/completions/wendy.fish",
		},
		{
			name:        "powershell on linux uses .config/powershell",
			shell:       "powershell",
			goos:        "linux",
			home:        "/h/u",
			wantScript:  "/h/u/.config/powershell/Completions/wendy.ps1",
			wantRC:      "/h/u/.config/powershell/Microsoft.PowerShell_profile.ps1",
			wantBlockIn: "/h/u/.config/powershell/Completions/wendy.ps1",
		},
		{
			name:        "powershell on windows uses Documents/PowerShell",
			shell:       "powershell",
			goos:        "windows",
			home:        `C:\Users\u`,
			wantScript:  filepath.Join(`C:\Users\u`, "Documents", "PowerShell", "Completions", "wendy.ps1"),
			wantRC:      filepath.Join(`C:\Users\u`, "Documents", "PowerShell", "Microsoft.PowerShell_profile.ps1"),
			wantBlockIn: completionRcSentinel,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := func(k string) string { return tc.env[k] }
			exists := func(p string) bool { return tc.exists[p] }
			plan, err := computeInstallPlan(tc.shell, tc.goos, tc.home, env, exists)
			if err != nil {
				t.Fatalf("computeInstallPlan: %v", err)
			}
			if plan.scriptPath != tc.wantScript {
				t.Errorf("scriptPath = %q; want %q", plan.scriptPath, tc.wantScript)
			}
			if plan.rcPath != tc.wantRC {
				t.Errorf("rcPath = %q; want %q", plan.rcPath, tc.wantRC)
			}
			if tc.wantBlockIn != "" && !strings.Contains(plan.rcBlock, tc.wantBlockIn) {
				t.Errorf("rcBlock = %q; want substring %q", plan.rcBlock, tc.wantBlockIn)
			}
		})
	}
}

func TestComputeInstallPlan_UnsupportedShell(t *testing.T) {
	_, err := computeInstallPlan("tcsh", "linux", "/h/u",
		func(string) string { return "" },
		func(string) bool { return false },
	)
	if err == nil {
		t.Fatal("expected error for unsupported shell")
	}
}

func TestInstall_HelpDoesNotExposeOutputDir(t *testing.T) {
	stdout, _, err := executeCompletion(t, "completion", "install", "--help")
	if err != nil {
		t.Fatalf("install --help: %v", err)
	}
	if strings.Contains(stdout.String(), "output-dir") {
		t.Errorf("install help exposed output-dir seam: %s", stdout.String())
	}
}

func TestInstall_OutputDirRejected(t *testing.T) {
	stdout, stderr, err := executeCompletion(t,
		"completion", "install",
		"--shell", "fish",
		"--output-dir", t.TempDir(),
	)
	if err == nil {
		t.Fatal("expected --output-dir to be rejected")
	}
	if stdout.Len() != 0 {
		t.Errorf("unexpected stdout: %s", stdout.String())
	}
	if !strings.Contains(err.Error(), "unknown flag: --output-dir") && !strings.Contains(stderr.String(), "unknown flag: --output-dir") {
		t.Errorf("missing unknown flag diagnostic; err: %v; stderr: %s", err, stderr.String())
	}
}

func TestInstall_WritesFile(t *testing.T) {
	tmp := t.TempDir()
	stdout, _, err := executeCompletionInHome(t, tmp,
		"completion", "install",
		"--shell", "fish",
	)
	if err != nil {
		t.Fatalf("install: %v\nstdout: %s", err, stdout.String())
	}

	scriptPath := filepath.Join(tmp, ".config", "fish", "completions", "wendy.fish")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("expected file at %s: %v", scriptPath, err)
	}
	if len(data) < 200 {
		t.Errorf("fish script too short (%d bytes)", len(data))
	}
	// Fish completion scripts use the `complete` builtin.
	if !bytes.Contains(data, []byte("complete")) {
		t.Errorf("fish script missing `complete` keyword; got: %s", data)
	}
}

func TestInstall_SetsCompletionInstalledFlag(t *testing.T) {
	tmp := t.TempDir()
	// HOME is pinned to tmp for the rest of this test, so config lives under
	// tmp/.wendy and stays isolated from the developer's real config.
	if _, _, err := executeCompletionInHome(t, tmp,
		"completion", "install",
		"--shell", "fish",
	); err != nil {
		t.Fatalf("install: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if !cfg.CompletionInstalled {
		t.Error("CompletionInstalled = false after `completion install`; want true")
	}
}

func TestInstall_PrintPathDoesNotSetFlag(t *testing.T) {
	tmp := t.TempDir()
	if _, _, err := executeCompletionInHome(t, tmp,
		"completion", "install",
		"--shell", "fish",
		"--print-path",
	); err != nil {
		t.Fatalf("install --print-path: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.CompletionInstalled {
		t.Error("CompletionInstalled = true after dry-run --print-path; want false")
	}
}

func TestInstall_Stdout(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		t.Run(shell, func(t *testing.T) {
			tmp := t.TempDir()
			stdout, _, err := executeCompletionInHome(t, tmp,
				"completion", "install",
				"--shell", shell,
				"--stdout",
			)
			if err != nil {
				t.Fatalf("install --stdout: %v", err)
			}
			if stdout.Len() < 200 {
				t.Errorf("stdout too short (%d bytes); got: %q", stdout.Len(), stdout.String())
			}
			// --stdout must not write any files or rc edits.
			entries, err := os.ReadDir(tmp)
			if err != nil {
				t.Fatalf("readdir: %v", err)
			}
			if len(entries) != 0 {
				names := make([]string, 0, len(entries))
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Errorf("--stdout wrote into tmp dir: %v", names)
			}
		})
	}
}

func TestInstall_StdoutAndPrintPathConflict(t *testing.T) {
	_, _, err := executeCompletion(t,
		"completion", "install",
		"--shell", "bash",
		"--stdout",
		"--print-path",
	)
	if err == nil {
		t.Fatal("expected error when both --stdout and --print-path are set")
	}
}

func TestInstall_PrintPath(t *testing.T) {
	tmp := t.TempDir()
	stdout, _, err := executeCompletionInHome(t, tmp,
		"completion", "install",
		"--shell", "zsh",
		"--print-path",
	)
	if err != nil {
		t.Fatalf("install --print-path: %v", err)
	}
	out := stdout.String()
	wantScript := filepath.Join(tmp, ".zfunc", "_wendy")
	wantRC := filepath.Join(tmp, ".zshrc")
	if !strings.Contains(out, wantScript) {
		t.Errorf("output missing script path %q; got: %q", wantScript, out)
	}
	if !strings.Contains(out, wantRC) {
		t.Errorf("output missing rc path %q; got: %q", wantRC, out)
	}
	// --print-path must not write anything.
	if _, err := os.Stat(wantScript); err == nil {
		t.Error("--print-path should not have created the script file")
	}
}

func TestInstall_RcLineIdempotent(t *testing.T) {
	tmp := t.TempDir()

	// First install: should add the sentinel block.
	if _, _, err := executeCompletionInHome(t, tmp,
		"completion", "install",
		"--shell", "zsh",
	); err != nil {
		t.Fatalf("install 1: %v", err)
	}

	rcPath := filepath.Join(tmp, ".zshrc")
	first, err := os.ReadFile(rcPath)
	if err != nil {
		t.Fatalf("read rc: %v", err)
	}
	if c := bytes.Count(first, []byte(completionRcSentinel)); c != 1 {
		t.Fatalf("after first install: sentinel count = %d; want 1", c)
	}

	// Second install: must not re-append.
	if _, _, err := executeCompletionInHome(t, tmp,
		"completion", "install",
		"--shell", "zsh",
	); err != nil {
		t.Fatalf("install 2: %v", err)
	}
	second, err := os.ReadFile(rcPath)
	if err != nil {
		t.Fatalf("read rc 2: %v", err)
	}
	if c := bytes.Count(second, []byte(completionRcSentinel)); c != 1 {
		t.Errorf("after second install: sentinel count = %d; want 1", c)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("rc file changed between idempotent installs:\nbefore: %s\nafter: %s", first, second)
	}
}

func TestRcBlock_QuotesPathsWithSpecialChars(t *testing.T) {
	// Paths containing $, backticks, single-quotes, or spaces must be safely
	// quoted in rc blocks so the shell doesn't expand them.
	awkward := "/h/u with $var and `cmd` and 'quote'"

	bash, err := computeInstallPlan("bash", "linux", awkward,
		func(string) string { return "" }, func(string) bool { return false })
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	for _, bad := range []string{"$var", "`cmd`"} {
		// The dollar/backtick should appear only inside the literal path,
		// never bare in shell-interpreted positions. POSIX single quotes
		// escape everything except `'` itself, so the literal `$var` must
		// remain in the block but bash treats it literally there.
		if !strings.Contains(bash.rcBlock, bad) {
			t.Errorf("bash rcBlock missing literal %q in path", bad)
		}
	}
	// Single quotes must be POSIX-escaped as '\''.
	if !strings.Contains(bash.rcBlock, `'\''`) {
		t.Errorf("bash rcBlock did not POSIX-escape single quote; got: %q", bash.rcBlock)
	}

	ps, err := computeInstallPlan("powershell", "linux", awkward,
		func(string) string { return "" }, func(string) bool { return false })
	if err != nil {
		t.Fatalf("powershell: %v", err)
	}
	// PowerShell single quotes are doubled.
	if !strings.Contains(ps.rcBlock, "''") {
		t.Errorf("powershell rcBlock did not double single quote; got: %q", ps.rcBlock)
	}
	// PowerShell rc must NOT use double quotes around the path (would interpolate $var).
	if strings.Contains(ps.rcBlock, `. "`) {
		t.Errorf("powershell rcBlock used double quotes (would interpolate $var); got: %q", ps.rcBlock)
	}
}

func TestEnsureBlockInFile_AppendsLeadingNewline(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "rc")
	if err := os.WriteFile(path, []byte("existing-line-no-newline"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	added, err := ensureBlockInFile(path, "# sentinel", "# sentinel\nblock-line")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !added {
		t.Error("expected added = true on first call")
	}
	got, _ := os.ReadFile(path)
	want := "existing-line-no-newline\n# sentinel\nblock-line\n"
	if string(got) != want {
		t.Errorf("got %q; want %q", got, want)
	}
}
