package commands

import (
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"testing"
)

func TestSimulationBackendIsExplicitAndDoesNotChangeHardwareConfig(t *testing.T) {
	svc := &appconfig.ServiceConfig{Context: "./camera", Entitlements: []appconfig.Entitlement{{Type: "usb"}}, Env: map[string]string{"KEEP": "yes"}, Simulation: &appconfig.SimulationConfig{Profile: "rosmaster-r2", Entitlements: []appconfig.Entitlement{{Type: "network", Mode: "host"}}, Env: map[string]string{"R2_SIMULATOR_URL": "http://127.0.0.1:8890"}}}
	cfg := &appconfig.AppConfig{AppID: "test", Services: map[string]*appconfig.ServiceConfig{"camera": svc}}
	got, err := applyRobotSimulationConfig(cfg, "rosmaster-r2")
	if err != nil {
		t.Fatal(err)
	}
	camera := got.Services["camera"]
	if camera.Entitlements[0].Type != "network" || camera.Env["KEEP"] != "yes" || camera.Env["R2_SIMULATOR_URL"] == "" || camera.Simulation != nil {
		t.Fatalf("backend not resolved: %+v", camera)
	}
	camera.Env["KEEP"] = "changed"
	camera.Entitlements[0].Mode = "none"
	if svc.Env["KEEP"] != "yes" || len(svc.Env) != 1 || svc.Entitlements[0].Type != "usb" || svc.Simulation.Entitlements[0].Mode != "host" {
		t.Fatal("source config mutated")
	}
	if _, err := applyRobotSimulationConfig(cfg, "g1"); err == nil {
		t.Fatal("wrong robot accepted")
	}
	physical, err := prepareRobotAppConfig(&grpcclient.AgentConnection{Host: "car.local"}, cfg, nil)
	if err != nil || physical != cfg {
		t.Fatal("physical deployment changed")
	}
}
