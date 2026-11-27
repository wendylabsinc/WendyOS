package replicaprobe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func paths(t *testing.T) (string, string, string) {
	t.Helper()
	base, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	commands, receipts := filepath.Join(base, "control"), filepath.Join(base, "proofs")
	for _, path := range []string{commands, receipts} {
		if e = os.Mkdir(path, 0700); e != nil {
			t.Fatal(e)
		}
	}
	return base, commands, receipts
}
func putCommand(t *testing.T, dir, name string, raw []byte) {
	t.Helper()
	if e := os.WriteFile(filepath.Join(dir, name), raw, 0600); e != nil {
		t.Fatal(e)
	}
}
func readReceipt(t *testing.T, path string) Receipt {
	t.Helper()
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var r Receipt
	if e = json.Unmarshal(raw, &r); e != nil {
		t.Fatal(e)
	}
	return r
}

func TestFilePhasesSaveExactReceiptsAndRefuseRestartReplay(t *testing.T) {
	_, f := fixture(t)
	_, commands, receipts := paths(t)
	for i, action := range []string{"publish", "query", "tombstone", "finish"} {
		putCommand(t, commands, []string{"000001.json", "000002.json", "000003.json", "000004.json"}[i], command(uint32(i+1), action))
	}
	out, e := Run(context.Background(), f.cfg, f, commands, receipts)
	if e != nil || out.Status != "complete" || out.Completed != 4 {
		t.Fatal(out, e)
	}
	r := readReceipt(t, filepath.Join(receipts, "000001.json"))
	if r.Status != "complete" || len(r.Originals) != 1 || !bytes.Equal(r.CommandJSON, command(1, "publish")) {
		t.Fatal("phase bytes lost")
	}
	before, e := os.ReadFile(filepath.Join(receipts, "000001.json"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Run(context.Background(), f.cfg, f, commands, receipts); e == nil {
		t.Fatal("used proof volume replayed")
	}
	after, _ := os.ReadFile(filepath.Join(receipts, "000001.json"))
	if !bytes.Equal(before, after) || f.publishCalls != 2 || f.queryCalls != 1 {
		t.Fatal("restart changed saved evidence or API calls")
	}
	entries, _ := os.ReadDir(receipts)
	if len(entries) != 6 {
		t.Fatal("unexpected receipt membership", len(entries))
	}
	for _, entry := range entries {
		info, _ := entry.Info()
		if info.Mode().Perm() != 0600 || !info.Mode().IsRegular() {
			t.Fatal("unsafe output", entry.Name())
		}
	}
}

func TestFileUnknownWritePreservesFailureAndDoesNotConsumeLaterCommand(t *testing.T) {
	_, f := fixture(t)
	f.publishError = true
	_, commands, receipts := paths(t)
	putCommand(t, commands, "000001.json", command(1, "publish"))
	putCommand(t, commands, "000002.json", command(2, "publish"))
	out, e := Run(context.Background(), f.cfg, f, commands, receipts)
	if !errors.Is(e, ErrUncertain) || out.Status != "unknown_outcome" || f.publishCalls != 1 {
		t.Fatal(out, e)
	}
	r := readReceipt(t, filepath.Join(receipts, "000001.json"))
	if r.Status != "unknown_outcome" || r.RPCCode != "DeadlineExceeded" || len(r.RequestJSON) == 0 {
		t.Fatal("failure evidence missing", r)
	}
	if _, e = os.Stat(filepath.Join(receipts, "000002.json")); !os.IsNotExist(e) {
		t.Fatal("later command consumed")
	}
	raw, e := os.ReadFile(filepath.Join(receipts, "result.json"))
	if e != nil || !bytes.Contains(raw, []byte("unknown_outcome")) {
		t.Fatal("terminal result missing", e)
	}
}

func TestCommandWaitDeadlineAndCancellationLeaveTerminalReceipt(t *testing.T) {
	for _, mode := range []string{"timeout", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			_, f := fixture(t)
			f.cfg.DurationMS = 100
			_, commands, receipts := paths(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancel" {
				cancel()
			}
			out, e := Run(ctx, f.cfg, f, commands, receipts)
			if e == nil || out.Status != "failed" || f.publishCalls != 0 || f.clockCalls != 0 {
				t.Fatal("wait did not stop", out, e)
			}
			if _, e = os.Stat(filepath.Join(receipts, "result.json")); e != nil {
				t.Fatal("missing cancellation result", e)
			}
		})
	}
}

func TestProtectedFilesRefuseLinksWritableInputsAndCollisions(t *testing.T) {
	for _, mode := range []string{"symlink-file", "hardlink-file", "writable-file", "symlink-directory", "existing-proof", "nested"} {
		t.Run(mode, func(t *testing.T) {
			_, f := fixture(t)
			base, commands, receipts := paths(t)
			name := filepath.Join(commands, "000001.json")
			switch mode {
			case "symlink-file", "hardlink-file":
				target := filepath.Join(base, "original.json")
				putCommand(t, base, "original.json", command(1, "publish"))
				var e error
				if mode == "symlink-file" {
					e = os.Symlink(target, name)
				} else {
					e = os.Link(target, name)
				}
				if e != nil {
					t.Fatal(e)
				}
			case "writable-file":
				putCommand(t, commands, "000001.json", command(1, "publish"))
				if e := os.Chmod(name, 0666); e != nil {
					t.Fatal(e)
				}
			case "symlink-directory":
				link := filepath.Join(base, "link")
				if e := os.Symlink(commands, link); e != nil {
					t.Fatal(e)
				}
				commands = link
			case "existing-proof":
				putCommand(t, receipts, "previous.json", []byte("keep"))
			case "nested":
				receipts = filepath.Join(commands, "nested")
				if e := os.Mkdir(receipts, 0700); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := Run(context.Background(), f.cfg, f, commands, receipts); e == nil || f.publishCalls != 0 || f.clockCalls != 0 {
				t.Fatal("unsafe paths reached API", mode, e)
			}
		})
	}
}

func TestAtomicReceiptNeverOverwritesPriorEvidence(t *testing.T) {
	_, _, receipts := paths(t)
	d, e := openDirectory(receipts)
	if e != nil {
		t.Fatal(e)
	}
	defer d.close()
	if e = d.write("saved.json", []byte("original"), 64); e != nil {
		t.Fatal(e)
	}
	if e = d.write("saved.json", []byte("changed"), 64); e == nil {
		t.Fatal("overwrote receipt")
	}
	raw, e := os.ReadFile(filepath.Join(receipts, "saved.json"))
	if e != nil || string(raw) != "original" {
		t.Fatal("prior bytes changed")
	}
	if e = d.write("../outside", []byte("bad"), 64); e == nil {
		t.Fatal("path escaped")
	}
}

func TestConfigBoundsAndNoAlternateSocket(t *testing.T) {
	_, f := fixture(t)
	base, _, _ := paths(t)
	path := filepath.Join(base, "probe.json")
	raw, _ := json.Marshal(f.cfg)
	if e := os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := LoadConfig(path); e != nil {
		t.Fatal(e)
	}
	for _, change := range []func(*Config){func(c *Config) { c.PeerDevice = c.Device }, func(c *Config) { c.SampleClockDomain = "utc" }, func(c *Config) { c.MaxCommands = 33 }, func(c *Config) { c.DurationMS = 600001 }, func(c *Config) { c.RunID = strings.Repeat("x", 129) }, func(c *Config) { c.ExpectedGeneration = 0; c.Seed = 1 << 32 }} {
		c := f.cfg
		change(&c)
		if _, e := New(c, f); e == nil {
			t.Fatal("invalid config accepted")
		}
	}
	if e := os.WriteFile(path, append(raw[:len(raw)-1], []byte(`,"private_key":"forbidden"}`)...), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := LoadConfig(path); e == nil {
		t.Fatal("unknown config accepted")
	}
	for _, path := range []string{"", "unix:///run/wendy/world/world.sock", "/run/wendy/admin.sock", "http://127.0.0.1", "/tmp/world.sock"} {
		if c, e := Dial(path); e == nil {
			_ = c.Close()
			t.Fatal("alternate endpoint accepted")
		}
	}
}

func TestCommandWaitIsBounded(t *testing.T) {
	_, f := fixture(t)
	_, commands, receipts := paths(t)
	f.cfg.DurationMS = 100
	start := time.Now()
	_, e := Run(context.Background(), f.cfg, f, commands, receipts)
	if !errors.Is(e, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatal("unbounded wait", e)
	}
}
