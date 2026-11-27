package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
)

const motionApp = "com.wendy.world.go2"
const motionService = "mission"

// release.json describes code, never an OCI identity. The external registry
// digest is supplied at commissioning and must be observed on the selected agent.
type releaseManifest struct {
	Schema string                `json:"schema"`
	Domain string                `json:"domain"`
	Arch   string                `json:"arch"`
	Files  map[string]fileDigest `json:"files"`
}
type releaseOutput struct {
	Schema    string                `json:"schema"`
	Domain    string                `json:"domain"`
	Arch      string                `json:"arch"`
	ReleaseID string                `json:"release_id"`
	Files     map[string]fileDigest `json:"files"`
}
type releaseOptions struct{ Out, Repo, Arch string }

var imageRef = regexp.MustCompile(`^(?:[a-z0-9][a-z0-9.-]*\.[a-z0-9.-]+|localhost)(?::[1-9][0-9]{0,4})?/[a-z0-9][a-z0-9._/-]*@sha256:[a-f0-9]{64}$`)
var regexpSHA = regexp.MustCompile(`^[0-9a-f]{64}$`)
var regexpSHA16 = regexp.MustCompile(`^[0-9a-f]{16}$`)
var regexpImageSHA = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func cloudSourceFiles() []string {
	return []string{"sim/__init__.py", "sim/go2_cloud.py", "sim/go2_cloud_observer.py", "sim/go2_cloud_recording.py", "sim/go2_cloud_stream.py", "sim/go2_cloud_capture.py", "sim/go2_cloud_shadow.py", "sim/go2_cloud_live.py", "sim/go2_cloud_native/CMakeLists.txt", "sim/go2_cloud_native/cloud.cpp"}
}

func sourceInventory() map[string]string {
	files := map[string]string{}
	for _, name := range []string{"boot.py", "release_boot.py", "run_bundle.py", "install.py", "seal_release.py", "mission-entrypoint.sh", "release-entrypoint.sh", "cyclonedds.xml"} {
		files[name] = "deploy/go2-sim/" + name
	}
	for _, name := range []string{"__init__.py", "__main__.py", "core.py", "protocol.py", "transport.py", "native/CMakeLists.txt", "native/sensors.cpp"} {
		files["sim/go2_bridge/"+name] = "sim/go2_bridge/" + name
	}
	for _, name := range cloudSourceFiles() {
		files[name] = name
	}
	return files
}
func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func ensureParents(root, name string) error {
	return os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0700)
}

func release(o releaseOptions, build builder) (result releaseOutput, err error) {
	if o.Out == "" || o.Repo == "" || o.Arch != "arm64" {
		return result, errors.New("new output, private repository and pinned arm64 platform required")
	}
	repo, e := filepath.Abs(o.Repo)
	if e != nil {
		return result, e
	}
	source, e := readFile(filepath.Join(repo, "deploy/go2-sim/Dockerfile"), 64<<10)
	if e != nil {
		return result, e
	}
	const marker = "FROM dependencies AS probe\n"
	if bytes.Count(source, []byte(marker)) != 1 || !bytes.Contains(source, []byte("FROM ros:humble-ros-base@sha256:7bea3d9aa2483d3ca34c8e30d921b79273b0913bd7dc64bebf51d082b5d107e4 AS dependencies")) {
		return result, errors.New("reviewed arm64 dependency stage unavailable")
	}
	if e = os.Mkdir(o.Out, 0700); e != nil {
		return result, errors.New("output must be a new directory")
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(o.Out)
		}
	}()
	project := filepath.Join(o.Out, "project")
	if e = os.Mkdir(project, 0700); e != nil {
		return result, e
	}
	code := releaseManifest{Schema: "wendy.world.go2-release.v1", Domain: "simulation", Arch: o.Arch, Files: map[string]fileDigest{}}
	for _, name := range []string{"worldd", "world-go2-executor", "world-go2-lab"} {
		path := filepath.Join(project, name)
		if e = build(repo, path, o.Arch, "./cmd/"+name); e != nil {
			return result, e
		}
		raw, e := readFile(path, 128<<20)
		if e != nil {
			return result, e
		}
		code.Files[name] = digest(raw)
	}
	for _, dst := range sortedKeys(sourceInventory()) {
		raw, e := readFile(filepath.Join(repo, sourceInventory()[dst]), 2<<20)
		if e != nil {
			return result, e
		}
		if e = ensureParents(project, dst); e != nil {
			return result, e
		}
		if e = writeNew(filepath.Join(project, dst), raw, 0600); e != nil {
			return result, e
		}
		code.Files[dst] = digest(raw)
	}
	codeRaw := jsonBytes(code)
	docker := append(append([]byte(nil), bytes.SplitN(source, []byte(marker), 2)[0]...), []byte(releaseDockerfile)...)
	app := jsonBytes(map[string]any{"appId": "com.wendy.world.go2.release", "version": "0.2.0", "platform": "linux/arm64"})
	extra := map[string][]byte{"release.json": codeRaw, "Dockerfile": docker, "wendy.json": app}
	ignore := "**\n!sim/\n!sim/go2_bridge/\n!sim/go2_bridge/native/\n!sim/go2_cloud_native/\n"
	for _, name := range append(sortedKeys(code.Files), sortedKeys(extra)...) {
		ignore += "!" + name + "\n"
	}
	extra[".dockerignore"] = []byte(ignore)
	for name, raw := range extra {
		if e = writeNew(filepath.Join(project, name), raw, 0600); e != nil {
			return result, e
		}
	}
	result = releaseOutput{Schema: "wendy.world.go2-release-output.v1", Domain: "simulation", Arch: o.Arch, ReleaseID: digest(codeRaw).SHA256, Files: map[string]fileDigest{}}
	for _, name := range append(sortedKeys(code.Files), sortedKeys(extra)...) {
		raw, e := readFile(filepath.Join(project, name), 128<<20)
		if e != nil {
			return result, e
		}
		result.Files["project/"+name] = digest(raw)
	}
	if e = writeNew(filepath.Join(o.Out, "manifest.json"), jsonBytes(result), 0600); e != nil {
		return result, e
	}
	complete = true
	return result, nil
}

func loadRelease(dir string) (releaseOutput, error) {
	var out releaseOutput
	raw, e := readFile(filepath.Join(dir, "manifest.json"), 64<<10)
	if e != nil {
		return out, e
	}
	if authority.DecodeStrict(raw, &out) != nil || out.Schema != "wendy.world.go2-release-output.v1" || out.Domain != "simulation" || out.Arch != "arm64" || len(out.Files) != len(sourceInventory())+7 {
		return out, errors.New("invalid release inventory")
	}
	expected := map[string]bool{"project/worldd": true, "project/world-go2-executor": true, "project/world-go2-lab": true, "project/release.json": true, "project/Dockerfile": true, "project/wendy.json": true, "project/.dockerignore": true}
	for key := range sourceInventory() {
		expected["project/"+key] = true
	}
	for name, entry := range out.Files {
		if !expected[name] {
			return out, errors.New("unexpected release file")
		}
		raw, e := readFile(filepath.Join(dir, name), 128<<20)
		if e != nil || digest(raw) != entry {
			return out, errors.New("release input changed")
		}
	}
	if out.Files["project/release.json"].SHA256 != out.ReleaseID {
		return out, errors.New("release identity mismatch")
	}
	return out, nil
}

func imageDigest(ref string) (string, error) {
	if !imageRef.MatchString(ref) || strings.Contains(ref, "..") || strings.Contains(ref, "//") {
		return "", errors.New("explicit fully qualified digest-pinned image required")
	}
	return strings.Split(ref, "@")[1], nil
}

const releaseDockerfile = `FROM dependencies AS mission-release
COPY --chown=65532:65532 worldd world-go2-executor world-go2-lab /opt/world/
COPY --chown=65532:65532 boot.py release_boot.py run_bundle.py install.py seal_release.py mission-entrypoint.sh release-entrypoint.sh cyclonedds.xml release.json /opt/world/
COPY --chown=65532:65532 sim /opt/world/sim
RUN /bin/bash -c 'source /opt/ros/humble/setup.bash && cmake -S sim/go2_bridge/native -B /tmp/world-sensors -DCMAKE_BUILD_TYPE=Release && cmake --build /tmp/world-sensors -j2 && install -m755 /tmp/world-sensors/world-go2-sensors /opt/world/world-go2-sensors && rm -rf /tmp/world-sensors'
RUN /bin/bash -c 'source /opt/ros/humble/setup.bash && cmake -S sim/go2_cloud_native -B /tmp/world-cloud -DCMAKE_BUILD_TYPE=Release && cmake --build /tmp/world-cloud --target world-go2-cloud-live -j2 && install -m755 /tmp/world-cloud/world-go2-cloud-live /opt/world/world-go2-cloud-live && rm -rf /tmp/world-cloud'
RUN chmod 0700 /opt/world/worldd /opt/world/world-go2-executor /opt/world/world-go2-lab && python3 /opt/world/seal_release.py
USER 0:0
ENTRYPOINT ["/bin/bash", "/opt/world/release-entrypoint.sh"]
`
