//go:build !windows

package sessionbroker

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestUnchangedOperatorRecipeDoesNotWaitForAnotherStartup(t *testing.T) {
	directory := shortTempDir(t)
	previous := configDir
	t.Cleanup(func() { configDir = previous })
	configDir = func() (string, error) { return directory, nil }
	recipe := OperatorSpec{Version: 2, Principal: "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/intent"}
	path := operatorRecipePath(directory, recipe.Principal)
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(recipe)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePrivateFile(path, data); err != nil {
		t.Fatal(err)
	}
	spec := Spec{Key: operatorKey(recipe.Principal), Operator: &recipe}
	brokerDirectory, err := brokerDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDir(brokerDirectory); err != nil {
		t.Fatal(err)
	}
	socket, _ := spec.paths(brokerDirectory)
	startup, acquired, err := acquireIdentityLock(socket + ".startup")
	if err != nil || !acquired {
		t.Fatal("could not simulate another joining command", err)
	}
	defer func() { releaseLock(startup); startup.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := writeOperatorRecipe(ctx, spec, path, recipe, false); err != nil {
		t.Fatalf("unchanged intent blocked behind another command's startup: %v", err)
	}
}
