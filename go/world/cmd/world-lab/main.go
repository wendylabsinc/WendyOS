// world-lab creates short-lived simulation-only credentials and node configs.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/annotationclock"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func generate(directory, runtimeDir, runID string, basePort int, container bool) error {
	return generateOptions(directory, runtimeDir, runID, basePort, container, false, false)
}
func generateOptions(directory, runtimeDir, runID string, basePort int, container, missions, sensors bool) error {
	return generateModeOptions(directory, runtimeDir, runID, basePort, container, missions, sensors, false)
}
func generateModeOptions(directory, runtimeDir, runID string, basePort int, container, missions, sensors, replay bool) error {
	return generateRelayOptions(directory, runtimeDir, runID, basePort, container, missions, sensors, replay, false)
}
func generateRelayOptions(directory, runtimeDir, runID string, basePort int, container, missions, sensors, replay, taskRelay bool) error {
	return generateProposalOptions(directory, runtimeDir, runID, basePort, container, missions, sensors, replay, taskRelay, false)
}
func generateProposalOptions(directory, runtimeDir, runID string, basePort int, container, missions, sensors, replay, taskRelay, proposalStore bool) error {
	return generateNativeMissionOptions(directory, runtimeDir, runID, basePort, container, missions, sensors, replay, taskRelay, proposalStore, "")
}
func generateNativeMissionOptions(directory, runtimeDir, runID string, basePort int, container, missions, sensors, replay, taskRelay, proposalStore bool, nativePackage string) error {
	if nativePackage != "" && (!missions || !sensors || !proposalStore || replay) {
		return errors.New("native mission lab requires missions, sensors and proposals and excludes replay")
	}
	packageManifest, err := nativeMissionManifest(nativePackage)
	if err != nil {
		return err
	}
	if proposalStore && !missions {
		return errors.New("proposal lab requires missions for the trusted catalog")
	}
	if replay && (!sensors || missions) {
		return errors.New("replay lab requires sensors and does not enable missions")
	}
	if taskRelay && !missions {
		return errors.New("task relay lab requires missions")
	}
	sensorPrefix := "sim-sensor-"
	if replay {
		sensorPrefix = "sim-replay-"
	}
	if directory == "" || runID == "" || basePort < 1024 || basePort > 65532 {
		return errors.New("out, run-id and valid base-port required")
	}
	if e := os.MkdirAll(directory, 0700); e != nil {
		return e
	}
	abs, e := filepath.Abs(directory)
	if e != nil {
		return e
	}
	if runtimeDir == "" {
		runtimeDir = abs
	}
	if e = os.Chmod(directory, 0700); e != nil {
		return e
	}
	root, rootKey, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		return e
	}
	now := time.Now()
	until := now.Add(59 * time.Minute)
	rev, e := authority.SignRevocations(authority.Revocations{Version: authority.Version, Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: until.UnixMilli()}, rootKey)
	if e != nil {
		return e
	}
	grant := func(name, kind string, scopes []string) (authority.SignedGrant, ed25519.PrivateKey, error) {
		pub, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return authority.SignedGrant{}, nil, err
		}
		owner := ""
		if name == "sim-publisher" || name == "sim-site-admin" {
			owner = "node-1"
		}
		if name == "sim-load-publisher" {
			owner = "node-2"
		}
		if strings.HasPrefix(name, sensorPrefix) {
			owner = "node-1"
			if name == sensorPrefix+"camera-b" {
				owner = "node-2"
			}
		}
		if name == "sim-fusion" {
			owner = "node-3"
		}
		if strings.HasPrefix(name, "sim-contributor-") {
			owner = "node-3"
		}
		if strings.HasPrefix(name, "sim-clock-") {
			owner = strings.TrimPrefix(name, "sim-clock-")
		}
		if strings.HasPrefix(name, "sim-native-") || name == "sim-mission-clock-node-1" {
			owner = "node-1"
		}
		service, stream := name, "state"
		if strings.HasPrefix(name, sensorPrefix) {
			service = strings.TrimPrefix(name, sensorPrefix)
			stream = "samples"
			if replay {
				stream = "replays"
			}
		}
		if name == "sim-fusion" {
			service = "fusion"
			stream = "hazards"
		}
		executeAssignee := ""
		if name == "sim-executor" {
			if sensors {
				owner = "node-1"
			}
			executeAssignee = "sim-robot/navigation"
		}
		unsigned := authority.Grant{Version: authority.Version, PublishAudience: owner, ExecuteAssignee: executeAssignee, ID: "lab/" + runID + "/" + name, Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Principal: "lab/" + name, Kind: kind, Source: world.Source{Device: name, App: "wendy-world", Service: service, Stream: stream}, PublicKey: pub, Scopes: scopes, NotBeforeUnixMS: now.Add(-time.Second).UnixMilli(), NotAfterUnixMS: until.UnixMilli()}
		if missions && name == "node-1" {
			unsigned.TaskStatusAssignee = "sim-robot/navigation"
			unsigned.TaskStatusAudience = "node-1"
		}
		g, err := authority.SignGrant(unsigned, rootKey)
		return g, key, err
	}
	write := func(name string, v any) error {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		f, err := os.OpenFile(filepath.Join(directory, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = f.Write(append(b, '\n'))
		closeErr := f.Close()
		if err != nil {
			return err
		}
		return closeErr
	}
	clockPins := map[string]*annotationclock.Pins{}
	native, err := newNativeMissionSetup(packageManifest, runID, rootKey, now, until)
	if err != nil {
		return err
	}
	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("node-%d", i)
		scopes := []string{"replicate", "read"}
		if missions && i == 1 {
			scopes = append(scopes, "task-status", "supervision-server")
		}
		g, key, err := grant(name, "device", scopes)
		if err != nil {
			return err
		}
		peers := []map[string]string{}
		for j := 1; j <= 3; j++ {
			if i == j {
				continue
			}
			host := "127.0.0.1"
			port := basePort + j - 1
			if container {
				host = fmt.Sprintf("world-node-%d", j)
				port = 8080
			}
			peers = append(peers, map[string]string{"url": fmt.Sprintf("http://%s:%d", host, port), "audience": fmt.Sprintf("node-%d", j)})
		}
		listen := fmt.Sprintf("127.0.0.1:%d", basePort+i-1)
		if container {
			listen = ":8080"
		}
		cfg := map[string]any{"listen": listen, "data_path": filepath.Join(runtimeDir, name+".state"), "proof_path": filepath.Join(runtimeDir, name+".proofs"), "tenant": "lab", "site": "synthetic-office", "clock_domain": "simulation:" + runID, "audience": name, "frames": []world.FrameRef{{ID: "office-floor", Revision: "office-frame-v1"}, sitepub.MetadataFrame()}, "root_public_key": []byte(root), "revocations": rev, "peer_grant": g, "peer_private_key": []byte(key), "peers": peers, "sync_interval_ms": 200, "allow_insecure_simulation": true}
		if taskRelay {
			cfg["task_relay"] = map[string]any{"path": filepath.Join(runtimeDir, name+".task-relay"), "routes": map[string]any{"node-1": map[string]string{"assignee": "sim-robot/navigation", "status_principal": "lab/node-1"}}}
		}
		if sensors {
			cfg["evidence"] = map[string]any{"path": filepath.Join(runtimeDir, name+".evidence"), "quota_bytes": 64 << 20, "max_blob_bytes": 8 << 20}
			// Clock assertions have their own narrowly scoped key. They cannot
			// replicate state, publish sensors, supervise or move hardware.
			clockGrant, clockKey, clockErr := grant("sim-clock-"+name, "device", []string{"annotation-clock-server"})
			if clockErr != nil {
				return clockErr
			}
			cfg["annotation_clock"] = map[string]any{"grant": clockGrant, "private_key": []byte(clockKey)}
			clockPins[name] = &annotationclock.Pins{RootPublicKey: []byte(root), GatewayPrincipal: clockGrant.Grant.Principal, GatewayKeySHA256: authority.KeyID(clockGrant.Grant.PublicKey), RunID: runID, ClockDomain: "simulation:" + runID}
		}
		if missions && i == 1 {
			catalog := map[string]string{"wendy.nav.go_to.v1": "MOVE"}
			if sensors {
				catalog["wendy.nav.inspect.v1"] = "MOVE"
			}
			cfg["tasks"] = map[string]any{"path": filepath.Join(runtimeDir, "node-1.tasks"), "assignee": "sim-robot/navigation", "executor_principal": "lab/sim-executor", "catalog": catalog, "enable_supervision": true}
			if native != nil {
				clockGrant, clockKey, err := grant("sim-mission-clock-node-1", "device", []string{"mission-clock-server"})
				if err != nil {
					return err
				}
				native.configure(cfg, clockGrant, clockKey, root)
			}
			if proposalStore {
				cfg["proposals"] = map[string]any{"path": filepath.Join(runtimeDir, "node-1.proposals")}
			}
		}
		if err = write(name+".json", cfg); err != nil {
			return err
		}
	}
	g, key, e := grant("sim-publisher", "app", []string{"publish", "read"})
	if e != nil {
		return e
	}
	for i := 1; i <= 3; i++ {
		host := "127.0.0.1"
		port := basePort + i - 1
		if container {
			host = fmt.Sprintf("world-node-%d", i)
			port = 8080
		}
		cfg := client.Config{URL: fmt.Sprintf("http://%s:%d", host, port), Audience: fmt.Sprintf("node-%d", i), Grant: g, PrivateKey: []byte(key), AllowInsecureSimulation: true}
		if e = write(fmt.Sprintf("client-%d.json", i), cfg); e != nil {
			return e
		}
	}

	siteScopes := []string{"publish", "site-publish", "read"}
	if sensors {
		siteScopes = append(siteScopes, "evidence-read", "evidence-write")
	}
	siteGrant, siteKey, err := grant("sim-site-admin", "operator", siteScopes)
	if err != nil {
		return err
	}
	for i := 1; i <= 3; i++ {
		host, port := "127.0.0.1", basePort+i-1
		if container {
			host, port = fmt.Sprintf("world-node-%d", i), 8080
		}
		if err = write(fmt.Sprintf("site-admin-%d.json", i), client.Config{URL: fmt.Sprintf("http://%s:%d", host, port), Audience: fmt.Sprintf("node-%d", i), Grant: siteGrant, PrivateKey: []byte(siteKey), AllowInsecureSimulation: true}); err != nil {
			return err
		}
	}
	if err = write("site-trust.json", map[string]any{"root_public_key": []byte(root), "revocations": rev}); err != nil {
		return err
	}
	if missions {
		for _, identity := range []struct {
			name, kind, file string
			scopes           []string
		}{
			{"sim-operator", "operator", "operator-1.json", []string{"mission", "stop", "read"}},
			{"supervisor-a", "operator", "supervisor-a.json", []string{"supervise", "stop", "read"}},
			{"supervisor-b", "operator", "supervisor-b.json", []string{"supervise", "stop", "read"}},
			{"sim-executor", "app", "executor-1.json", []string{"execute", "read"}},
		} {
			if identity.name == "sim-executor" && sensors {
				identity.scopes = append(identity.scopes, "evidence-write", "evidence-read")
			}
			g, k, err := grant(identity.name, identity.kind, identity.scopes)
			if err != nil {
				return err
			}
			host, port := "127.0.0.1", basePort
			if container {
				host, port = "world-node-1", 8080
			}
			if err = write(identity.file, client.Config{URL: fmt.Sprintf("http://%s:%d", host, port), Audience: "node-1", Grant: g, PrivateKey: []byte(k), AllowInsecureSimulation: true}); err != nil {
				return err
			}
			if taskRelay && identity.name == "sim-operator" {
				for i := 2; i <= 3; i++ {
					host, port := "127.0.0.1", basePort+i-1
					if container {
						host, port = fmt.Sprintf("world-node-%d", i), 8080
					}
					if err = write(fmt.Sprintf("operator-%d.json", i), client.Config{URL: fmt.Sprintf("http://%s:%d", host, port), Audience: fmt.Sprintf("node-%d", i), Grant: g, PrivateKey: []byte(k), AllowInsecureSimulation: true}); err != nil {
						return err
					}
				}
			}
		}
		loadGrant, loadKey, err := grant("sim-load-publisher", "app", []string{"publish", "read"})
		if err != nil {
			return err
		}
		loadHost, loadPort := "127.0.0.1", basePort+1
		if container {
			loadHost, loadPort = "world-node-2", 8080
		}
		if err = write("load-publisher.json", client.Config{URL: fmt.Sprintf("http://%s:%d", loadHost, loadPort), Audience: "node-2", Grant: loadGrant, PrivateKey: []byte(loadKey), AllowInsecureSimulation: true}); err != nil {
			return err
		}
		tokenFiles := []string{"adapter.token"}
		if sensors {
			tokenFiles = append(tokenFiles, "sensor.token")
		}
		for _, tokenFile := range tokenFiles {
			token := make([]byte, 32)
			if _, err := rand.Read(token); err != nil {
				return err
			}
			f, err := os.OpenFile(filepath.Join(directory, tokenFile), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			_, err = f.WriteString(hex.EncodeToString(token))
			closeErr := f.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
	}
	if sensors {
		configFor := func(index int, g authority.SignedGrant, k ed25519.PrivateKey) client.Config {
			host, port := "127.0.0.1", basePort+index-1
			if container {
				host, port = fmt.Sprintf("world-node-%d", index), 8080
			}
			return client.Config{URL: fmt.Sprintf("http://%s:%d", host, port), Audience: fmt.Sprintf("node-%d", index), Grant: g, PrivateKey: []byte(k), AllowInsecureSimulation: true}
		}
		for _, sensor := range []string{"camera-a", "camera-b", "gas-1", "audio-1"} {
			scopes := []string{"publish", "evidence-write"}
			if missions || replay {
				// The executor may retain identical capture bytes first. Sensors
				// need read authority to verify and reuse that existing object.
				scopes = append(scopes, "evidence-read")
			}
			g, k, err := grant(sensorPrefix+sensor, "app", scopes)
			if err != nil {
				return err
			}
			i := 1
			if sensor == "camera-b" {
				i = 2
			}
			if err = write("sensor-"+sensor+".json", configFor(i, g, k)); err != nil {
				return err
			}
		}
		g, k, err := grant("sim-fusion", "app", []string{"publish", "read", "evidence-read"})
		if err != nil {
			return err
		}
		sources := map[string]world.Source{}
		for _, sensor := range []string{"camera-a", "camera-b", "gas-1", "audio-1"} {
			stream := "samples"
			if replay {
				stream = "replays"
			}
			sources[sensor] = world.Source{Device: sensorPrefix + sensor, App: "wendy-world", Service: sensor, Stream: stream}
		}
		if err = write("fusion.json", map[string]any{"publisher": configFor(3, g, k), "evidence_sources": []client.Config{configFor(1, g, k), configFor(2, g, k)}, "sources": sources, "required_sensors": map[string]string{"camera-a": "camera", "camera-b": "camera", "gas-1": "gas", "audio-1": "audio"}}); err != nil {
			return err
		}
		for i := 1; i <= 2; i++ {
			g, k, err = grant(fmt.Sprintf("sim-viewer-%d", i), "operator", []string{"read", "evidence-read"})
			if err != nil {
				return err
			}
			if err = write(fmt.Sprintf("viewer-%d.json", i), configFor(i, g, k)); err != nil {
				return err
			}
		}
		for _, platform := range []string{"apple", "macos", "ios", "visionos", "android"} {
			g, k, err = grant("sim-contributor-"+platform, "operator", []string{"read", "evidence-read", "annotation-write"})
			if err != nil {
				return err
			}
			contributor := configFor(3, g, k)
			contributor.ClockTrust = clockPins["node-3"]
			if err = write("contributor-"+platform+".json", contributor); err != nil {
				return err
			}
		}
	}

	if proposalStore {
		g, k, err := grant("sim-agent", "agent", []string{"read", "propose"})
		if err != nil {
			return err
		}
		host, port := "127.0.0.1", basePort
		if container {
			host, port = "world-node-1", 8080
		}
		if err = write("agent-1.json", client.Config{URL: fmt.Sprintf("http://%s:%d", host, port), Audience: "node-1", Grant: g, PrivateKey: []byte(k), AllowInsecureSimulation: true}); err != nil {
			return err
		}
	}
	if native != nil {
		if err := native.write(write, grant, rootKey, basePort, container, now); err != nil {
			return err
		}
	}
	// Lab issuer is scoped to the generated simulation roots. Never use this
	// private key or any generated grant for physical hardware or production.
	return write("issuer.json", map[string]any{"domain": "simulation", "private_key": []byte(rootKey), "public_key": []byte(root), "revocations": rev})
}
func main() {
	out := flag.String("out", "", "new private output directory")
	runtimeDir := flag.String("runtime-dir", "", "path mounted into runtime containers")
	runID := flag.String("run-id", "lab", "simulation run ID")
	port := flag.Int("base-port", 18081, "first local node port")
	containers := flag.Bool("containers", false, "configure isolated Docker network hostnames")
	missions := flag.Bool("missions", false, "add simulation mission authority and executor credentials")
	sensors := flag.Bool("sensors", false, "add multimodal sensor, fusion and evidence credentials")
	replay := flag.Bool("replay", false, "create distinct replay sensor identities; requires sensors and excludes missions")
	taskRelay := flag.Bool("task-relay", false, "add durable task relay configuration and gateway operator credentials; requires missions")
	proposalStore := flag.Bool("proposals", false, "add non-executable proposal store and agent credentials; requires missions")
	nativePackage := flag.String("native-mission-package", "", "verified synthetic one-axis world package for explicitly reviewed native missions; requires missions, sensors and proposals")
	flag.Parse()
	if e := generateNativeMissionOptions(*out, *runtimeDir, *runID, *port, *containers, *missions, *sensors, *replay, *taskRelay, *proposalStore, *nativePackage); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	fmt.Println("Created three simulation-only node configs and client credentials. Grants expire in one hour.")
}
