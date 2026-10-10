package commands

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The hidden "__usb-setup" subcommand is the privileged half of the USB-C
// auto-setup flow, re-executed under sudo by maybeOfferUSBSetup.
func TestNewUSBSetupHiddenCmd_Flags(t *testing.T) {
	cmd := newUSBSetupHiddenCmd()
	if cmd.Use != "__usb-setup" {
		t.Fatalf("Use = %q, want __usb-setup", cmd.Use)
	}
	if !cmd.Hidden {
		t.Error("expected __usb-setup to be hidden")
	}
	if cmd.Flags().Lookup("iface") == nil {
		t.Error("missing flag --iface")
	}
}

// An agent or script can't answer the USB-C setup prompt in `wendy discover`,
// so it gets this notice instead. It must name the interface and send a person
// to that prompt, since there is no setup command to run.
func TestPendingUSBSetupNotice(t *testing.T) {
	orig := pendingUSBSetupIface
	t.Cleanup(func() { pendingUSBSetupIface = orig })

	pendingUSBSetupIface = func() string { return "" }
	if got := pendingUSBSetupNotice(); got != "" {
		t.Errorf("no link to configure: notice = %q, want none", got)
	}

	pendingUSBSetupIface = func() string { return "enxaa" }
	got := pendingUSBSetupNotice()
	for _, want := range []string{"enxaa", "`wendy discover`", "sudo"} {
		if !strings.Contains(got, want) {
			t.Errorf("notice %q should mention %s", got, want)
		}
	}
	if strings.Contains(got, "usb-setup") {
		t.Errorf("notice %q names a usb-setup command, which doesn't exist", got)
	}
}

// USB-C setup is implicit: `wendy discover` offers it. AGENTS.md, the docs and
// the agent skills once sent people to a `wendy device usb-setup` command that
// didn't exist; none of them may name a usb-setup command again.
func TestDocsNameNoUSBSetupCommand(t *testing.T) {
	re := regexp.MustCompile("`(?:sudo )?wendy [^`]*usb-setup[^`]*`")
	check := func(name string, data []byte) {
		for _, m := range re.FindAll(data, -1) {
			t.Errorf("%s: %s names a usb-setup command; point at `wendy discover`'s USB-C setup prompt instead", name, m)
		}
	}
	repoRoot := filepath.Join("..", "..", "..", "..")
	agents, err := os.ReadFile(filepath.Join(repoRoot, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	check("AGENTS.md", agents)
	for _, root := range []string{
		filepath.Join("..", "assets", "docs"),
		filepath.Join("..", "assets", "skills"),
		filepath.Join(repoRoot, "plugins"),
	} {
		scanned := 0
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// The ignored docs-site content copy can outlive its source
				// checkout. Scan the canonical embedded docs above instead.
				if path == filepath.Join("..", "assets", "docs", "content") {
					return filepath.SkipDir
				}
				// Skip untracked local content: installed packages, build output
				// and gitignored working notes.
				if n := d.Name(); path != root && (n == "node_modules" || n == "superpowers" || strings.HasPrefix(n, ".")) {
					return filepath.SkipDir
				}
				return nil
			}
			if ext := filepath.Ext(path); ext != ".md" && ext != ".mdx" {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scanned++
			check(path, data)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if scanned == 0 {
			t.Fatalf("scanned no markdown under %s; the walk is broken", root)
		}
	}
}
