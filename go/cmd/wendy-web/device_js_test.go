//go:build js && wasm

package main

import (
	"encoding/json"
	"testing"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func TestDeviceOperationsRequireLocalConnection(t *testing.T) {
	for _, method := range []string{"device-rpc", "device-schema", "device-stream-stop", "device-stream-send", "shell-open"} {
		b := &bridge{}
		if _, err := b.handle(request{Method: method, Params: json.RawMessage(`{}`)}); err == nil {
			t.Errorf("%s allowed without local device connection", method)
		}
	}
}
func TestDeviceDescriptorsExcludeCloudAndUnknownMethods(t *testing.T) {
	for _, pair := range [][2]string{{"wendy.cloud.services.v2.AssetService", "ListAssets"}, {"wendy.agent.services.v2.GetWiFiStatusRequest", "GetWiFiStatus"}, {"wendy.agent.services.v2.WendyWiFiService", "NotAnOperation"}} {
		if _, err := deviceMethod(pair[0], pair[1]); err == nil {
			t.Errorf("accepted %v", pair)
		}
	}
	method, err := deviceMethod("wendy.agent.services.v2.WendyWiFiService", "ConnectToWiFi")
	if err != nil {
		t.Fatal(err)
	}
	fields := deviceFields(method.Input(), 0)
	if len(fields) != 4 || fields[0]["name"] != "ssid" || fields[2]["kind"] != "enum" {
		t.Fatalf("wrong Wi-Fi form: %v", fields)
	}
}
func TestDeviceResponsePreservesFailureAndOptionalFalse(t *testing.T) {
	var result map[string]any
	if err := json.Unmarshal(deviceJSON(&pb.ConnectToWiFiResponse{Success: false}), &result); err != nil {
		t.Fatal(err)
	}
	if value, exists := result["success"]; !exists || value != false {
		t.Fatalf("failed operation could look successful: %v", result)
	}
	method, err := deviceMethod("wendy.agent.services.v2.WendyLocalMeshAdminService", "ConfigureLocalMesh")
	if err != nil {
		t.Fatal(err)
	}
	fields := deviceFields(method.Input(), 0)
	for _, field := range fields {
		if field["kind"] != "bool" {
			t.Fatalf("unexpected mesh field: %v", field)
		}
	}
}
