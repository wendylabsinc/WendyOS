package commands

import (
	"reflect"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

func TestEffectiveServiceEnvs(t *testing.T) {
	t.Setenv("ENV_FIXTURE", "expanded")
	app := &appconfig.AppConfig{Env: map[string]string{"DEFAULT": "$ENV_FIXTURE", "SHARED": "app", "EMPTY": "app"}}
	services := map[string]*appconfig.ServiceConfig{
		"api":    {Env: map[string]string{"SHARED": "api", "ONLY": "api"}},
		"worker": {Env: map[string]string{"SHARED": "worker", "ONLY": "worker"}},
	}
	envs := effectiveServiceEnvs(app, services, []string{"SHARED=first", "SHARED=last", "EMPTY=", "LITERAL=$ENV_FIXTURE"})
	for name := range services {
		want := []string{"DEFAULT=expanded", "EMPTY=", "LITERAL=$ENV_FIXTURE", "ONLY=" + name, "SHARED=last"}
		if !reflect.DeepEqual(envs[name], want) {
			t.Fatalf("%s environment = %q, want %q", name, envs[name], want)
		}
	}
	if app.Env["SHARED"] != "app" || services["api"].Env["SHARED"] != "api" {
		t.Fatal("resolution mutated configuration")
	}
}

func TestEnvironmentOnlyChangeInvalidatesWatchService(t *testing.T) {
	cfg := &appconfig.AppConfig{AppID: "app"}
	services := map[string]*appconfig.ServiceConfig{"api": {}, "worker": {}}
	before := effectiveServiceEnvs(cfg, services, []string{"MODE=one"})
	after := effectiveServiceEnvs(cfg, services, []string{"MODE=two"})
	state := &watchDeployState{hashes: map[string]string{}}
	for name := range services {
		oldHash, err := multiServiceWatchHash("same-image", cfg, before[name], nil)
		if err != nil {
			t.Fatal(err)
		}
		newHash, err := multiServiceWatchHash("same-image", cfg, after[name], nil)
		if err != nil {
			t.Fatal(err)
		}
		key := watchServiceKey("device", cfg.AppID, name)
		state.record(key, oldHash)
		if state.matches(key, newHash) {
			t.Fatalf("%s was preserved after environment changed", name)
		}
	}
}

func TestEnvironmentFingerprintIgnoresOverriddenValues(t *testing.T) {
	one := mergeEnvEntries([]string{"KEY=ignored", "EMPTY=ignored"}, []string{"KEY=final", "EMPTY="})
	two := mergeEnvEntries([]string{"EMPTY=", "KEY=final"})
	if !reflect.DeepEqual(one, two) {
		t.Fatalf("equivalent environments differ: %v, %v", one, two)
	}
}
