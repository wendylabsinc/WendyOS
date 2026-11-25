package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"time"
)

type runMaterial struct {
	Files                      map[string][]byte
	Operator, Trust            []byte
	Unique, Audience, Assignee string
	Until                      time.Time
}

func generateRun(o options, b tasking.Go2Binding, bindingBytes, a, p []byte) (runMaterial, error) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	if o.Now != nil {
		now = o.Now().UTC().Truncate(time.Millisecond)
	}
	until := now.Add(59 * time.Minute)
	root, rootKey, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		return runMaterial{}, e
	}
	defer clear(rootKey)
	rev, e := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "lab", Site: o.Site, Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: until.UnixMilli()}, rootKey)
	if e != nil {
		return runMaterial{}, e
	}
	suffix := make([]byte, 8)
	if _, e = rand.Read(suffix); e != nil {
		return runMaterial{}, e
	}
	unique := b.RunID + "-" + hex.EncodeToString(suffix)
	audience := "go2-" + unique
	assignee := "sim-go2/navigation"
	executorPrincipal := "lab/" + unique + "/executor"
	issue := func(name, kind string, scopes []string) (authority.SignedGrant, ed25519.PrivateKey, error) {
		pub, key, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return authority.SignedGrant{}, nil, e
		}
		g := authority.Grant{Version: 1, ID: "lab/" + unique + "/" + name, Tenant: "lab", Site: o.Site, Domain: "simulation", Principal: "lab/" + unique + "/" + name, Kind: kind, Source: world.Source{Device: "sim-go2", App: "wendy-world", Service: name, Stream: "state"}, PublicKey: pub, Scopes: scopes, NotBeforeUnixMS: now.Add(-time.Second).UnixMilli(), NotAfterUnixMS: until.UnixMilli()}
		if name == "executor" {
			g.ExecuteAssignee = assignee
			g.PublishAudience = audience
		}
		if name == "gateway" {
			g.TaskStatusAssignee = assignee
			g.TaskStatusAudience = audience
		}
		s, e := authority.SignGrant(g, rootKey)
		return s, key, e
	}
	gateway, gatewayKey, e := issue("gateway", "device", []string{"task-status", "replicate", "read"})
	if e != nil {
		return runMaterial{}, e
	}
	defer clear(gatewayKey)
	executor, executorKey, e := issue("executor", "app", []string{"execute", "read"})
	if e != nil {
		return runMaterial{}, e
	}
	defer clear(executorKey)
	operator, operatorKey, e := issue("operator", "operator", []string{"mission", "stop", "read"})
	if e != nil {
		return runMaterial{}, e
	}
	defer clear(operatorKey)
	clientCfg := func(g authority.SignedGrant, key ed25519.PrivateKey) client.Config {
		return client.Config{URL: fmt.Sprintf("http://127.0.0.1:%d", o.Port), Audience: audience, Grant: g, PrivateKey: append([]byte(nil), key...), AllowInsecureSimulation: true}
	}
	node := nodeConfiguration(o, b, audience, executorPrincipal, root, rev, gateway, gatewayKey)

	tokenBytes := make([]byte, 32)
	if _, e = rand.Read(tokenBytes); e != nil {
		return runMaterial{}, e
	}
	generated := map[string][]byte{"node.json": jsonBytes(node), "executor.json": jsonBytes(clientCfg(executor, executorKey)), "binding.json": bindingBytes, "alignment.json": a, "profile.json": p, "bridge-token": []byte(hex.EncodeToString(tokenBytes) + "\n")}
	return runMaterial{Files: generated, Operator: jsonBytes(clientCfg(operator, operatorKey)), Trust: jsonBytes(map[string]any{"root_public_key": []byte(root), "revocations": rev}), Unique: unique, Audience: audience, Assignee: assignee, Until: until}, nil
}

func nodeConfiguration(o options, b tasking.Go2Binding, audience, executorPrincipal string, root []byte, rev authority.SignedRevocations, gateway authority.SignedGrant, gatewayKey []byte) map[string]any {
	const assignee = "sim-go2/navigation"
	state := "/data/world-go2/"
	return map[string]any{"listen": fmt.Sprintf("0.0.0.0:%d", o.Port), "data_path": state + "world.state", "proof_path": state + "world.proofs", "revocation_path": state + "world.revocations", "replay_path": state + "world.replay", "tenant": "lab", "site": o.Site, "clock_domain": "simulation:" + b.RunID, "audience": audience, "frames": []world.FrameRef{b.Frame}, "root_public_key": []byte(root), "revocations": rev, "peer_grant": gateway, "peer_private_key": []byte(gatewayKey), "peers": []any{}, "sync_interval_ms": 200, "allow_insecure_simulation": true, "max_records": 512,
		"tasks":      map[string]any{"path": state + "world.tasks", "assignee": assignee, "executor_principal": executorPrincipal, "catalog": map[string]string{tasking.Go2GoToType: "MOVE"}, "enable_supervision": false, "max_lease_ms": 500, "max_telemetry_age_ms": 250, "max_tasks": 32, "go2_binding": b},
		"task_relay": map[string]any{"path": state + "world.task-relay", "max_entries": 32, "max_bytes": 4 << 20, "routes": map[string]any{audience: map[string]string{"assignee": assignee, "status_principal": gateway.Grant.Principal}}}}
}
