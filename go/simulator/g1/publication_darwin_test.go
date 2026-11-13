//go:build darwin && g1_cache_lifecycle

package g1

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/flock"
)

// The runner overlays only the production os.Rename call with this boundary
// observer. Every chmod, source write, flock, rename and cleanup stays real.
func cachePublicationRename(source, target string) error {
	if os.Getenv("WENDY_G1_CACHE_CHILD") != "1" {
		return os.Rename(source, target)
	}
	probe := filepath.Join(source, ".publication-write-probe")
	f, err := os.OpenFile(probe, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("G1_PUBLICATION_ROOT_NOT_WRITABLE: %w", err)
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Remove(probe); err != nil {
		return err
	}
	checked := 0
	err = filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		file, writeErr := os.OpenFile(path, os.O_WRONLY, 0)
		if writeErr == nil {
			file.Close()
			return fmt.Errorf("G1_PAYLOAD_WRITABLE: %s", path)
		}
		if !errors.Is(writeErr, os.ErrPermission) {
			return writeErr
		}
		checked++
		return nil
	})
	if err != nil {
		return err
	}
	if checked == 0 {
		return errors.New("no actual readonly payload checked")
	}
	if err = os.Rename(source, target); err != nil {
		return err
	}
	data, err := json.Marshal(map[string]any{"source": source, "target": target, "readonly_payloads": checked})
	if err != nil {
		return err
	}
	ready, err := os.OpenFile(os.Getenv("WENDY_G1_CACHE_READY"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = ready.Write(data); err != nil {
		ready.Close()
		return err
	}
	if err = ready.Close(); err != nil {
		return err
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(os.Getenv("WENDY_G1_CACHE_PERMIT")); err == nil && string(data) == target {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("publication observer permit deadline")
}

// Pause only an owned, post-rename failure cleanup. The parent checks the
// actual flock again while the failed published tree still exists.
func cachePublicationCleanup(root string) {
	if os.Getenv("WENDY_G1_CACHE_CHILD") == "1" && os.Getenv("WENDY_G1_CACHE_DENY") == "1" && root == os.Getenv("WENDY_G1_CACHE_TARGET") {
		ready := os.Getenv("WENDY_G1_CACHE_READY") + ".cleanup"
		permit := os.Getenv("WENDY_G1_CACHE_PERMIT") + ".cleanup"
		if err := os.WriteFile(ready, []byte(root), 0o600); err != nil {
			fmt.Println("G1_CLEANUP_OBSERVER_ERROR", err)
		}
		deadline := time.Now().Add(8 * time.Second)
		released := false
		for time.Now().Before(deadline) {
			if data, err := os.ReadFile(permit); err == nil && string(data) == root {
				released = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !released {
			fmt.Println("G1_CLEANUP_OBSERVER_ERROR permit deadline")
		}
	}
	removeTemporary(root)
}

var publicationFlat = fstest.MapFS{"source.txt": {Data: []byte("owned source, kernel-denied final protection\n")}}

func TestG1CacheChild(t *testing.T) {
	if os.Getenv("WENDY_G1_CACHE_CHILD") != "1" {
		return
	}
	root := os.Getenv("WENDY_G1_CACHE_ROOT")
	var path string
	var err error
	if os.Getenv("WENDY_G1_CACHE_DENY") == "1" {
		digest, e := digestTree(publicationFlat)
		if e != nil {
			t.Fatal(e)
		}
		path, err = materialize(root, publicationFlat, digest)
	} else {
		path, err = Materialize(root)
	}
	result := map[string]any{"path": path, "error": "", "permission_error": errors.Is(err, os.ErrPermission)}
	if err != nil {
		result["error"] = err.Error()
	}
	data, e := json.Marshal(result)
	if e != nil {
		t.Fatal(e)
	}
	fmt.Println("G1_CHILD_RESULT " + string(data))
}

func TestG1CachePublicationLifecycle(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("requires an ordinary macOS user so kernel permission assertions are meaningful")
	}
	for _, deny := range []bool{false, true} {
		t.Run(fmt.Sprintf("deny-final-protection-%v", deny), func(t *testing.T) {
			scratch, err := os.MkdirTemp("", "wendy-g1-cache-lifecycle-")
			if err != nil {
				t.Fatal(err)
			}
			canClean := true
			t.Cleanup(func() {
				if canClean {
					removeTemporary(scratch)
				} else {
					t.Logf("G1_CACHE_RETAINED_UNKNOWN_DRAIN %s", scratch)
				}
			})
			root, err := filepath.EvalSymlinks(scratch)
			if err != nil {
				t.Fatal(err)
			}
			digest := SourceDigest()
			if deny {
				digest, err = digestTree(publicationFlat)
				if err != nil {
					t.Fatal(err)
				}
			}
			target := filepath.Join(root, "g1", strings.TrimPrefix(digest, "sha256:"))
			foreign := filepath.Join(root, "g1", ".materialize-foreign")
			if err = os.MkdirAll(foreign, 0o700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(foreign, "keep"), []byte("foreign writer"), 0o600); err != nil {
				t.Fatal(err)
			}
			ready, permit := filepath.Join(root, "ready.json"), filepath.Join(root, "permit")
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			args := []string{"-test.run=^TestG1CacheChild$", "-test.v", "-test.timeout=15s"}
			command := binary
			if deny {
				// Per-process sandbox policy; it never changes host permissions, TCC or
				// a user profile. Only the future owned target's chmod is denied.
				profile := fmt.Sprintf("(version 1)(allow default)(deny file-write-mode (literal %q))", target)
				args = append([]string{"-p", profile, binary}, args...)
				command = "/usr/bin/sandbox-exec"
			}
			cmd := exec.CommandContext(ctx, command, args...)
			cmd.WaitDelay = 2 * time.Second
			cmd.Env = append(os.Environ(), "WENDY_G1_CACHE_CHILD=1", "WENDY_G1_CACHE_ROOT="+root,
				"WENDY_G1_CACHE_READY="+ready, "WENDY_G1_CACHE_PERMIT="+permit, "WENDY_G1_CACHE_TARGET="+target, "WENDY_G1_CACHE_DENY=0")
			if deny {
				cmd.Env = append(cmd.Env, "WENDY_G1_CACHE_DENY=1")
			}
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			canClean = false
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			joined := false
			defer func() {
				if !joined {
					cancel()
					select {
					case <-done:
						joined = true
					case <-time.After(3 * time.Second):
						t.Error("G1_CACHE_CHILD_DRAIN_UNKNOWN")
					}
				}
				canClean = joined
			}()
			observed := false
			deadline := time.Now().Add(8 * time.Second)
			for time.Now().Before(deadline) {
				select {
				case err = <-done:
					joined = true
					t.Fatalf("G1_CACHE_PUBLICATION_NOT_OBSERVED: child=%v output=%s", err, output.String())
				default:
				}
				data, e := os.ReadFile(ready)
				if e == nil {
					var event struct {
						Source, Target string
						Readonly       int `json:"readonly_payloads"`
					}
					if json.Unmarshal(data, &event) == nil && event.Target == target && event.Readonly > 0 {
						if _, e = os.Lstat(event.Source); !errors.Is(e, os.ErrNotExist) {
							t.Fatalf("source still exists after real rename: %v", e)
						}
						observed = true
						break
					}
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !observed {
				t.Fatal("G1_CACHE_PUBLICATION_NOT_OBSERVED: timed out before actual rename")
			}
			info, err := os.Stat(target)
			if err != nil || info.Mode().Perm() != 0o700 {
				t.Fatalf("published intermediate root not0700: %v %v", info, err)
			}
			// This process must not acquire the kernel cache lock while the child
			// protects or cleans up its published tree.
			lock, err := os.OpenFile(filepath.Join(root, "g1", ".materialize.lock"), os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			locked, err := flock.TryLock(lock)
			if err != nil {
				t.Fatal(err)
			}
			if locked {
				flock.Unlock(lock)
				t.Fatal("G1_CACHE_LOCK_RELEASED_BEFORE_PROTECTION")
			}
			if err = os.WriteFile(permit, []byte(target), 0o600); err != nil {
				t.Fatal(err)
			}
			cleanupObserved := false
			if deny {
				for !joined && !cleanupObserved {
					select {
					case err = <-done:
						joined = true
					default:
					}
					if data, e := os.ReadFile(ready + ".cleanup"); e == nil && string(data) == target {
						if _, e = os.Stat(target); e != nil {
							t.Fatal("failed publication disappeared before cleanup observation", e)
						}
						locked, e := flock.TryLock(lock)
						if e != nil {
							t.Fatal(e)
						}
						if locked {
							flock.Unlock(lock)
							t.Fatal("G1_CACHE_LOCK_RELEASED_BEFORE_CLEANUP")
						}
						if e = os.WriteFile(permit+".cleanup", []byte(target), 0o600); e != nil {
							t.Fatal(e)
						}
						cleanupObserved = true
					}
					if ctx.Err() != nil {
						t.Fatal("G1_CACHE_CHILD_DRAIN_UNKNOWN: cleanup deadline")
					}
					if !joined && !cleanupObserved {
						time.Sleep(10 * time.Millisecond)
					}
				}
			}
			if !joined {
				select {
				case err = <-done:
					joined = true
				case <-ctx.Done():
					t.Fatal("G1_CACHE_CHILD_DRAIN_UNKNOWN: deadline")
				}
			}
			if err != nil {
				t.Fatalf("cache child failed: %v %s", err, output.String())
			}
			var result struct {
				Path, Error string
				Permission  bool `json:"permission_error"`
			}
			found := false
			scanner := bufio.NewScanner(strings.NewReader(output.String()))
			for scanner.Scan() {
				if line, ok := strings.CutPrefix(scanner.Text(), "G1_CHILD_RESULT "); ok {
					if found || json.Unmarshal([]byte(line), &result) != nil {
						t.Fatal("invalid child receipt")
					}
					found = true
				}
			}
			if !found {
				t.Fatalf("missing child result: %s", output.String())
			}
			if deny {
				if result.Path != "" || !result.Permission || !strings.Contains(result.Error, "protecting published G1 source cache") {
					t.Fatalf("G1_KERNEL_PROTECTION_REFUSAL_MISSING: %+v", result)
				}
				if _, err = os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("G1_FAILED_PUBLICATION_LEAKED: target remains after real chmod refusal: %v", err)
				}
				if !cleanupObserved {
					t.Fatal("G1_CACHE_OWNED_CLEANUP_NOT_OBSERVED")
				}
			} else if result.Error != "" || result.Path != target {
				t.Fatalf("actual materialization failed: %+v", result)
			}
			entries, err := os.ReadDir(filepath.Join(root, "g1"))
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".materialize-") && entry.Name() != ".materialize-foreign" {
					t.Fatal("owned staging cache leaked", entry.Name())
				}
			}
			if data, err := os.ReadFile(filepath.Join(foreign, "keep")); err != nil || string(data) != "foreign writer" {
				t.Fatal("foreign source tree changed", err)
			}
			locked, err = flock.TryLock(lock)
			if err != nil || !locked {
				t.Fatal("cache lock retained after child exit", err)
			}
			flock.Unlock(lock)
			var published string
			if deny {
				published, err = materialize(root, publicationFlat, digest)
			} else {
				published, err = Materialize(root)
			}
			if err != nil || published != target {
				t.Fatal("same cache could not recover/reuse", published, err)
			}
			before, err := os.Stat(target)
			if err != nil || before.Mode().Perm() != 0o555 {
				t.Fatal("G1_PUBLISHED_ROOT_NOT_PROTECTED", err)
			}
			if deny {
				published, err = materialize(root, publicationFlat, digest)
			} else {
				published, err = Materialize(root)
			}
			after, statErr := os.Stat(published)
			if err != nil || statErr != nil || !os.SameFile(before, after) {
				t.Fatal("verified retry cache replaced on reuse", err, statErr)
			}
			t.Logf("G1_CACHE_PUBLICATION_VERIFIED deny=%v real_rename=true real_chmod_refusal=%v lock_exclusion=true reuse_same_inode=true", deny, deny)
		})
	}
}

func TestG1DarwinRenamePermissions(t *testing.T) {
	root := cacheRoot(t)
	for _, cross := range []bool{false, true} {
		source := filepath.Join(root, fmt.Sprintf("source-%v", cross))
		if err := os.Mkdir(source, 0o555); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, fmt.Sprintf("target-%v", cross))
		if cross {
			parent := filepath.Join(root, "other-parent")
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			target = filepath.Join(parent, "target")
		}
		err := os.Rename(source, target)
		if err != nil && !errors.Is(err, os.ErrPermission) {
			t.Fatal(err)
		}
		t.Logf("G1_DARWIN_RENAME cross_parent=%v source_mode=0555 permission_denied=%v", cross, errors.Is(err, os.ErrPermission))
		// This reports native behavior without pretending cross-parent failure is
		// the same-parent production path or making an OS-dependent skip pass.
	}
}
