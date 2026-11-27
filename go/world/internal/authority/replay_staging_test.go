package authority

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestReplayStagingRecoversInitializationAndOnlyOwnedPartialNext(t *testing.T) {
	for _, phase := range []string{"empty_directory", "marker_only", "empty_next", "partial_next"} {
		t.Run(phase, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ledger")
			stage := path + ReplayStagingSuffix
			if phase == "empty_directory" {
				if err := os.Mkdir(stage, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				c := openAdmissionCache(t, path, 8)
				stage = c.staging
				c.Close()
				if phase != "marker_only" {
					var partial []byte
					if phase == "partial_next" {
						partial = []byte(journalMagic + "partial")
					}
					if err := os.WriteFile(filepath.Join(stage, "next"), partial, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			c := openAdmissionCache(t, path, 8)
			entries, err := os.ReadDir(stage)
			if err != nil || len(entries) != 1 || !entries[0].IsDir() {
				t.Fatal("staging not bounded after recovery", entries, err)
			}
			if err = c.Checkpoint(1, "one", admissionTestNow); err != nil {
				t.Fatal(err)
			}
			c.Close()
			entries, err = os.ReadDir(stage)
			if err != nil || len(entries) != 1 {
				t.Fatal("installed staging not cleared", entries, err)
			}
		})
	}
}

func TestReplayStagingRefusesUnknownOwnershipAndPreservesFiles(t *testing.T) {
	for _, kind := range []string{"unowned_next", "unknown_owned_entry", "next_symlink", "next_hardlink", "marker_payload", "stage_symlink", "stage_public_mode"} {
		t.Run(kind, func(t *testing.T) {
			folder := t.TempDir()
			path := filepath.Join(folder, "ledger")
			stage := path + ReplayStagingSuffix
			sentinel := filepath.Join(folder, "unrelated")
			private := []byte("must remain unchanged")
			if err := os.WriteFile(sentinel, private, 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "stage_symlink" {
				if err := os.Symlink(folder, stage); err != nil {
					t.Fatal(err)
				}
			} else if kind == "unowned_next" {
				os.Mkdir(stage, 0700)
				os.WriteFile(filepath.Join(stage, "next"), private, 0600)
			} else {
				c := openAdmissionCache(t, path, 8)
				stage = c.staging
				c.Close()
				switch kind {
				case "unknown_owned_entry":
					os.WriteFile(filepath.Join(stage, "unknown"), private, 0600)
				case "next_symlink":
					if err := os.Symlink(sentinel, filepath.Join(stage, "next")); err != nil {
						t.Fatal(err)
					}
				case "next_hardlink":
					if err := os.Link(sentinel, filepath.Join(stage, "next")); err != nil {
						t.Fatal(err)
					}
				case "marker_payload":
					entries, _ := os.ReadDir(stage)
					os.WriteFile(filepath.Join(stage, entries[0].Name(), "unknown"), private, 0600)
				case "stage_public_mode":
					if err := os.Chmod(stage, 0755); err != nil {
						t.Fatal(err)
					}
				}
			}
			if c, err := OpenReplay(path, 8); err == nil {
				c.Close()
				t.Fatal("unsafe staging accepted")
			}
			data, err := os.ReadFile(sentinel)
			if err != nil || !bytes.Equal(data, private) {
				t.Fatal("unrelated file modified", err)
			}
			if kind == "unowned_next" || kind == "next_hardlink" || kind == "next_symlink" {
				if _, err := os.Lstat(filepath.Join(stage, "next")); err != nil {
					t.Fatal("unknown next removed", err)
				}
			}
		})
	}
}

func TestReplayStagingCanonicalParentAliasesShareOwner(t *testing.T) {
	folder := t.TempDir()
	real := filepath.Join(folder, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(folder, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	c := openAdmissionCache(t, filepath.Join(alias, "ledger"), 8)
	stage := c.staging
	if err := c.Checkpoint(1, "one", admissionTestNow); err != nil {
		t.Fatal(err)
	}
	c.Close()
	c = openAdmissionCache(t, filepath.Join(real, "ledger"), 8)
	if c.staging != stage {
		t.Fatal("alias changed staging ownership")
	}
}
