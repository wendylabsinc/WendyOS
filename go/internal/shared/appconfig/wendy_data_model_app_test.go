package appconfig

import (
	"encoding/json"
	"os"
	"testing"
)

// TestWendyDataModelAppExampleValidates guards the real
// Examples/WendyDataModelApp app config: the reference app for the model
// harness must parse, validate, declare exactly the camera and episode-write
// entitlements it documents, and produce no ValidateJSON warnings. It fails
// when the example and the config schema drift apart.
func TestWendyDataModelAppExampleValidates(t *testing.T) {
	const path = "../../../../Examples/WendyDataModelApp/wendy.json"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading model app example: %v", err)
	}
	cfg, err := LoadFromBytes(raw)
	if err != nil {
		t.Fatalf("loading model app example: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("model app wendy.json invalid: %v", err)
	}
	if warnings := ValidateJSON(raw); len(warnings) != 0 {
		t.Fatalf("model app wendy.json produced warnings: %v", warnings)
	}

	var episodeWrite, camera bool
	for _, e := range cfg.Entitlements {
		switch e.Type {
		case EntitlementEpisodeWrite:
			episodeWrite = true
		case EntitlementCamera:
			camera = true
		}
	}
	if !episodeWrite {
		t.Error("missing episode-write entitlement (predictions-out contract)")
	}
	// Reads are native: the app opens the agent-fed v4l2loopback node, and
	// that node is a device-node grant, which is what the camera entitlement
	// is. Frame identity arrives in-band in the buffer timestamp, so there is
	// no second entitlement and no second socket to declare.
	if !camera {
		t.Error("missing camera entitlement (the agent-fed node the app reads)")
	}

	// The published JSON schema must accept every entitlement type the
	// example declares. The episode-write entitlement was valid in Go-side
	// validation before the schema listed it; this guard keeps the two
	// surfaces from drifting again.
	consts := schemaEntitlementConsts(t)
	for _, e := range cfg.Entitlements {
		if !consts[e.Type] {
			t.Errorf("wendy.schema.json has no entitlement entry for type %q used by the example", e.Type)
		}
	}

	// Beyond the example's own entitlements, the schema must enumerate every
	// entitlement type the Go validator accepts. ValidEntitlementTypes is the
	// source of truth; a type missing from the schema makes an editor
	// validating against wendy.schema.json falsely reject a manifest the agent
	// would happily run. Walking the full list here catches the whole class of
	// Go/schema drift (this is how the episode-write and mcp gaps went
	// unnoticed).
	for _, typ := range ValidEntitlementTypes {
		if !consts[typ] {
			t.Errorf("wendy.schema.json has no entitlement entry for supported type %q (Go/schema drift)", typ)
		}
	}
}

// schemaEntitlementConsts walks $defs.entitlement.oneOf in the embedded
// wendy.schema.json and returns the set of entitlement type consts it
// declares.
func schemaEntitlementConsts(t *testing.T) map[string]bool {
	t.Helper()
	var schema struct {
		Defs struct {
			Entitlement struct {
				OneOf []struct {
					Properties struct {
						Type struct {
							Const string `json:"const"`
						} `json:"type"`
					} `json:"properties"`
				} `json:"oneOf"`
			} `json:"entitlement"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal([]byte(SchemaJSON), &schema); err != nil {
		t.Fatalf("wendy.schema.json is not valid JSON: %v", err)
	}
	consts := map[string]bool{}
	for _, variant := range schema.Defs.Entitlement.OneOf {
		if variant.Properties.Type.Const != "" {
			consts[variant.Properties.Type.Const] = true
		}
	}
	if len(consts) == 0 {
		t.Fatal("wendy.schema.json declares no entitlement type consts")
	}
	return consts
}
