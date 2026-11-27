package main

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/go2executor"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

var bundleNames = []string{"alignment.json", "binding.json", "bridge-token", "executor.json", "node.json", "profile.json"}

type bundleManifest struct {
	Schema          string                `json:"schema"`
	Domain          string                `json:"domain"`
	ReleaseID       string                `json:"release_id"`
	ImageDigest     string                `json:"image_digest"`
	AppID           string                `json:"app_id"`
	Service         string                `json:"service"`
	RunID           string                `json:"run_id"`
	Site            string                `json:"site"`
	TaskID          string                `json:"task_id"`
	Volume          string                `json:"volume"`
	Audience        string                `json:"audience"`
	Assignee        string                `json:"assignee"`
	GatewayPort     int                   `json:"gateway_port"`
	BridgePort      int                   `json:"bridge_port"`
	IssuedAtUnixMS  int64                 `json:"issued_at_unix_ms"`
	ExpiresAtUnixMS int64                 `json:"expires_at_unix_ms"`
	Files           map[string]fileDigest `json:"files"`
}
type commissionedOutput struct {
	Schema       string                `json:"schema"`
	Bundle       bundleManifest        `json:"bundle"`
	BundleSHA256 string                `json:"bundle_sha256"`
	Image        string                `json:"image"`
	Files        map[string]fileDigest `json:"files"`
}

func commission(o options, releaseDir, image string) (result commissionedOutput, err error) {
	if o.Out == "" || o.Binding == "" || o.Alignment == "" || o.Profile == "" || !token.MatchString(o.Site) || !token.MatchString(o.TaskID) || o.Arch != "arm64" || o.Transport != "host-http" || !o.Insecure || !o.Commission || o.Port < 1024 || o.Port > 65535 || o.BridgePort < 1024 || o.BridgePort > 65535 || o.Port == o.BridgePort || o.Port == 8890 || o.BridgePort == 8890 {
		return result, errors.New("explicit bounded simulation commissioning required")
	}
	release, e := loadRelease(releaseDir)
	if e != nil {
		return result, e
	}
	imageSHA, e := imageDigest(image)
	if e != nil {
		return result, e
	}
	bindingRaw, e := readFile(o.Binding, 16384)
	if e != nil {
		return result, e
	}
	var b tasking.Go2Binding
	if authority.DecodeStrict(bindingRaw, &b) != nil || b.Validate() != nil || !runToken.MatchString(b.RunID) {
		return result, errors.New("invalid commissioned binding")
	}
	a, e := readFile(o.Alignment, 16384)
	if e != nil {
		return result, e
	}
	p, e := readFile(o.Profile, 16384)
	if e != nil {
		return result, e
	}
	if _, _, e = go2executor.DecodeConfiguration(go2executor.Config{TaskID: o.TaskID, Site: o.Site, Binding: b, AlignmentJSON: a, ProfileJSON: p}); e != nil {
		return result, errors.New("commissioned configuration refused")
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
	for _, dir := range []string{"carrier", "carrier/bundle", "clients", "mission"} {
		if e = os.Mkdir(filepath.Join(o.Out, dir), 0700); e != nil {
			return result, e
		}
	}
	material, e := generateRun(o, b, bindingRaw, a, p)
	if e != nil {
		return result, e
	}
	bundle := bundleManifest{Schema: "wendy.world.go2-run.v1", Domain: "simulation", ReleaseID: release.ReleaseID, ImageDigest: imageSHA, AppID: motionApp, Service: motionService, RunID: b.RunID, Site: o.Site, TaskID: o.TaskID, Volume: "wendy-world-go2-" + material.Unique, Audience: material.Audience, Assignee: material.Assignee, GatewayPort: o.Port, BridgePort: o.BridgePort, IssuedAtUnixMS: material.Until.Add(-59 * time.Minute).UnixMilli(), ExpiresAtUnixMS: material.Until.UnixMilli(), Files: map[string]fileDigest{}}
	for _, name := range bundleNames {
		bundle.Files[name] = digest(material.Files[name])
	}
	bundleRaw := jsonBytes(bundle)
	bundleSHA := digest(bundleRaw).SHA256
	files := map[string][]byte{"clients/operator.json": material.Operator, "clients/trust.json": material.Trust, "carrier/bundle/run.json": bundleRaw}
	for name, raw := range material.Files {
		files["carrier/bundle/"+name] = raw
	}
	installerApp := "com.wendy.world.go2.install." + material.Unique
	files["carrier/wendy.json"] = jsonBytes(map[string]any{"appId": installerApp, "version": "0.2.0", "platform": "linux/arm64", "entitlements": []any{map[string]any{"type": "network", "mode": "none"}, map[string]any{"type": "persist", "name": bundle.Volume, "path": "/data"}}})
	// Only data differs in this carrier. The executable installer comes from the
	// exact pinned release, not from the untrusted bundle or writable volume.
	args := []string{"python3", "/opt/world/install.py", "--bundle-sha256", bundleSHA, "--release-id", bundle.ReleaseID, "--image-digest", imageSHA}
	argv, _ := json.Marshal(args)
	files["carrier/Dockerfile"] = []byte("FROM " + image + "\nCOPY --chown=0:0 bundle /opt/world/commission-input\nUSER 0:0\nENTRYPOINT " + string(argv) + "\n")
	files["carrier/.dockerignore"] = []byte("**\n!Dockerfile\n!wendy.json\n!bundle/\n!bundle/run.json\n!bundle/alignment.json\n!bundle/binding.json\n!bundle/bridge-token\n!bundle/executor.json\n!bundle/node.json\n!bundle/profile.json\n")
	// JSON is accepted as YAML without quoting ambiguities. There is no build
	// directive: commissioning cannot modify the executable image.
	env := map[string]string{"WORLD_GO2_BUNDLE_SHA256": bundleSHA, "WORLD_GO2_RELEASE_ID": bundle.ReleaseID, "WORLD_GO2_IMAGE_DIGEST": imageSHA}
	files["mission/compose.yaml"] = jsonBytes(map[string]any{"name": "wendy-world-go2", "services": map[string]any{motionService: map[string]any{"image": image, "network_mode": "host", "restart": "no", "volumes": []string{bundle.Volume + ":/data"}, "environment": env}}, "volumes": map[string]any{bundle.Volume: map[string]any{}}})
	files["mission/wendy.json"] = jsonBytes(map[string]any{"appId": motionApp, "version": "0.2.0", "platform": "linux/arm64", "services": map[string]any{motionService: map[string]any{"frameworks": map[string]any{"ros2": map[string]any{"distro": "humble", "rmw": "rmw_cyclonedds_cpp", "domainId": 0, "scope": "peripherals"}}, "entitlements": []any{map[string]any{"type": "http", "port": o.Port}}, "readiness": map[string]any{"tcpSocket": map[string]int{"port": o.Port}, "timeoutSeconds": 30}}}})
	result = commissionedOutput{Schema: "wendy.world.go2-commission.v1", Bundle: bundle, BundleSHA256: bundleSHA, Image: image, Files: map[string]fileDigest{}}
	for _, name := range sortedKeys(files) {
		if e = writeNew(filepath.Join(o.Out, name), files[name], 0600); e != nil {
			return result, e
		}
		result.Files[name] = digest(files[name])
	}
	if e = validateRun(filepath.Join(o.Out, "carrier/bundle"), bundleSHA, bundle.ReleaseID, imageSHA, false, o.Now); e != nil {
		return result, e
	}
	if e = writeNew(filepath.Join(o.Out, "manifest.json"), jsonBytes(result), 0600); e != nil {
		return result, e
	}
	complete = true
	return result, nil
}

// validateRun has no network or storage mutation. Historical mode validates
// issuance at the recorded time; current mode additionally admits motion only
// within the original, unrenewed validity window.
func validateRun(dir, bundleSHA, releaseID, imageSHA string, current bool, clock func() time.Time) error {
	raw, e := readFile(filepath.Join(dir, "run.json"), 64<<10)
	if e != nil {
		return e
	}
	var m bundleManifest
	if digest(raw).SHA256 != bundleSHA || authority.DecodeStrict(raw, &m) != nil || m.Schema != "wendy.world.go2-run.v1" || m.Domain != "simulation" || m.ReleaseID != releaseID || m.ImageDigest != imageSHA || len(releaseID) != 64 || !regexpSHA.MatchString(releaseID) || !regexpImageSHA.MatchString(imageSHA) || m.AppID != motionApp || m.Service != motionService || !runToken.MatchString(m.RunID) || !token.MatchString(m.Site) || !token.MatchString(m.TaskID) || m.Assignee != "sim-go2/navigation" || m.GatewayPort < 1024 || m.GatewayPort > 65535 || m.BridgePort < 1024 || m.BridgePort > 65535 || m.GatewayPort == m.BridgePort || m.GatewayPort == 8890 || m.BridgePort == 8890 || m.IssuedAtUnixMS <= 0 || m.ExpiresAtUnixMS-m.IssuedAtUnixMS != int64((59*time.Minute)/time.Millisecond) || len(m.Files) != len(bundleNames) {
		return errors.New("run manifest refused")
	}
	unique := strings.TrimPrefix(m.Volume, "wendy-world-go2-")
	if !strings.HasPrefix(unique, m.RunID+"-") || len(unique) != len(m.RunID)+17 || !regexpSHA16.MatchString(unique[len(m.RunID)+1:]) || m.Audience != "go2-"+unique {
		return errors.New("run scope refused")
	}
	entries, e := os.ReadDir(dir)
	if e != nil || len(entries) != 7 {
		return errors.New("unexpected run files")
	}
	all := map[string][]byte{}
	for _, name := range bundleNames {
		info, e := os.Lstat(filepath.Join(dir, name))
		if e != nil || info.Mode().Perm()&0077 != 0 {
			return errors.New("run files must remain private")
		}
		data, e := readFile(filepath.Join(dir, name), 64<<10)
		if e != nil || digest(data) != m.Files[name] {
			return errors.New("run file digest mismatch")
		}
		all[name] = data
	}
	var b tasking.Go2Binding
	var c client.Config
	if authority.DecodeStrict(all["binding.json"], &b) != nil || b.Validate() != nil || b.RunID != m.RunID || authority.DecodeStrict(all["executor.json"], &c) != nil {
		return errors.New("run binding or credentials refused")
	}
	if _, _, e = go2executor.DecodeConfiguration(go2executor.Config{TaskID: m.TaskID, Site: m.Site, Binding: b, AlignmentJSON: all["alignment.json"], ProfileJSON: all["profile.json"]}); e != nil {
		return errors.New("run geometry configuration refused")
	}
	if len(all["bridge-token"]) != 65 || all["bridge-token"][64] != '\n' || !regexpSHA.Match(all["bridge-token"][:64]) {
		return errors.New("bridge token format refused")
	}
	var n struct {
		Root  []byte                      `json:"root_public_key"`
		Rev   authority.SignedRevocations `json:"revocations"`
		Grant authority.SignedGrant       `json:"peer_grant"`
		Key   []byte                      `json:"peer_private_key"`
	}
	// Decode the whole node object separately and compare it against the sole
	// supported topology. Unknown paths, peers, capabilities and diagnostics fail.
	if json.Unmarshal(all["node.json"], &n) != nil {
		return errors.New("node credentials refused")
	}
	expected := nodeConfiguration(options{Port: m.GatewayPort, Site: m.Site}, b, m.Audience, "lab/"+unique+"/executor", n.Root, n.Rev, n.Grant, n.Key)
	var actualNode, expectedNode any
	if authority.DecodeStrict(all["node.json"], &actualNode) != nil || json.Unmarshal(jsonBytes(expected), &expectedNode) != nil || !reflect.DeepEqual(actualNode, expectedNode) {
		return errors.New("node topology refused")
	}
	if c.URL != fmt.Sprintf("http://127.0.0.1:%d", m.GatewayPort) || c.Audience != m.Audience || !c.AllowInsecureSimulation || c.ClockTrust != nil {
		return errors.New("executor target refused")
	}
	for _, entry := range []struct {
		name, kind string
		grant      authority.SignedGrant
		key        []byte
		scopes     []string
	}{{"executor", "app", c.Grant, c.PrivateKey, []string{"execute", "read"}}, {"gateway", "device", n.Grant, n.Key, []string{"task-status", "replicate", "read"}}} {
		expectedGrant := authority.Grant{Version: 1, ID: "lab/" + unique + "/" + entry.name, Tenant: "lab", Site: m.Site, Domain: "simulation", Principal: "lab/" + unique + "/" + entry.name, Kind: entry.kind, Source: world.Source{Device: "sim-go2", App: "wendy-world", Service: entry.name, Stream: "state"}, PublicKey: entry.grant.Grant.PublicKey, Scopes: entry.scopes, NotBeforeUnixMS: m.IssuedAtUnixMS - 1000, NotAfterUnixMS: m.ExpiresAtUnixMS}
		if entry.name == "executor" {
			expectedGrant.ExecuteAssignee = m.Assignee
			expectedGrant.PublishAudience = m.Audience
		} else {
			expectedGrant.TaskStatusAssignee = m.Assignee
			expectedGrant.TaskStatusAudience = m.Audience
		}
		if !reflect.DeepEqual(entry.grant.Grant, expectedGrant) || len(entry.key) != ed25519.PrivateKeySize || !reflect.DeepEqual([]byte(ed25519.NewKeyFromSeed(entry.key[:32])), entry.key) || !reflect.DeepEqual(entry.key[32:], []byte(entry.grant.Grant.PublicKey)) {
			return errors.New("run grant scope or key refused")
		}
	}
	at := time.UnixMilli(m.IssuedAtUnixMS)
	if current {
		at = time.Now()
		if clock != nil {
			at = clock()
		}
		if at.UnixMilli() < m.IssuedAtUnixMS || at.UnixMilli() >= m.ExpiresAtUnixMS {
			return errors.New("run credential window unavailable")
		}
	}
	v, e := authority.NewVerifier(authority.Config{RootPublicKey: n.Root, Tenant: "lab", Site: m.Site, Domain: "simulation", Audience: m.Audience, Now: func() time.Time { return at }, Replay: authority.NewMemoryReplay(64)})
	if e != nil {
		return errors.New("run trust refused")
	}
	if e = v.UpdateRevocations(n.Rev); e != nil {
		return errors.New("run revocations refused")
	}
	if _, e = v.VerifyGrant(c.Grant, "execute"); e != nil {
		return errors.New("executor grant refused")
	}
	if _, e = v.VerifyGrant(n.Grant, "task-status"); e != nil {
		return errors.New("gateway grant refused")
	}
	return nil
}
