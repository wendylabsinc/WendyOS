package authority

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func canonicalJournalTestPath(t *testing.T, path string) string {
	t.Helper()
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(parent, filepath.Base(path))
}

func TestReplayJournalMigratesLegacyAndRefusesOldReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger")
	legacy := replayState{Version: Version, Nonces: map[string]int64{"retained": admissionTestNow.Add(time.Minute).UnixMilli()}, Revision: 3, Digest: "three", LastUnixMS: admissionTestNow.UnixMilli()}
	raw, _ := json.Marshal(legacy)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	c := openAdmissionCache(t, path, 8)
	if err := c.Consume("retained", admissionTestNow.Add(time.Minute), admissionTestNow); !errors.Is(err, ErrReplay) {
		t.Fatal("migration lost nonce", err)
	}
	if err := c.Checkpoint(2, "old", admissionTestNow); err == nil {
		t.Fatal("migration lost revision")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte(journalMagic)) {
		t.Fatal("legacy file was not migrated")
	}
	if json.Unmarshal(data, new(replayState)) == nil {
		t.Fatal("old JSON reader accepted journal")
	}
	if other, err := OpenReplay(path, 8); err == nil {
		other.Close()
		t.Fatal("second writer accepted")
	}
	c.Close()
	c = openAdmissionCache(t, path, 8)
	if err := c.ValidateCheckpoint(3, "three", admissionTestNow.Add(-time.Millisecond)); err == nil {
		t.Fatal("migrated floor lost after reopen")
	}
}

func TestReplayJournalMigrationFailuresLeaveRecoverableState(t *testing.T) {
	for _, phase := range []string{"before_install", "after_rename"} {
		t.Run(phase, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ledger")
			legacy := replayState{Version: Version, Nonces: map[string]int64{"retained": admissionTestNow.Add(time.Minute).UnixMilli()}, Revision: 1, Digest: "one", LastUnixMS: admissionTestNow.UnixMilli()}
			data, _ := json.Marshal(legacy)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			probe := NewMemoryReplay(8)
			probe.path = canonicalJournalTestPath(t, path)
			injected := errors.New("test migration failure")
			if phase == "before_install" {
				probe.syncFile = func(*os.File) error { return injected }
			} else {
				probe.syncDirectory = func(string) error { return injected }
			}
			// Exercise the actual OpenReplay migration helper with private failure
			// seams. The fixture has no competing process or external authority.
			if err := probe.openJournal(); !errors.Is(err, injected) {
				t.Fatal(err)
			}
			if probe.poisoned != (phase == "after_rename") {
				t.Fatal("wrong migration uncertainty")
			}
			probe.Close()
			after, _ := os.ReadFile(path)
			if phase == "before_install" && !bytes.Equal(after, data) {
				t.Fatal("failed migration destroyed legacy state")
			}
			c := openAdmissionCache(t, path, 8)
			if err := c.Consume("retained", admissionTestNow.Add(time.Minute), admissionTestNow); !errors.Is(err, ErrReplay) {
				t.Fatal("migration failure lost nonce", err)
			}
		})
	}
}

func TestReplayJournalAppendDoesNotReplaceFileOrEncodeWholeMap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger")
	c := openAdmissionCache(t, path, 128)
	for i := 0; i < 100; i++ {
		if err := c.Consume(fmt.Sprintf("key-%03d", i), admissionTestNow.Add(time.Hour), admissionTestNow); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	offset := c.offset
	c.syncDirectory = func(string) error { t.Fatal("steady append synchronized directory"); return nil }
	syncs := 0
	c.syncFile = func(f *os.File) error { syncs++; return f.Sync() }
	if err := c.Checkpoint(1, "digest", admissionTestNow.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) || syncs != 1 {
		t.Fatal("append replaced inode or used multiple syncs", syncs)
	}
	data, _ := os.ReadFile(path)
	tail := data[offset:]
	if len(tail) > 1024 || bytes.Contains(tail, []byte("key-099")) || bytes.Contains(tail, []byte(`"nonces"`)) {
		t.Fatal("checkpoint encoded nonce map")
	}
	if c.offset != int64(len(data)) {
		t.Fatal("wrong append offset")
	}
}

func TestReplayJournalLogicalSizeMatchesJSON(t *testing.T) {
	c := NewMemoryReplay(10)
	for i, key := range []string{"<>&", "quote\"slash\\", "unicode\u2028", "last"} {
		s, err := c.candidate(admissionTestNow.Add(time.Duration(i) * time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		s.Nonces[key] = admissionTestNow.Add(time.Duration(i+1) * time.Millisecond).UnixMilli()
		_, size, err := c.prepareMutation(s)
		if err != nil {
			t.Fatal(err)
		}
		got, err := logicalSize(s, size)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(s)
		if got != int64(len(encoded)) {
			t.Fatalf("size %d != serialized %d", got, len(encoded))
		}
		c.state = s
		c.nonceBytes = size
	}
}

func TestReplayJournalMillisecondExpiryCompatibility(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger")
	c := openAdmissionCache(t, path, 1)
	now := admissionTestNow.Add(100 * time.Microsecond)
	if err := c.Consume("same-ms", now.Add(time.Microsecond), now); err != nil {
		t.Fatal(err)
	}
	c.Close()
	c = openAdmissionCache(t, path, 1)
	if c.state.Nonces["same-ms"] != now.UnixMilli() {
		t.Fatal("rounded expiry changed")
	}
	if err := c.Consume("same-ms", now.Add(time.Microsecond), now); err != nil {
		t.Fatal("expired rounded nonce not pruned", err)
	}
	c.Close()
	c = openAdmissionCache(t, path, 1)
	if c.state.LastUnixMS != now.UnixMilli() {
		t.Fatal("rounded clock floor changed")
	}
}

func TestReplayJournalIncompleteTailRecoveryEveryByte(t *testing.T) {
	path := filepath.Join(t.TempDir(), "original")
	c := openAdmissionCache(t, path, 8)
	if err := c.CheckpointAndConsume(1, "one", "acknowledged", admissionTestNow.Add(time.Minute), admissionTestNow); err != nil {
		t.Fatal(err)
	}
	prefix, _ := os.ReadFile(path)
	if err := c.CheckpointAndConsume(2, "two", "new", admissionTestNow.Add(time.Minute), admissionTestNow.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	all, _ := os.ReadFile(path)
	tail := all[len(prefix):]
	c.Close()
	// Every truncation of the last frame models interruption before its fsync
	// acknowledgment. Already-fsynced prefix bytes are not externally damaged.
	for cut := 0; cut < len(tail); cut++ {
		candidate := filepath.Join(t.TempDir(), "torn")
		if err := os.WriteFile(candidate, append(append([]byte(nil), prefix...), tail[:cut]...), 0600); err != nil {
			t.Fatal(err)
		}
		reopened, err := OpenReplay(candidate, 8)
		if err != nil {
			t.Fatalf("cut%d: %v", cut, err)
		}
		if err = reopened.Consume("acknowledged", admissionTestNow.Add(time.Minute), admissionTestNow); !errors.Is(err, ErrReplay) {
			t.Fatal("prefix nonce lost", cut, err)
		}
		if reopened.state.Revision != 1 || reopened.offset != int64(len(prefix)) {
			t.Fatal("torn tail changed prefix", cut)
		}
		if err = reopened.CheckpointAndConsume(2, "two", "replacement", admissionTestNow.Add(time.Minute), admissionTestNow.Add(time.Millisecond)); err != nil {
			t.Fatal("append after recovery", cut, err)
		}
		reopened.Close()
		again, err := OpenReplay(candidate, 8)
		if err != nil {
			t.Fatal("recovered hash chain invalid", cut, err)
		}
		again.Close()
	}
}

func TestReplayJournalRefusesCompleteCorruptionAndIncompleteBase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "original")
	c := openAdmissionCache(t, path, 8)
	if err := c.Checkpoint(1, "one", admissionTestNow); err != nil {
		t.Fatal(err)
	}
	base, _ := os.ReadFile(path)
	if err := c.Consume("nonce", admissionTestNow.Add(time.Minute), admissionTestNow); err != nil {
		t.Fatal(err)
	}
	firstEnd := c.offset
	if err := c.Checkpoint(2, "two", admissionTestNow.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	all, _ := os.ReadFile(path)
	c.Close()
	for name, edit := range map[string]func([]byte) []byte{
		"last_length":          func(b []byte) []byte { b[firstEnd] ^= 1; return b },
		"last_header":          func(b []byte) []byte { b[firstEnd+40] ^= 1; return b },
		"last_payload":         func(b []byte) []byte { b[len(b)-1] ^= 1; return b },
		"interior_payload":     func(b []byte) []byte { b[len(base)+journalHeaderBytes] ^= 1; return b },
		"removed_middle":       func(b []byte) []byte { return append(b[:len(base)], b[firstEnd:]...) },
		"partial_base_header":  func(b []byte) []byte { return b[:len(journalMagic)+20] },
		"partial_base_payload": func(b []byte) []byte { return b[:len(base)-1] },
	} {
		t.Run(name, func(t *testing.T) {
			data := edit(append([]byte(nil), all...))
			candidate := filepath.Join(t.TempDir(), "bad")
			os.WriteFile(candidate, data, 0600)
			if opened, err := OpenReplay(candidate, 8); err == nil {
				opened.Close()
				t.Fatal("corrupt ledger accepted")
			}
			after, _ := os.ReadFile(candidate)
			if !bytes.Equal(after, data) {
				t.Fatal("corruption was silently truncated")
			}
		})
	}
}

func TestReplayJournalPartialWriteAndSyncFailurePoison(t *testing.T) {
	for _, kind := range []string{"partial", "short", "sync"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ledger")
			c := openAdmissionCache(t, path, 8)
			if err := c.Checkpoint(1, "one", admissionTestNow); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected append failure")
			if kind == "sync" {
				c.syncFile = func(*os.File) error { return injected }
			} else {
				c.appendWrite = func(data []byte) (int, error) {
					n, err := c.file.WriteAt(data[:journalHeaderBytes+1], c.offset)
					if err != nil {
						return n, err
					}
					if kind == "short" {
						return n, nil
					}
					return n, injected
				}
			}
			err := c.CheckpointAndConsume(2, "two", "candidate", admissionTestNow.Add(time.Minute), admissionTestNow.Add(time.Millisecond))
			if err == nil || !c.poisoned || c.state.Revision != 1 {
				t.Fatal("uncertain append admitted", err)
			}
			if _, exists := c.state.Nonces["candidate"]; exists {
				t.Fatal("failed candidate visible")
			}
			if err = c.Checkpoint(3, "three", admissionTestNow.Add(time.Second)); err == nil {
				t.Fatal("poisoned writer reused")
			}
			c.Close()
			c = openAdmissionCache(t, path, 8)
			_, retained := c.state.Nonces["candidate"]
			if retained != (kind == "sync") {
				t.Fatal("wrong conservative recovery", kind, retained)
			}
		})
	}
}

func TestReplayJournalCompactionAndProcessExit(t *testing.T) {
	const env = "WENDY_REPLAY_JOURNAL_COMPACTION_TEST"
	const phaseEnv = "WENDY_REPLAY_JOURNAL_COMPACTION_PHASE"
	const deliberateExit = 73
	if path := os.Getenv(env); path != "" {
		phase := os.Getenv(phaseEnv)
		if phase != "before_install" && phase != "after_rename" && phase != "after_commit" {
			t.Fatal("invalid journal child phase")
		}
		c, err := OpenReplay(path, 8)
		if err != nil {
			t.Fatal(err)
		}
		c.appendedEntries = journalCompactEntries // force bounded threshold without thousands of fsyncs
		if phase == "before_install" {
			c.syncFile = func(file *os.File) error {
				if err := file.Sync(); err != nil {
					return err
				}
				os.Exit(deliberateExit)
				return nil
			}
		}
		if phase == "after_rename" {
			c.syncDirectory = func(string) error { os.Exit(deliberateExit); return nil }
		}
		if err = c.CheckpointAndConsume(2, "two", "compacted", admissionTestNow.Add(time.Minute), admissionTestNow.Add(time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		os.Exit(deliberateExit)
	}
	for _, phase := range []string{"before_install", "after_rename", "after_commit"} {
		t.Run(phase, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ledger")
			c := openAdmissionCache(t, path, 8)
			if err := c.CheckpointAndConsume(1, "one", "prior", admissionTestNow.Add(time.Minute), admissionTestNow); err != nil {
				t.Fatal(err)
			}
			before, _ := os.Stat(path)
			c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReplayJournalCompactionAndProcessExit$")
			cmd.Env = append(os.Environ(), env+"="+path, phaseEnv+"="+phase)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if ctx.Err() != nil || !errors.As(err, &exit) || exit.ExitCode() != deliberateExit {
				t.Fatalf("child failed: %v %s", err, output)
			}
			after, _ := os.Stat(path)
			if os.SameFile(before, after) != (phase == "before_install") {
				t.Fatal("compaction replaced file at wrong boundary")
			}
			c = openAdmissionCache(t, path, 8)
			keys := []string{"prior"}
			if phase != "before_install" {
				keys = append(keys, "compacted")
			}
			for _, key := range keys {
				if err := c.Consume(key, admissionTestNow.Add(time.Minute), admissionTestNow.Add(time.Millisecond)); !errors.Is(err, ErrReplay) {
					t.Fatal("compaction lost nonce", key, err)
				}
			}
			expectedRevision := uint64(2)
			floor := admissionTestNow.Add(time.Millisecond)
			if phase == "before_install" {
				expectedRevision, floor = 1, admissionTestNow
				if _, ok := c.state.Nonces["compacted"]; ok {
					t.Fatal("uninstalled nonce admitted")
				}
			}
			if c.appendedEntries != 0 || c.state.Revision != expectedRevision {
				t.Fatal("wrong compacted base")
			}
			if err := c.Checkpoint(expectedRevision, c.state.Digest, floor.Add(-time.Millisecond)); err == nil {
				t.Fatal("compaction lost floor")
			}
		})
	}
}

func TestReplayJournalTailRecoverySyncFailureRefusesOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger")
	c := openAdmissionCache(t, path, 8)
	if err := c.Checkpoint(1, "one", admissionTestNow); err != nil {
		t.Fatal(err)
	}
	c.Close()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte{1, 2, 3})
	f.Close()
	probe := NewMemoryReplay(8)
	probe.path = canonicalJournalTestPath(t, path)
	probe.syncFile = func(*os.File) error { return io.ErrUnexpectedEOF }
	defer probe.Close()
	if err = probe.openJournal(); !errors.Is(err, io.ErrUnexpectedEOF) || !probe.poisoned {
		t.Fatal("failed tail sync permitted reuse", err)
	}
}
