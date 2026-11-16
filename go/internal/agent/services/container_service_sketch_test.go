package services

import (
	"context"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

type recordingSketches struct{ forgotten []string }

func (r *recordingSketches) ForgetApp(_ context.Context, appID string) {
	r.forgotten = append(r.forgotten, appID)
}

func TestForgetDroppedSketchOnlyForAnAppWithoutMCU(t *testing.T) {
	r := &recordingSketches{}
	s := &ContainerService{}
	s.SetSketchOwner(r)
	mcu := &appconfig.MCUConfig{Sketch: "sketch"}
	for _, cfg := range []*appconfig.AppConfig{
		nil, // a raw create without a config says nothing about the app
		{},  // nor does an empty one
		{AppID: "has.sketch", MCU: mcu},
		{AppID: "service.has.sketch", ServiceName: "web", MCU: mcu},
		{AppID: "one.service", ServiceName: "web"},
		{AppID: "multi", Services: map[string]*appconfig.ServiceConfig{"web": {}}},
		{AppID: "dropped.it"},
	} {
		s.forgetDroppedSketch(context.Background(), cfg)
	}
	if got := strings.Join(r.forgotten, ","); got != "one.service,multi,dropped.it" {
		t.Fatalf("forgot %q, want every app without a sketch", got)
	}
}
