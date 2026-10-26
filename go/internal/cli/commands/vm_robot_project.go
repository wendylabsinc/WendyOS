package commands

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

const sceneLinkFile = ".wendy-simulation.json"
const sceneMaxFile = 2 << 20
const sceneMaxTotal = 8 << 20

type sceneLink struct {
	Version int    `json:"version"`
	VM      string `json:"vm"`
	Profile string `json:"profile"`
	ID      string `json:"project_id"`
}
type sceneSnapshot struct {
	Files    map[string][]byte `json:"files"` // encoding/json encodes bytes as base64.
	Revision string            `json:"-"`
}
type sceneProjectStatus struct {
	ID       string `json:"project_id"`
	Revision string `json:"revision"`
	Error    string `json:"error"`
	Changed  bool   `json:"changed"`
}

var sceneAssetPath = regexp.MustCompile(`^assets/[A-Za-z0-9_./-]+\.obj$`)

// Read through os.Root so even a concurrent rename/symlink cannot escape the
// project. Lstat rejects in-root symlinks too; before/after metadata detects
// ordinary editor partial writes. Watch also requires two identical snapshots.
func readSceneFile(root *os.Root, name string) ([]byte, error) {
	if !filepath.IsLocal(name) || strings.Contains(name, "\\") {
		return nil, fmt.Errorf("invalid project path %q", name)
	}
	for parent := name; parent != "."; parent = path.Dir(parent) {
		info, err := root.Lstat(parent)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("project symlinks are unsupported: %s", parent)
		}
	}
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > sceneMaxFile {
		return nil, fmt.Errorf("%s must be a regular file of at most 2 MiB", name)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, opened) {
		return nil, fmt.Errorf("%s changed while reading; retry", name)
	}
	content, err := io.ReadAll(io.LimitReader(f, sceneMaxFile+1))
	if err != nil {
		return nil, err
	}
	after, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if len(content) > sceneMaxFile || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, fmt.Errorf("%s changed while reading; retry", name)
	}
	return content, nil
}

func openSceneRoot(folder string) (*os.Root, error) {
	info, err := os.Lstat(folder)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("project folder must be a real directory")
	}
	return os.OpenRoot(folder)
}

func readSceneSnapshot(folder string) (sceneSnapshot, error) {
	result := sceneSnapshot{Files: map[string][]byte{}}
	root, err := openSceneRoot(folder)
	if err != nil {
		return result, err
	}
	defer root.Close()
	scene, err := readSceneFile(root, "scene.json")
	if err != nil {
		return result, err
	}
	var manifest struct {
		Geoms []struct {
			Mesh string `json:"mesh"`
		} `json:"geoms"`
	}
	if err := json.Unmarshal(scene, &manifest); err != nil {
		return result, fmt.Errorf("scene.json: %w", err)
	}
	if len(manifest.Geoms) > 64 {
		return result, fmt.Errorf("scene exceeds 64 surfaces")
	}
	result.Files["scene.json"] = scene
	total := len(scene)
	for _, geom := range manifest.Geoms {
		name := geom.Mesh
		if name == "" {
			continue
		}
		if !sceneAssetPath.MatchString(name) || path.Clean(name) != name {
			return result, fmt.Errorf("mesh %q must be a relative assets/*.obj path without traversal", name)
		}
		if _, exists := result.Files[name]; exists {
			continue
		}
		if len(result.Files) >= 9 {
			return result, fmt.Errorf("scene exceeds eight assets")
		}
		content, err := readSceneFile(root, name)
		if err != nil {
			return result, fmt.Errorf("%s: %w", name, err)
		}
		total += len(content)
		if total > sceneMaxTotal {
			return result, fmt.Errorf("scene exceeds 8 MiB")
		}
		result.Files[name] = content
	}
	names := make([]string, 0, len(result.Files))
	for name := range result.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	digest := sha256.New()
	for _, name := range names {
		digest.Write([]byte(name))
		digest.Write([]byte{0})
		digest.Write(result.Files[name])
		digest.Write([]byte{0})
	}
	result.Revision = hex.EncodeToString(digest.Sum(nil))
	return result, nil
}

func readSceneLink(folder string) (sceneLink, error) {
	var link sceneLink
	root, err := openSceneRoot(folder)
	if err != nil {
		return link, err
	}
	defer root.Close()
	content, err := readSceneFile(root, sceneLinkFile)
	if err != nil {
		return link, fmt.Errorf("link project first with 'wendy vm robot project link <folder> --vm <name>': %w", err)
	}
	if err := json.Unmarshal(content, &link); err != nil {
		return link, err
	}
	if link.Version != 1 || (link.Profile != "go2" && link.Profile != "g1") || !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(link.ID) {
		return link, fmt.Errorf("invalid simulation link")
	}
	return link, vm.ValidName(link.VM)
}

// Every operation resolves the current forward and verifies the full managed
// identity. Never boot, update, deploy or fall back to a default/physical device.
var resolveSceneProjectEndpoint = sceneEndpoint

func sceneEndpoint(ctx context.Context, link sceneLink) (int, vm.RobotProfile, error) {
	_, profile, port, err := robotEndpoint(ctx, link.VM)
	if err != nil {
		return 0, profile, err
	}
	if profile.Kind != link.Profile {
		return 0, profile, fmt.Errorf("project profile differs from VM profile")
	}
	runtime, err := robotRuntimeForKind(profile.Kind)
	if err != nil {
		return 0, profile, err
	}
	if err := runtime.validateSource(link.VM, profile); err != nil {
		return 0, profile, err
	}
	status, err := readRobotStatus(ctx, port)
	if err != nil {
		return 0, profile, err
	}
	if err := status.matches(link.VM, profile); err != nil {
		return 0, profile, err
	}
	return port, profile, nil
}

var errSceneVMLinkedElsewhere = errors.New("VM already has another linked project; use its folder or restart the managed runtime to relink")

type sceneProjectHTTPError struct {
	Code    int
	Message string
}

func (e *sceneProjectHTTPError) Error() string {
	return fmt.Sprintf("project HTTP %d: %s", e.Code, e.Message)
}

var sceneHTTPClient = &http.Client{Timeout: 180 * time.Second, CheckRedirect: robotHTTPClient.CheckRedirect, Transport: &http.Transport{Proxy: nil}}

func sceneRequest(ctx context.Context, port int, body any) (sceneProjectStatus, error) {
	var status sceneProjectStatus
	method := http.MethodGet
	var data []byte
	var err error
	if body != nil {
		method = http.MethodPost
		data, err = json.Marshal(body)
		if err != nil {
			return status, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, robotURL(port)+"/api/project", bytes.NewReader(data))
	if err != nil {
		return status, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := sceneHTTPClient.Do(req)
	if err != nil {
		return status, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return status, err
	}
	if resp.StatusCode != 200 {
		message := strings.TrimSpace(string(payload))
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(payload, &failure) == nil && failure.Error != "" {
			message = failure.Error
		}
		return status, &sceneProjectHTTPError{Code: resp.StatusCode, Message: message}
	}
	if err := json.Unmarshal(payload, &status); err != nil {
		return status, err
	}
	return status, nil
}

func sceneEnvelope(link sceneLink, profile vm.RobotProfile, base string) map[string]any {
	return map[string]any{"project_id": link.ID, "vm_name": link.VM, "source_digest": profile.SourceDigest, "base_revision": base}
}

func newVMRobotProjectCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "project", Short: "Link and watch local Go2/G1 environment files"}
	for _, action := range []string{"init", "link", "sync", "watch"} {
		sub := &cobra.Command{Use: action + " <folder>", Args: cobra.ExactArgs(1), Short: map[string]string{
			"init": "Create a scene project and link it to an explicit running VM", "link": "Link an existing scene project to an explicit running VM", "sync": "Validate and apply the linked project's current snapshot", "watch": "Watch and sync stable edits; Ctrl+C disconnects without stopping the VM",
		}[action]}
		if action == "init" || action == "link" {
			sub.Flags().String("vm", "", "Explicit Go2/G1 VM name (required)")
			_ = sub.MarkFlagRequired("vm")
		}
		sub.RunE = func(c *cobra.Command, args []string) error { return runVMRobotProject(c, action, args[0]) }
		cmd.AddCommand(sub)
	}
	return cmd
}

func runVMRobotProject(cmd *cobra.Command, action, folder string) error {
	ctx := cmd.Context()
	if action == "init" || action == "link" {
		name, _ := cmd.Flags().GetString("vm")
		if err := vm.ValidName(name); err != nil {
			return err
		}
		_, profile, _, err := robotEndpoint(ctx, name)
		if err != nil {
			return err
		}
		if profile.Kind != "go2" && profile.Kind != "g1" {
			return fmt.Errorf("live projects support only Go2/G1 MuJoCo environments")
		}
		link := sceneLink{Version: 1, VM: name, Profile: profile.Kind}
		port, _, err := resolveSceneProjectEndpoint(ctx, link)
		if err != nil {
			return err
		}
		live, err := sceneRequest(ctx, port, nil)
		if err != nil {
			return err
		}
		if action == "init" {
			// A new folder gets a new identity; refuse before creating it so a
			// retry after restarting the runtime is not blocked by the folder.
			if live.ID != "" {
				return errSceneVMLinkedElsewhere
			}
			if err := os.Mkdir(folder, 0755); err != nil {
				return fmt.Errorf("init requires a new folder: %w", err)
			}
			scene := fmt.Sprintf("{\n  \"version\": 1,\n  \"profile\": %q,\n  \"geoms\": [{\"name\": \"crate\", \"type\": \"box\", \"size\": [0.4, 0.5, 0.4], \"position\": [3, -2, 0.4], \"rgba\": [0.3, 0.6, 0.8, 1]}]\n}\n", profile.Kind)
			createdRoot, err := openSceneRoot(folder)
			if err != nil {
				return err
			}
			f, err := createdRoot.OpenFile("scene.json", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
			if err != nil {
				createdRoot.Close()
				return err
			}
			_, writeErr := f.Write([]byte(scene))
			closeErr := f.Close()
			createdRoot.Close()
			if writeErr != nil {
				return writeErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
		if _, err := readSceneSnapshot(folder); err != nil {
			return err
		}
		root, err := openSceneRoot(folder)
		if err != nil {
			return err
		}
		defer root.Close()
		// Repeated link to the same VM keeps the stable project identity.
		if old, err := readSceneLink(folder); err == nil {
			if old.VM != name || old.Profile != profile.Kind {
				return fmt.Errorf("folder is already linked to VM %s; remove %s to link it elsewhere", old.VM, sceneLinkFile)
			}
			link = old
		} else {
			if _, statErr := root.Lstat(sceneLinkFile); !os.IsNotExist(statErr) {
				return fmt.Errorf("existing link is invalid; fix %s", sceneLinkFile)
			}
			id := make([]byte, 16)
			if _, err := rand.Read(id); err != nil {
				return err
			}
			link.ID = hex.EncodeToString(id)
		}
		if live.ID != "" && live.ID != link.ID {
			return errSceneVMLinkedElsewhere
		}
		data, _ := json.MarshalIndent(link, "", "  ")
		if _, err := root.Lstat(sceneLinkFile); os.IsNotExist(err) {
			f, err := root.OpenFile(sceneLinkFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				return err
			}
			_, writeErr := f.Write(append(data, '\n'))
			closeErr := f.Close()
			if writeErr != nil {
				return writeErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Linked %s to vm:%s. Run 'wendy vm robot project watch %s'.\n", folder, name, folder)
		return nil
	}
	link, err := readSceneLink(folder)
	if err != nil {
		return err
	}
	if action == "sync" {
		return syncSceneOnce(ctx, cmd, folder, link)
	}
	return watchScene(ctx, cmd, folder, link)
}

func syncSceneOnce(ctx context.Context, cmd *cobra.Command, folder string, link sceneLink) error {
	first, err := readSceneSnapshot(folder)
	if err != nil {
		return err
	}
	timer := time.NewTimer(350 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}
	snapshot, err := readSceneSnapshot(folder)
	if err != nil {
		return err
	}
	if first.Revision != snapshot.Revision {
		return fmt.Errorf("project is changing; wait for writes to finish or use watch")
	}
	port, profile, err := resolveSceneProjectEndpoint(ctx, link)
	if err != nil {
		return err
	}
	live, err := sceneRequest(ctx, port, nil)
	if err != nil {
		return err
	}
	body := sceneEnvelope(link, profile, live.Revision)
	body["files"] = snapshot.Files
	applied, err := sceneRequest(ctx, port, body)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Synced %s to vm:%s (revision %.8s). Changed scenes reset and pause physics; resume and rearm in %s.\n", folder, link.VM, applied.Revision, robotURL(port))
	return nil
}

// Bounded polling watches only scene.json and referenced assets, including
// atomic rename saves. No recursive watch handles, event queue or upload queue.
func watchScene(ctx context.Context, cmd *cobra.Command, folder string, link sceneLink) error {
	ticker := time.NewTicker(350 * time.Millisecond)
	defer ticker.Stop()
	var stable, accepted, attempted, base, lastError string
	var connected bool
	var lastContact time.Time
	fmt.Fprintf(cmd.OutOrStdout(), "Watching %s for vm:%s; successful edits reset and pause physics.\n", folder, link.VM)
	report := func(err error) {
		if err.Error() != lastError {
			fmt.Fprintln(cmd.ErrOrStderr(), err)
			lastError = err.Error()
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		if !connected && !lastContact.IsZero() && time.Since(lastContact) < 5*time.Second {
			continue
		}
		snapshot, localErr := readSceneSnapshot(folder)
		if localErr == nil && snapshot.Revision != stable {
			stable = snapshot.Revision
			continue
		}
		if localErr != nil {
			stable = ""
			report(localErr)
		}
		due := time.Since(lastContact) >= 5*time.Second
		if connected && localErr == nil && (snapshot.Revision == accepted || snapshot.Revision == attempted) && !due {
			continue
		}
		if connected && localErr != nil && !due {
			continue
		}
		port, profile, err := resolveSceneProjectEndpoint(ctx, link)
		if err != nil {
			connected = false
			lastContact = time.Now()
			report(err)
			continue
		}
		live, err := sceneRequest(ctx, port, nil)
		if err != nil {
			connected = false
			lastContact = time.Now()
			report(err)
			continue
		}
		if live.ID != "" && live.ID != link.ID {
			return fmt.Errorf("VM is now linked to another project; watcher stopped")
		}
		if connected && base != "" && live.ID == "" {
			// A runtime restarted between heartbeats serves the default world
			// without a project. Reapply this folder as after a lost connection.
			connected = false
		}
		if connected && live.Revision != base && live.Revision == snapshot.Revision && localErr == nil {
			base = live.Revision
			accepted = snapshot.Revision
		}
		if connected && live.Revision != base {
			return fmt.Errorf("VM project revision changed outside this watcher; stopped to avoid overwriting it")
		}
		if !connected {
			base = live.Revision
			accepted = ""
			attempted = ""
		}
		body := sceneEnvelope(link, profile, base)
		if localErr != nil {
			body["error"] = localErr.Error()
		} else if snapshot.Revision == accepted || snapshot.Revision == attempted {
			body["heartbeat"] = true
			body["clear_error"] = snapshot.Revision == accepted
		} else {
			body["files"] = snapshot.Files
			attempted = snapshot.Revision
		}
		result, err := sceneRequest(ctx, port, body)
		lastContact = time.Now()
		if err != nil {
			report(err)
			var rejected *sceneProjectHTTPError
			connected = errors.As(err, &rejected) && rejected.Code == http.StatusBadRequest
			continue
		}
		connected = true
		base = result.Revision
		if localErr == nil && body["files"] != nil {
			accepted = snapshot.Revision
			lastError = ""
			state := "unchanged"
			if result.Changed {
				state = "reset and paused"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Synced %.8s · %s · %s\n", base, state, robotURL(port))
		}
	}
}
