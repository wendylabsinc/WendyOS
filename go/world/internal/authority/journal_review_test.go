package authority

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func journalReviewBase(t *testing.T) ([]byte, uint64, string, time.Time) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "replay")
	c, err := OpenReplay(path, 8)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	if err = c.CheckpointAndConsume(1, "review-revision", "acknowledged-nonce", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	sequence, previous := c.sequence, hex.EncodeToString(c.previous[:])
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data, sequence, previous, now
}

// Recompute the checksums around hostile transitions so corruption detection
// alone cannot make these pass. The reader must also enforce ledger semantics.
func TestJournalReviewRejectsChecksummedInvalidTransitions(t *testing.T) {
	base, sequence, previous, now := journalReviewBase(t)
	changedDigest := "different-revision"
	cases := map[string]func(*journalFrame){
		"repeated-sequence":          func(f *journalFrame) { f.Sequence = sequence },
		"skipped-sequence":           func(f *journalFrame) { f.Sequence = sequence + 2 },
		"wrong-prefix-hash":          func(f *journalFrame) { f.Previous = hex.EncodeToString(make([]byte, 32)) },
		"clock-regression":           func(f *journalFrame) { f.LastUnixMS = now.Add(-time.Millisecond).UnixMilli() },
		"revision-regression":        func(f *journalFrame) { f.Revision = 0 },
		"same-revision-equivocation": func(f *journalFrame) { f.Digest = &changedDigest },
		"nonce-overwrite": func(f *journalFrame) {
			f.Nonce = &journalNonce{Key: "acknowledged-nonce", Expires: now.Add(2 * time.Minute).UnixMilli()}
		},
		"expired-new-nonce": func(f *journalFrame) { f.Nonce = &journalNonce{Key: "new", Expires: now.UnixMilli()} },
		"empty-nonce":       func(f *journalFrame) { f.Nonce = &journalNonce{Key: "", Expires: now.Add(time.Minute).UnixMilli()} },
		"base-inside-log": func(f *journalFrame) {
			f.Kind = "base"
			f.State = &replayState{Version: Version, Nonces: map[string]int64{}}
		},
		"unexpected-state": func(f *journalFrame) { f.State = &replayState{Version: Version, Nonces: map[string]int64{}} },
		"unknown-version":  func(f *journalFrame) { f.Version++ },
	}
	for name, alter := range cases {
		t.Run(name, func(t *testing.T) {
			frame := journalFrame{Version: journalVersion, Kind: "mutation", Sequence: sequence + 1, Previous: previous, LastUnixMS: now.Add(time.Millisecond).UnixMilli(), Revision: 1}
			alter(&frame)
			encoded, err := encodeJournalFrame(frame)
			if err != nil {
				t.Fatal(err)
			}
			data := append(append([]byte(nil), base...), encoded...)
			path := filepath.Join(t.TempDir(), "replay")
			if err = os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if c, err := OpenReplay(path, 8); err == nil {
				c.Close()
				t.Fatal("checksummed invalid transition accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, data) {
				t.Fatal("semantic corruption was silently truncated", err)
			}
			// Failed open must release its descriptor and process lock. Restoring the
			// known-good fixture is a test operation, not a production recovery policy.
			if err = os.WriteFile(path, base, 0600); err != nil {
				t.Fatal(err)
			}
			c, err := OpenReplay(path, 8)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if err = c.Consume("acknowledged-nonce", now.Add(time.Minute), now); !errors.Is(err, ErrReplay) {
				t.Fatal("prior acknowledged nonce lost", err)
			}
		})
	}
}

func TestJournalReviewCompactionRebindsAppendDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replay")
	c, err := OpenReplay(path, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	admit := func(key string) {
		t.Helper()
		if err := c.CheckpointAndConsume(1, "review-revision", key, now.Add(time.Minute), now); err != nil {
			t.Fatal(err)
		}
	}
	admit("one")
	now = now.Add(time.Millisecond)
	admit("two")
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	oldBytes, err := io.ReadAll(old)
	if err != nil {
		t.Fatal(err)
	}
	oldInfo, err := old.Stat()
	if err != nil {
		t.Fatal(err)
	}
	// Force the normal bounded compaction branch without thousands of fsyncs.
	c.appendedEntries = journalCompactEntries
	now = now.Add(time.Millisecond)
	admit("three")
	newInfo, err := c.file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(oldInfo, newInfo) || c.appendedEntries != 0 {
		t.Fatal("compaction retained old append descriptor")
	}
	now = now.Add(time.Millisecond)
	admit("four")
	if _, err = old.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	unchanged, err := io.ReadAll(old)
	if err != nil || !bytes.Equal(oldBytes, unchanged) {
		t.Fatal("post-compaction append wrote unlinked old file", err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = OpenReplay(path, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, key := range []string{"one", "two", "three", "four"} {
		if err = c.Consume(key, now.Add(time.Minute), now); !errors.Is(err, ErrReplay) {
			t.Fatalf("acknowledged nonce %s lost after compact/append/reopen: %v", key, err)
		}
	}
	if err = c.ValidateCheckpoint(1, "review-revision", now.Add(-time.Millisecond)); err == nil {
		t.Fatal("post-compaction acknowledged floor lost")
	}
}
