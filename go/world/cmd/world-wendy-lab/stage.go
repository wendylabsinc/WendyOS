//go:build !world_wendy_boot

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"github.com/wendylabsinc/wendy/go/world/deploy/wendy-sim"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const nodeAudience = "wendy-sim-node"

var runIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

type stageOptions struct {
	Out, Repo, RunID, Arch, Transport string
	Port                              int
	AllowInsecureSimulation           bool
	Now                               func() time.Time
}
type stageManifest struct {
	Schema          string            `json:"schema"`
	Directory       string            `json:"directory"`
	AppID           string            `json:"app_id"`
	VolumeName      string            `json:"volume_name"`
	RunID           string            `json:"run_id"`
	Tenant          string            `json:"tenant"`
	Site            string            `json:"site"`
	Audience        string            `json:"audience"`
	Frame           world.FrameRef    `json:"frame"`
	PublisherSource world.Source      `json:"publisher_source"`
	Architecture    string            `json:"architecture"`
	Transport       string            `json:"transport"`
	GuestPort       int               `json:"guest_port"`
	IssuedAtUnixMS  int64             `json:"issued_at_unix_ms"`
	ExpiresAtUnixMS int64             `json:"expires_at_unix_ms"`
	Artifacts       map[string]string `json:"artifact_sha256"`
	Limits          []string          `json:"limits"`
}
type builder func(repo, output, arch, tag, target string) error

func buildBinary(repo, output, arch, tag, target string) error {
	args := []string{"build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o", output}
	if tag != "" {
		args = append(args, "-tags", tag)
	}
	args = append(args, target)
	cmd := exec.Command("go", args...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("build %s: %w: %s", target, err, output)
	}
	return os.Chmod(output, 0755)
}

func writeNew(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	return err
}
func writeJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeNew(path, append(b, '\n'), 0600)
}

func stage(opts stageOptions, build builder) (manifest stageManifest, err error) {
	if opts.Out == "" || !runIDPattern.MatchString(opts.RunID) || opts.Port < 1024 || opts.Port > 65535 || (opts.Arch != "arm64" && opts.Arch != "amd64") || opts.Transport != "host-http" || !opts.AllowInsecureSimulation || opts.Now == nil || build == nil {
		return manifest, errors.New("new output, bounded run ID/port, Linux architecture, and explicit --transport host-http --allow-insecure-simulation are required")
	}
	out, err := filepath.Abs(opts.Out)
	if err != nil {
		return manifest, err
	}
	repo, err := filepath.Abs(opts.Repo)
	if err != nil {
		return manifest, err
	}
	if _, err = os.Stat(filepath.Join(repo, "cmd", "worldd", "main.go")); err != nil {
		return manifest, errors.New("--repo must name the private World repository")
	}
	if err = os.MkdirAll(filepath.Dir(out), 0700); err != nil {
		return manifest, err
	}
	if err = os.Mkdir(out, 0700); err != nil {
		return manifest, fmt.Errorf("output must be new: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(out)
		}
	}()
	project, clients := filepath.Join(out, "project"), filepath.Join(out, "clients")
	for _, dir := range []string{project, clients} {
		if err = os.Mkdir(dir, 0700); err != nil {
			return manifest, err
		}
	}
	if err = build(repo, filepath.Join(project, "worldd"), opts.Arch, "", "./cmd/worldd"); err != nil {
		return manifest, err
	}
	if err = build(repo, filepath.Join(project, "world-wendy-boot"), opts.Arch, "world_wendy_boot", "./cmd/world-wendy-lab"); err != nil {
		return manifest, err
	}
	// Generate credentials after compilation so the bounded test session is not
	// spent waiting for cross-compilation. The root private key is never saved.
	now := opts.Now().UTC().Truncate(time.Millisecond)
	until := now.Add(59 * time.Minute)
	root, rootKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return manifest, err
	}
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: until.UnixMilli()}, rootKey)
	if err != nil {
		return manifest, err
	}
	frame := world.FrameRef{ID: "office-floor", Revision: "office-frame-v1"}
	publisherSource := world.Source{Device: "sim-wendy-publisher", App: "wendy-world", Service: "publisher", Stream: "observations"}
	for _, who := range []struct {
		name, kind string
		source     world.Source
		scopes     []string
	}{
		{"publisher", "app", publisherSource, []string{"publish", "read"}},
		{"reader", "operator", world.Source{Device: "sim-wendy-reader", App: "wendy-world", Service: "reader", Stream: "read"}, []string{"read"}},
	} {
		pub, key, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return manifest, e
		}
		owner := ""
		if who.name == "publisher" {
			owner = nodeAudience
		}
		g, e := authority.SignGrant(authority.Grant{Version: 1, ID: "lab/" + opts.RunID + "/sim-wendy-" + who.name, Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Principal: "lab/sim-wendy-" + who.name, Kind: who.kind, Source: who.source, PublicKey: pub, Scopes: who.scopes, PublishAudience: owner, NotBeforeUnixMS: now.Add(-time.Second).UnixMilli(), NotAfterUnixMS: until.UnixMilli()}, rootKey)
		if e != nil {
			return manifest, e
		}
		cfg := client.Config{URL: fmt.Sprintf("http://127.0.0.1:%d", opts.Port), Audience: nodeAudience, Grant: g, PrivateKey: []byte(key), AllowInsecureSimulation: true}
		if err = writeJSON(filepath.Join(clients, who.name+".json"), cfg); err != nil {
			return manifest, err
		}
	}
	trust := struct {
		Root        []byte                      `json:"root_public_key"`
		Revocations authority.SignedRevocations `json:"revocations"`
	}{root, rev}
	if err = writeJSON(filepath.Join(clients, "trust.json"), trust); err != nil {
		return manifest, err
	}
	seed := seedConfig{Listen: fmt.Sprintf("0.0.0.0:%d", opts.Port), DataPath: "/data/world.state", ProofPath: "/data/world.proofs", RevocationPath: "/data/world.revocations", ReplayPath: "/data/world.replay", Tenant: "lab", Site: "synthetic-office", ClockDomain: "simulation:" + opts.RunID, Audience: nodeAudience, Frames: []seedFrame{{frame.ID, frame.Revision}}, RootPublicKey: root, AllowInsecureSimulation: true, MaxRecords: 512}
	seed.Revocations, err = json.Marshal(rev)
	if err != nil {
		return manifest, err
	}
	if err = writeJSON(filepath.Join(project, "node-seed.json"), seed); err != nil {
		return manifest, err
	}
	appID := "com.wendy.world.vm." + opts.RunID
	volume := "wendy-world-vm-" + opts.RunID
	app := map[string]any{"appId": appID, "version": "0.1.0", "platform": "linux/" + opts.Arch, "restartPolicy": "unless-stopped", "entitlements": []map[string]any{{"type": "network", "mode": "host"}, {"type": "http", "port": opts.Port}, {"type": "persist", "name": volume, "path": "/data"}}, "readiness": map[string]any{"tcpSocket": map[string]int{"port": opts.Port}, "timeoutSeconds": 30}}
	if err = writeJSON(filepath.Join(project, "wendy.json"), app); err != nil {
		return manifest, err
	}
	for name, data := range map[string][]byte{"Dockerfile": wendysim.Dockerfile, ".dockerignore": wendysim.Dockerignore} {
		if err = writeNew(filepath.Join(project, name), data, 0600); err != nil {
			return manifest, err
		}
	}
	manifest = stageManifest{Schema: "wendy.world.wendy-vm-stage.v1", Directory: out, AppID: appID, VolumeName: volume, RunID: opts.RunID, Tenant: "lab", Site: "synthetic-office", Audience: nodeAudience, Frame: frame, PublisherSource: publisherSource, Architecture: opts.Arch, Transport: opts.Transport, GuestPort: opts.Port, IssuedAtUnixMS: now.UnixMilli(), ExpiresAtUnixMS: until.UnixMilli(), Artifacts: map[string]string{}, Limits: []string{"simulation-only grants; no physical authority", "host HTTP fallback; not authenticated MeshDial evidence", "client URL is a placeholder; use only the actual Wendy-reported loopback forward", "no tasks, executor, robot interfaces, evidence capture, or peer configuration", "root private key discarded; no renewal; expires after 59 minutes"}}
	for _, name := range []string{"worldd", "world-wendy-boot", "node-seed.json", "Dockerfile", ".dockerignore", "wendy.json"} {
		b, e := os.ReadFile(filepath.Join(project, name))
		if e != nil {
			return manifest, e
		}
		sum := sha256.Sum256(b)
		manifest.Artifacts["project/"+name] = hex.EncodeToString(sum[:])
	}
	if err = writeJSON(filepath.Join(out, "manifest.json"), manifest); err != nil {
		return manifest, err
	}
	complete = true
	return manifest, nil
}
