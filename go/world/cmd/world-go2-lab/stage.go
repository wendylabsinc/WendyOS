package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/go2executor"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

type options struct {
	Out, Repo, Binding, Alignment, Profile, Site, TaskID, Arch, Transport string
	Port, BridgePort                                                      int
	Insecure, Commission                                                  bool
	Now                                                                   func() time.Time
}
type builder func(repo, path, arch, target string) error
type fileDigest struct {
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}
type manifest struct {
	Schema            string                `json:"schema"`
	Domain            string                `json:"domain"`
	RunID             string                `json:"run_id"`
	Site              string                `json:"site"`
	TaskID            string                `json:"task_id"`
	AppID             string                `json:"app_id"`
	Volume            string                `json:"volume"`
	Arch              string                `json:"arch"`
	Audience          string                `json:"audience"`
	Assignee          string                `json:"assignee"`
	Port              int                   `json:"port"`
	BridgePort        int                   `json:"bridge_port"`
	Transport         string                `json:"transport"`
	ExpiresAtUnixMS   int64                 `json:"expires_at_unix_ms"`
	PhysicalAuthority bool                  `json:"physical_authority"`
	Files             map[string]fileDigest `json:"files"`
}
type bootConfig struct {
	Schema      string                `json:"schema"`
	RunID       string                `json:"run_id"`
	TaskID      string                `json:"task_id"`
	GatewayPort int                   `json:"gateway_port"`
	BridgePort  int                   `json:"bridge_port"`
	Files       map[string]fileDigest `json:"files"`
}

var token = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
var runToken = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Nonblocking/no-follow opening keeps FIFOs and final-component swaps from
// turning command-line input into an unbounded read. Ancestors are explicit
// operator-selected paths; no device or user credential discovery takes place.
func readFile(path string, limit int64) ([]byte, error) {
	info, e := os.Lstat(path)
	if e != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("expected bounded regular input file")
	}
	fd, e := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return nil, errors.New("cannot open regular input file")
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	after, e := f.Stat()
	if e != nil || !after.Mode().IsRegular() || !os.SameFile(info, after) || after.Size() > limit {
		return nil, errors.New("input file changed or exceeds bound")
	}
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil || int64(len(b)) > limit {
		return nil, errors.New("cannot read bounded input file")
	}
	return b, nil
}
func writeNew(path string, b []byte, mode os.FileMode) error {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if e != nil {
		return e
	}
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	c := f.Close()
	if e == nil {
		e = c
	}
	return e
}
func jsonBytes(v any) []byte {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		panic(e)
	}
	return append(b, '\n')
}
func digest(b []byte) fileDigest {
	h := sha256.Sum256(b)
	return fileDigest{hex.EncodeToString(h[:]), len(b)}
}
func buildBinary(repo, path, arch, target string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, "go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o", path, target)
	c.Dir = repo
	// Remove duplicates so inherited cross-compilation settings cannot win.
	for _, s := range os.Environ() {
		if !strings.HasPrefix(s, "GOOS=") && !strings.HasPrefix(s, "GOARCH=") && !strings.HasPrefix(s, "CGO_ENABLED=") {
			c.Env = append(c.Env, s)
		}
	}
	c.Env = append(c.Env, "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	if e := c.Run(); e != nil {
		return errors.New("bounded private Go build failed")
	}
	return os.Chmod(path, 0755)
}
func stage(o options, build builder) (m manifest, err error) {
	if o.Out == "" || o.Repo == "" || o.Binding == "" || o.Alignment == "" || o.Profile == "" || !token.MatchString(o.Site) || !token.MatchString(o.TaskID) || o.Arch != "arm64" || o.Transport != "host-http" || !o.Insecure || !o.Commission || o.Port < 1024 || o.Port > 65535 || o.BridgePort < 1024 || o.BridgePort > 65535 || o.Port == o.BridgePort || o.Port == 8890 || o.BridgePort == 8890 {
		return m, errors.New("explicit arm64 simulation commissioning, distinct unprivileged ports, site and task required; amd64 base is not pinned")
	}
	bindingBytes, e := readFile(o.Binding, 16384)
	if e != nil {
		return m, e
	}
	var b tasking.Go2Binding
	if authority.DecodeStrict(bindingBytes, &b) != nil || b.Validate() != nil || !runToken.MatchString(b.RunID) {
		return m, errors.New("invalid commissioned Go2 binding or bounded lowercase run ID")
	}
	a, e := readFile(o.Alignment, 16384)
	if e != nil {
		return m, e
	}
	p, e := readFile(o.Profile, 16384)
	if e != nil {
		return m, e
	}
	if _, _, e = go2executor.DecodeConfiguration(go2executor.Config{TaskID: o.TaskID, Site: o.Site, Binding: b, AlignmentJSON: a, ProfileJSON: p}); e != nil {
		return m, e
	}
	repo, e := filepath.Abs(o.Repo)
	if e != nil {
		return m, e
	}
	out, e := filepath.Abs(o.Out)
	if e != nil {
		return m, e
	}
	if e = os.Mkdir(out, 0700); e != nil {
		return m, errors.New("output must be a new directory")
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(out)
		}
	}()
	project := filepath.Join(out, "project")
	clients := filepath.Join(out, "clients")
	for _, d := range []string{project, clients, filepath.Join(project, "sim"), filepath.Join(project, "sim/go2_bridge"), filepath.Join(project, "sim/go2_bridge/native")} {
		if e = os.Mkdir(d, 0700); e != nil {
			return m, e
		}
	}
	// Compilation precedes issuance so it does not spend the short grant lifetime.
	for _, name := range []string{"worldd", "world-go2-executor"} {
		if e = build(repo, filepath.Join(project, name), o.Arch, "./cmd/"+name); e != nil {
			return m, e
		}
	}
	copied := map[string]string{"boot.py": "deploy/go2-sim/boot.py", "mission-entrypoint.sh": "deploy/go2-sim/mission-entrypoint.sh", "cyclonedds.xml": "deploy/go2-sim/cyclonedds.xml"}
	for _, n := range []string{"__init__.py", "__main__.py", "core.py", "protocol.py", "transport.py", "native/CMakeLists.txt", "native/sensors.cpp"} {
		copied["sim/go2_bridge/"+n] = "sim/go2_bridge/" + n
	}
	for _, name := range cloudSourceFiles() {
		copied[name] = name
	}
	for dst, src := range copied {
		if e = ensureParents(project, dst); e != nil {
			return m, e
		}
		raw, e := readFile(filepath.Join(repo, src), 2<<20)
		if e != nil {
			return m, e
		}
		if e = writeNew(filepath.Join(project, dst), raw, 0600); e != nil {
			return m, e
		}
	}
	dockerSource, e := readFile(filepath.Join(repo, "deploy/go2-sim/Dockerfile"), 64<<10)
	if e != nil {
		return m, e
	}
	const marker = "FROM dependencies AS probe\n"
	if bytes.Count(dockerSource, []byte(marker)) != 1 || !bytes.Contains(dockerSource, []byte("FROM ros:humble-ros-base@sha256:7bea3d9aa2483d3ca34c8e30d921b79273b0913bd7dc64bebf51d082b5d107e4 AS dependencies")) {
		return m, errors.New("reviewed arm64 dependency stage unavailable")
	}
	prefix := bytes.SplitN(dockerSource, []byte(marker), 2)[0]
	docker := append(append([]byte(nil), prefix...), []byte(missionDockerfile)...)
	if e = writeNew(filepath.Join(project, "Dockerfile"), docker, 0600); e != nil {
		return m, e
	}
	material, e := generateRun(o, b, bindingBytes, a, p)
	if e != nil {
		return m, e
	}
	unique, audience, assignee, until := material.Unique, material.Audience, material.Assignee, material.Until
	for name, data := range material.Files {
		if e = writeNew(filepath.Join(project, name), data, 0600); e != nil {
			return m, e
		}
	}
	for name, data := range map[string][]byte{"operator.json": material.Operator, "trust.json": material.Trust} {
		if e = writeNew(filepath.Join(clients, name), data, 0600); e != nil {
			return m, e
		}
	}
	volume := "wendy-world-go2-" + unique
	appID := "com.wendy.world.go2." + unique
	app := map[string]any{"appId": appID, "version": "0.1.0", "platform": "linux/arm64", "restartPolicy": "unless-stopped", "frameworks": map[string]any{"ros2": map[string]any{"distro": "humble", "rmw": "rmw_cyclonedds_cpp", "domainId": 0, "scope": "peripherals"}}, "entitlements": []any{map[string]any{"type": "network", "mode": "host"}, map[string]any{"type": "http", "port": o.Port}, map[string]any{"type": "persist", "name": volume, "path": "/data"}}, "readiness": map[string]any{"tcpSocket": map[string]int{"port": o.Port}, "timeoutSeconds": 30}}
	if e = writeNew(filepath.Join(project, "wendy.json"), jsonBytes(app), 0600); e != nil {
		return m, e
	}
	boot := bootConfig{Schema: "wendy.world.go2-boot.v1", RunID: b.RunID, TaskID: o.TaskID, GatewayPort: o.Port, BridgePort: o.BridgePort, Files: map[string]fileDigest{}}
	for _, name := range append([]string{"worldd", "world-go2-executor", "node.json", "executor.json", "binding.json", "alignment.json", "profile.json", "bridge-token", "boot.py", "mission-entrypoint.sh", "cyclonedds.xml", "sim/go2_bridge/__init__.py", "sim/go2_bridge/__main__.py", "sim/go2_bridge/core.py", "sim/go2_bridge/protocol.py", "sim/go2_bridge/transport.py", "sim/go2_bridge/native/CMakeLists.txt", "sim/go2_bridge/native/sensors.cpp"}, cloudSourceFiles()...) {
		raw, e := readFile(filepath.Join(project, name), 128<<20)
		if e != nil {
			return m, e
		}
		boot.Files[name] = digest(raw)
	}
	if e = writeNew(filepath.Join(project, "boot.json"), jsonBytes(boot), 0600); e != nil {
		return m, e
	}
	// Docker receives only this generated context. A later accidental sibling
	// copy is excluded too, including clients and the commissioning input files.
	allow := []string{"Dockerfile", "wendy.json", "boot.json"}
	for name := range boot.Files {
		allow = append(allow, name)
	}
	ignore := "**\n!sim/\n!sim/go2_bridge/\n!sim/go2_bridge/native/\n!sim/go2_cloud_native/\n"
	for _, name := range allow {
		ignore += "!" + name + "\n"
	}
	if e = writeNew(filepath.Join(project, ".dockerignore"), []byte(ignore), 0600); e != nil {
		return m, e
	}
	m = manifest{Schema: "wendy.world.go2-lab.v1", Domain: "simulation", RunID: b.RunID, Site: o.Site, TaskID: o.TaskID, AppID: appID, Volume: volume, Arch: o.Arch, Audience: audience, Assignee: assignee, Port: o.Port, BridgePort: o.BridgePort, Transport: "host-http", ExpiresAtUnixMS: until.UnixMilli(), Files: map[string]fileDigest{}}
	for _, name := range append(allow, ".dockerignore") {
		raw, e := readFile(filepath.Join(project, name), 128<<20)
		if e != nil {
			return m, e
		}
		m.Files["project/"+name] = digest(raw)
	}
	if e = writeNew(filepath.Join(out, "manifest.json"), jsonBytes(m), 0600); e != nil {
		return m, e
	}
	success = true
	return m, nil
}

const missionDockerfile = `FROM dependencies AS mission
COPY --chown=65532:65532 worldd world-go2-executor /opt/world/
COPY --chown=65532:65532 boot.py mission-entrypoint.sh cyclonedds.xml boot.json node.json executor.json binding.json alignment.json profile.json bridge-token /opt/world/
COPY --chown=65532:65532 sim /opt/world/sim
RUN /bin/bash -c 'source /opt/ros/humble/setup.bash && cmake -S sim/go2_bridge/native -B /tmp/world-sensors -DCMAKE_BUILD_TYPE=Release && cmake --build /tmp/world-sensors -j2 && install -m755 /tmp/world-sensors/world-go2-sensors /opt/world/world-go2-sensors && rm -rf /tmp/world-sensors'
RUN /bin/bash -c 'source /opt/ros/humble/setup.bash && cmake -S sim/go2_cloud_native -B /tmp/world-cloud -DCMAKE_BUILD_TYPE=Release && cmake --build /tmp/world-cloud --target world-go2-cloud-live -j2 && install -m755 /tmp/world-cloud/world-go2-cloud-live /opt/world/world-go2-cloud-live && rm -rf /tmp/world-cloud'
RUN chmod 0700 /opt/world/worldd /opt/world/world-go2-executor && chmod 0600 /opt/world/*.json /opt/world/bridge-token
USER 0:0
ENTRYPOINT ["/bin/bash", "/opt/world/mission-entrypoint.sh"]
`
