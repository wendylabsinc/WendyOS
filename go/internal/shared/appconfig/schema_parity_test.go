package appconfig

import (
	"encoding/json"
	"slices"
	"testing"
)

// TestSchemaEntitlementKeyParity asserts that every key allowedKeys permits for
// an entitlement type is declared in that type's schema variant. The Go
// validator is the source of truth; because each schema variant sets
// "additionalProperties": false, a key missing from the schema makes editors
// falsely reject manifests the agent accepts (this happened with the camera
// entitlement's IP-camera "user"/"password" keys).
func TestSchemaEntitlementKeyParity(t *testing.T) {
	var schema struct {
		Defs struct {
			Entitlement struct {
				OneOf []struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"oneOf"`
			} `json:"entitlement"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal([]byte(SchemaJSON), &schema); err != nil {
		t.Fatalf("parse wendy.schema.json: %v", err)
	}

	variants := make(map[string]map[string]json.RawMessage)
	for _, v := range schema.Defs.Entitlement.OneOf {
		var typeConst struct {
			Const string `json:"const"`
		}
		raw, ok := v.Properties["type"]
		if !ok {
			continue
		}
		if err := json.Unmarshal(raw, &typeConst); err != nil || typeConst.Const == "" {
			t.Fatalf("entitlement variant has no usable type constant: %s", raw)
		}
		if _, exists := variants[typeConst.Const]; exists {
			t.Fatalf("duplicate schema variant for entitlement %q", typeConst.Const)
		}
		variants[typeConst.Const] = v.Properties
	}
	// Editors must not advertise keys or types that runtime validation rejects.
	for entType, props := range variants {
		keys, ok := allowedKeys[entType]
		if !ok {
			t.Errorf("schema advertises unsupported entitlement %q", entType)
			continue
		}
		for key := range props {
			if !slices.Contains(keys, key) {
				t.Errorf("schema advertises unsupported key %q for entitlement %q", key, entType)
			}
		}
	}

	for entType, keys := range allowedKeys {
		props, ok := variants[entType]
		if !ok {
			t.Errorf("entitlement type %q has no variant in wendy.schema.json", entType)
			continue
		}
		for _, key := range keys {
			if _, ok := props[key]; !ok {
				t.Errorf("entitlement %q: allowedKeys permits %q but the schema variant does not declare it (editors will falsely reject valid manifests)", entType, key)
			}
		}
	}
}
