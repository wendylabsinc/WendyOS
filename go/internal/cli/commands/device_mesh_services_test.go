package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
)

func TestNormalMeshServicesCommandPreservesOwnerAndInheritedLease(t *testing.T) {
	previousRead, previousJSON := readOperatorServices, jsonOutput
	t.Cleanup(func() { readOperatorServices = previousRead; jsonOutput = previousJSON })
	want := sessionbroker.OperatorServices{Version: 1, State: "ready", Source: "trusted-reporter", Services: []sessionbroker.OperatorServiceInfo{{Selector: "mesh://11111111-1111-4111-8111-111111111111/22222222-2222-4222-8222-222222222222", Principal: "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/22222222-2222-4222-8222-222222222222", Mesh: "lab", App: "sh.wendy.sensor", Service: "temperature", Type: "_sensor._tcp", Instance: "Room sensor", Port: 8080, Expires: time.Now().Add(time.Minute).UnixMilli(), Revision: 42}}}
	readOperatorServices = func(ctx context.Context, query sessionbroker.OperatorServiceQuery) (sessionbroker.OperatorServices, error) {
		if query.Mesh != "lab" || query.Type != "_sensor._tcp" {
			t.Fatal("query lost", query)
		}
		return want, nil
	}
	jsonOutput = true
	command := newDeviceMeshServicesCmd()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{"--mesh", "lab", "--type", "_sensor._tcp"})
	if err := command.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	var actual sessionbroker.OperatorServices
	if err := json.Unmarshal(output.Bytes(), &actual); err != nil {
		t.Fatal(err)
	}
	if actual.Services[0].Selector != want.Services[0].Selector || actual.Services[0].Expires != want.Services[0].Expires || actual.Services[0].Revision != 42 {
		t.Fatal("service owner/lease changed", actual)
	}
}
