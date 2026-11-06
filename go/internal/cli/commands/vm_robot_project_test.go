package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

func sceneFixture(t *testing.T, mesh string) string {
	t.Helper()
	folder := t.TempDir()
	text := fmt.Sprintf(`{"version":1,"profile":"go2","geoms":[{"mesh":%q}]}`, mesh)
	if err := os.WriteFile(filepath.Join(folder, "scene.json"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return folder
}

func TestSceneSnapshotTracksOnlyReferencedFilesAndRejectsUnsafeAssets(t *testing.T) {
	folder := sceneFixture(t, "assets/crate.obj")
	if err := os.Mkdir(filepath.Join(folder, "assets"), 0700); err != nil {
		t.Fatal(err)
	}
	mesh := filepath.Join(folder, "assets/crate.obj")
	if err := os.WriteFile(mesh, []byte("mesh-one"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := readSceneSnapshot(folder)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "unrelated.txt"), []byte("ignored"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := readSceneSnapshot(folder)
	if err != nil || first.Revision != second.Revision {
		t.Fatalf("unrelated file affected snapshot: %v", err)
	}
	if err := os.WriteFile(mesh, []byte("mesh-two"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err = readSceneSnapshot(folder)
	if err != nil || first.Revision == second.Revision {
		t.Fatalf("asset edit not tracked: %v", err)
	}
	if err := os.Remove(mesh); err != nil {
		t.Fatal(err)
	}
	if _, err = readSceneSnapshot(folder); err == nil {
		t.Fatal("accepted missing mesh")
	}
	outside := filepath.Join(t.TempDir(), "secret.obj")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, mesh); err != nil {
		t.Fatal(err)
	}
	if _, err = readSceneSnapshot(folder); err == nil {
		t.Fatal("accepted symlink asset")
	}
	if err := os.Remove(mesh); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mesh, make([]byte, sceneMaxFile+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = readSceneSnapshot(folder); err == nil {
		t.Fatal("accepted oversized asset")
	}
	for _, name := range []string{"../secret.obj", "assets/../../secret.obj", "/tmp/secret.obj", "assets//crate.obj", `assets\crate.obj`} {
		if _, err := readSceneSnapshot(sceneFixture(t, name)); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
}

func TestSceneSnapshotRejectsSymlinkDirectoryAndRoot(t *testing.T) {
	folder := sceneFixture(t, "assets/crate.obj")
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "crate.obj"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(folder, "assets")); err != nil {
		t.Fatal(err)
	}
	if _, err := readSceneSnapshot(folder); err == nil {
		t.Fatal("accepted symlink directory")
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(folder, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := readSceneSnapshot(alias); err == nil {
		t.Fatal("accepted symlink project root")
	}
}

func TestSceneProjectCommandsRequireExplicitVM(t *testing.T) {
	cmd := newVMRobotProjectCmd()
	cmd.SetArgs([]string{"link", t.TempDir()})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "vm") {
		t.Fatalf("link accepted missing explicit VM: %v", err)
	}
}

func TestSceneRequestDoesNotFollowRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://foreign.invalid", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port
	if _, err := sceneRequest(context.Background(), port, nil); err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("redirect accepted: %v", err)
	}
}

func TestSceneRequestReportsServerErrorText(t *testing.T) {
	const reason = "project rejected; previous scene retained: crate: sizes must be positive"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": reason})
	}))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port
	_, err := sceneRequest(context.Background(), port, map[string]any{})
	var rejected *sceneProjectHTTPError
	if !errors.As(err, &rejected) || rejected.Code != http.StatusBadRequest || rejected.Message != reason {
		t.Fatalf("rejection did not carry the server's reason: %v", err)
	}
}

func TestSceneWatchDebouncesEditsAndRetainsWorldOnPartialWrite(t *testing.T) {
	folder := sceneFixture(t, "")
	var mu sync.Mutex
	var revision string
	var snapshots []map[string][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			var body struct {
				Files map[string][]byte `json:"files"`
				Base  string            `json:"base_revision"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				http.Error(w, "bad", 400)
				return
			}
			if body.Base != revision {
				t.Errorf("CAS base %q differs from %q", body.Base, revision)
			}
			if body.Files != nil {
				snapshots = append(snapshots, body.Files)
				revision = fmt.Sprintf("revision-%d", len(snapshots))
			}
		}
		_ = json.NewEncoder(w).Encode(sceneProjectStatus{ID: strings.Repeat("a", 32), Revision: revision})
	}))
	defer server.Close()
	old := resolveSceneProjectEndpoint
	defer func() { resolveSceneProjectEndpoint = old }()
	resolveSceneProjectEndpoint = func(context.Context, sceneLink) (int, vm.RobotProfile, error) {
		return server.Listener.Addr().(*net.TCPAddr).Port, vm.RobotProfile{SourceDigest: "pin"}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	done := make(chan error, 1)
	go func() {
		done <- watchScene(ctx, cmd, folder, sceneLink{VM: "test", Profile: "go2", ID: strings.Repeat("a", 32)})
	}()
	waitFor := func(count int) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			n := len(snapshots)
			mu.Unlock()
			if n >= count {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("watch did not sync %d snapshots", count)
	}
	waitFor(1)
	scene := filepath.Join(folder, "scene.json")
	if err := os.WriteFile(scene, []byte(`{"version":`), 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(750 * time.Millisecond)
	mu.Lock()
	count := len(snapshots)
	mu.Unlock()
	if count != 1 {
		t.Fatal("partial write uploaded")
	}
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(scene, []byte(fmt.Sprintf(`{"version":1,"profile":"go2","geoms":[],"edit":%d}`, i)), 0600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(60 * time.Millisecond)
	}
	waitFor(2)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("watch did not cancel")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(snapshots) != 2 || !strings.Contains(string(snapshots[1]["scene.json"]), `"edit":2`) {
		t.Fatalf("rapid edits were not coalesced: %v", snapshots)
	}
}

func TestSceneWatchRetriesDisconnectWithoutResettingAcknowledgedContentTwice(t *testing.T) {
	for _, lostAck := range []bool{false, true} {
		t.Run(fmt.Sprintf("lost_ack_%t", lostAck), func(t *testing.T) {
			folder := sceneFixture(t, "")
			snapshot, err := readSceneSnapshot(folder)
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var revision string
			attempts, commits := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Method == http.MethodPost {
					var body struct {
						Files map[string][]byte `json:"files"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body.Files != nil {
						attempts++
						if attempts == 1 {
							if lostAck {
								revision = snapshot.Revision
								commits++
							}
							http.Error(w, "transient disconnect", 503)
							return
						}
						if revision != snapshot.Revision {
							revision = snapshot.Revision
							commits++
						}
					}
				}
				_ = json.NewEncoder(w).Encode(sceneProjectStatus{ID: strings.Repeat("a", 32), Revision: revision})
			}))
			defer server.Close()
			old := resolveSceneProjectEndpoint
			defer func() { resolveSceneProjectEndpoint = old }()
			resolveSceneProjectEndpoint = func(context.Context, sceneLink) (int, vm.RobotProfile, error) {
				return server.Listener.Addr().(*net.TCPAddr).Port, vm.RobotProfile{SourceDigest: "pin"}, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cmd := &cobra.Command{}
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			done := make(chan error, 1)
			go func() {
				done <- watchScene(ctx, cmd, folder, sceneLink{VM: "test", Profile: "go2", ID: strings.Repeat("a", 32)})
			}()
			deadline := time.Now().Add(8 * time.Second)
			for time.Now().Before(deadline) {
				mu.Lock()
				ready := attempts >= 2
				mu.Unlock()
				if ready {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("watch did not cancel")
			}
			mu.Lock()
			defer mu.Unlock()
			if attempts != 2 || commits != 1 {
				t.Fatalf("attempts=%d commits=%d; expected retry and exactly one commit", attempts, commits)
			}
		})
	}
}

func TestSceneWatchReappliesProjectAfterRuntimeRestartBetweenHeartbeats(t *testing.T) {
	folder := sceneFixture(t, "")
	var mu sync.Mutex
	var id, revision string
	commits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			var body struct {
				ID    string            `json:"project_id"`
				Base  string            `json:"base_revision"`
				Files map[string][]byte `json:"files"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.Base != revision {
				http.Error(w, `{"error": "project revision changed"}`, http.StatusBadRequest)
				return
			}
			if body.Files != nil {
				id = body.ID
				revision = fmt.Sprintf("revision-%d", commits)
				commits++
			}
		}
		_ = json.NewEncoder(w).Encode(sceneProjectStatus{ID: id, Revision: revision})
	}))
	defer server.Close()
	old := resolveSceneProjectEndpoint
	defer func() { resolveSceneProjectEndpoint = old }()
	resolveSceneProjectEndpoint = func(context.Context, sceneLink) (int, vm.RobotProfile, error) {
		return server.Listener.Addr().(*net.TCPAddr).Port, vm.RobotProfile{SourceDigest: "pin"}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	done := make(chan error, 1)
	go func() {
		done <- watchScene(ctx, cmd, folder, sceneLink{VM: "test", Profile: "go2", ID: strings.Repeat("a", 32)})
	}()
	waitFor := func(count int) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			n := commits
			mu.Unlock()
			if n >= count {
				return
			}
			select {
			case err := <-done:
				t.Fatalf("watch stopped instead of reapplying the project: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
		}
		t.Fatalf("watch did not commit %d snapshots", count)
	}
	waitFor(1)
	// The runtime restarted between heartbeats: no request failed, but it now
	// serves the managed default world without a project.
	mu.Lock()
	id, revision = "", ""
	mu.Unlock()
	waitFor(2)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
