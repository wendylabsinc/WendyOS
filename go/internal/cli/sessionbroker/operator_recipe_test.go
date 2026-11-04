package sessionbroker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyOperatorRecipeDropsTargeting(t *testing.T) {
	for _, transport := range []string{"tcp", "ble"} {
		t.Run(transport, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "operator.json")
			data := `{"version":1,"principal":"spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/alice","enabled":false,"routerAddress":"old-address","routerUuid":"old-router","transport":"` + transport + `","blePsm":129}`
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			recipe, err := readOperatorRecipe(path)
			if err != nil || recipe.Version != 2 || !recipe.Enabled || !strings.HasSuffix(recipe.Principal, "/operator/alice") {
				t.Fatalf("migration: %+v %v", recipe, err)
			}
			saved, err := json.Marshal(recipe)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{"router", "transport", "blePsm", "automatic", "old-address"} {
				if strings.Contains(string(saved), forbidden) {
					t.Fatalf("retained manual connection policy: %s", saved)
				}
			}
		})
	}
}

func TestLegacyAutomaticRecipeKeepsIdlePolicy(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "operator.json")
		data, err := json.Marshal(map[string]any{"version": 1, "automatic": true, "enabled": enabled, "principal": "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/alice"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		recipe, err := readOperatorRecipe(path)
		if err != nil || recipe.Version != 2 || recipe.Enabled != enabled {
			t.Fatalf("automatic participation changed: %+v %v", recipe, err)
		}
	}
}
